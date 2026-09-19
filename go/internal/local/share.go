package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/app"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
	controlapi "github.com/aabdlwahab/PKGCache/internal/control/api"
	"github.com/aabdlwahab/PKGCache/internal/control/auth"
)

// Sharing the console with other machines.
//
// The loopback socket is not touched. Every client setting on this machine names it, and
// when systemd or launchd started this daemon they hold it, so there is nothing here that
// could move it to another interface. Sharing is a second socket, on every interface,
// serving the console behind a password — see app.SharedHandler for what it will and will
// not answer. Turning it off closes that socket and forgets the password, and the cache is
// exactly what it was before.
//
// The choice is a file in the cache directory, like the budget, so it outlives the
// daemon: a console somebody shared is still shared after the idle exit brings the daemon
// back. Deleting the file is the way back when nothing else answers.

// SharePort is where the shared console listens unless somebody chose otherwise.
//
// Next to the loopback port rather than on it. The same port on every interface would
// collide with the loopback socket systemd holds, and a second number is also the one
// thing that makes it obvious, from an address alone, which of the two somebody is using.
const SharePort = config.LocalPort + 1

// ShareCookie is the session cookie on the shared socket. Not the control plane's: that
// one belongs to accounts this program does not have.
const ShareCookie = "pkgcache_share"

// MinSharePassword is the shortest password sharing accepts. The socket is plain HTTP on
// a network, and this is the only thing in front of a control API that can delete the
// cache and read the credentials it holds.
const MinSharePassword = 8

const shareName = "share.json"

// SharePath is where the choice is kept.
func SharePath(dataDir string) string { return filepath.Join(dataDir, shareName) }

// shareRecord is the stored choice: which port, and the password's scrypt digest.
type shareRecord struct {
	Port   int    `json:"port"`
	Salt   string `json:"salt"`
	Digest string `json:"digest"`
}

// readShare returns the stored choice, and whether there is one.
func readShare(dataDir string) (shareRecord, bool, error) {
	data, err := os.ReadFile(SharePath(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return shareRecord{}, false, nil
	}
	if err != nil {
		return shareRecord{}, false, fmt.Errorf("local: read sharing: %w", err)
	}
	var record shareRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return shareRecord{}, true, fmt.Errorf("local: %s is not readable: %w", SharePath(dataDir), err)
	}
	return record, true, nil
}

// SharedPort reports the port a cache directory shares its console on, and whether it
// does. For `pkgcache share` when no daemon is running to ask.
func SharedPort(dataDir string) (port int, shared bool, err error) {
	record, found, err := readShare(dataDir)
	if err != nil || !found {
		return 0, found, err
	}
	return record.Port, true, nil
}

func writeShare(dataDir string, record shareRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dataDir, ".share-*.json")
	if err != nil {
		return fmt.Errorf("local: write sharing: %w", err)
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := temp.Write(append(data, '\n')); err != nil {
		_ = temp.Close()
		return fmt.Errorf("local: write sharing: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("local: write sharing: %w", err)
	}
	// Private for the reason the rest of the directory is: a digest is not the password,
	// but it is what somebody would start guessing against.
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("local: write sharing: %w", err)
	}
	if err := os.Rename(name, SharePath(dataDir)); err != nil {
		return fmt.Errorf("local: write sharing: %w", err)
	}
	return nil
}

// Unshare forgets the choice without a daemon, and reports whether there was one to
// forget. It is what `pkgcache share off` falls back to when nothing is running, so the
// way back never depends on the console that was shared.
func Unshare(dataDir string) (bool, error) {
	err := os.Remove(SharePath(dataDir))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("local: stop sharing: %w", err)
	}
}

// Share runs the shared socket for one daemon.
type Share struct {
	DataDir string
	// Handler is what the socket serves. Set once, before Start; it is built from this
	// Share, which is the gate it asks.
	Handler           http.Handler
	SessionTTL        time.Duration
	ReadHeaderTimeout time.Duration
	Log               *slog.Logger

	// listen binds the socket. Nil means every interface; tests bind loopback, which is
	// all a test machine can be sure of having.
	listen func(port int) (net.Listener, error)

	mu       sync.Mutex
	record   shareRecord
	enabled  bool
	server   *http.Server
	port     int
	problem  string
	sessions *auth.Sessions
}

var (
	_ controlapi.LocalShare = (*Share)(nil)
	_ app.SharedGate        = (*Share)(nil)
)

// Start opens the socket if the cache was shared when the last daemon stopped.
//
// A port somebody else took in the meantime is reported through State rather than
// failing the daemon: this machine's own builds do not use the shared socket, and
// refusing to serve them because another machine cannot reach the console would be the
// wrong way round.
func (s *Share) Start() {
	record, found, err := readShare(s.DataDir)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = auth.NewSessions(s.SessionTTL)
	switch {
	case err != nil:
		s.enabled, s.problem = true, err.Error()
	case !found:
		return
	case record.Digest == "" || record.Salt == "":
		// Nothing could ever sign in, so the socket would be a door with no key. Refusing
		// to open it says so; opening it would look shared and be useless.
		s.enabled, s.problem = true, "the sharing record has no password; "+
			"turn sharing off and on again to set one"
	default:
		s.record, s.enabled = record, true
		if err := s.bindLocked(record.Port); err != nil {
			s.problem = err.Error()
		}
	}
	if s.problem != "" && s.Log != nil {
		s.Log.Warn("the console is shared but not reachable", "reason", s.problem)
	}
}

// Shared reports whether other machines can reach the console right now. The idle
// watcher reads it: a console that vanished fifteen minutes after somebody opened it on
// another machine would be a broken promise, and nothing on that machine can start it
// again. One that could not open its port promises nothing and holds nothing up.
func (s *Share) Shared() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.server != nil
}

// State says whether the console is shared, and where.
func (s *Share) State() controlapi.ShareState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

func (s *Share) stateLocked() controlapi.ShareState {
	state := controlapi.ShareState{Enabled: s.enabled, Port: s.record.Port, URLs: []string{},
		Error: s.problem}
	if state.Port == 0 {
		state.Port = SharePort
	}
	if s.server != nil {
		state.URLs = shareURLs(s.port)
	}
	return state
}

// Enable shares the console, or changes the password of one that is shared.
func (s *Share) Enable(password string, port int) (controlapi.ShareState, error) {
	if len([]rune(password)) < MinSharePassword {
		return controlapi.ShareState{}, control.NewError(http.StatusBadRequest, "weak_password",
			"the password has to be at least %d characters: it is all that stands between "+
				"the network and this cache's controls", MinSharePassword)
	}
	if port < 0 || port > 65535 {
		return controlapi.ShareState{}, control.NewError(http.StatusBadRequest, "invalid_port",
			"%d is not a port", port)
	}
	salt, digest, err := auth.Password{}.Hash(password)
	if err != nil {
		return controlapi.ShareState{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if port == 0 {
		port = s.record.Port
	}
	if port == 0 {
		port = SharePort
	}
	// Bound before anything is written, so a port that is taken leaves the cache exactly
	// as it was rather than recorded as shared on a socket that does not exist.
	previous, previousPort := s.server, s.port
	rebind := previous == nil || previousPort != port
	if rebind {
		if err := s.bindLocked(port); err != nil {
			s.server, s.port = previous, previousPort
			return controlapi.ShareState{}, control.NewError(http.StatusConflict, "port_taken",
				"%s", err.Error())
		}
	}
	record := shareRecord{Port: port, Salt: salt, Digest: digest}
	if err := writeShare(s.DataDir, record); err != nil {
		// Put back what was running: the new socket is the half that did not happen.
		if rebind {
			s.closeLocked(0)
			s.server, s.port = previous, previousPort
		}
		return controlapi.ShareState{}, err
	}
	if rebind && previous != nil {
		closeServer(previous, time.Second)
	}
	s.record, s.enabled, s.problem = record, true, ""
	// Everybody signed in with the old password is signed out: changing it is how
	// somebody takes the console back from a machine they no longer want on it.
	s.sessions = auth.NewSessions(s.SessionTTL)
	if s.Log != nil {
		s.Log.Info("console shared", "port", port)
	}
	return s.stateLocked(), nil
}

// Disable closes the socket and forgets the password.
func (s *Share) Disable() (controlapi.ShareState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := Unshare(s.DataDir); err != nil {
		return controlapi.ShareState{}, err
	}
	// A moment's grace, because the request asking may have arrived on this very socket
	// and deserves its answer. Nothing new is accepted in the meantime, and every session
	// is already void.
	s.closeLocked(time.Second)
	// The port is kept for as long as this daemon runs, so turning sharing back on from
	// the window lands where the other machines were told to look.
	s.record, s.enabled, s.problem = shareRecord{Port: s.record.Port}, false, ""
	s.sessions = auth.NewSessions(s.SessionTTL)
	if s.Log != nil {
		s.Log.Info("console no longer shared")
	}
	return s.stateLocked(), nil
}

// Close stops the socket with the daemon, and forgets nothing.
func (s *Share) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked(0)
}

func (s *Share) bindLocked(port int) error {
	listen := s.listen
	if listen == nil {
		listen = func(port int) (net.Listener, error) {
			// Every interface, both families: the point is that another machine can reach
			// it by whatever address it has for this one.
			return net.Listen("tcp", ":"+strconv.Itoa(port)) //nolint:noctx // binding has nothing to cancel
		}
	}
	listener, err := listen(port)
	if err != nil {
		return fmt.Errorf("cannot listen on port %d for other machines: %w; "+
			"another program may hold it, so choose another port", port, err)
	}
	server := app.NewServer(listener.Addr().String(), s.Handler, s.ReadHeaderTimeout)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) &&
			s.Log != nil {
			s.Log.Warn("the shared console stopped", "error", err)
		}
	}()
	s.server, s.port = server, listener.Addr().(*net.TCPAddr).Port
	return nil
}

func (s *Share) closeLocked(grace time.Duration) {
	if s.server == nil {
		return
	}
	closeServer(s.server, grace)
	s.server, s.port = nil, 0
}

// closeServer stops accepting at once and cuts what is still open after grace. A console
// holds an event stream open for as long as it is on screen, so waiting for connections to
// finish on their own would wait for somebody to close a tab.
func closeServer(server *http.Server, grace time.Duration) {
	if grace <= 0 {
		_ = server.Close()
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		if server.Shutdown(ctx) != nil {
			_ = server.Close()
		}
	}()
}

// shareURLs lists the addresses another machine could type, IPv4 first.
//
// Container and VM bridges are left out: an address on docker0 is this machine's, but no
// other machine on the network reaches it there, and listing it beside the real one makes
// somebody guess.
func shareURLs(port int) []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return []string{}
	}
	var v4, v6 []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || bridgeLike(iface.Name) {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			network, ok := address.(*net.IPNet)
			if !ok || network.IP.IsLoopback() || network.IP.IsLinkLocalUnicast() {
				continue
			}
			url := "http://" + net.JoinHostPort(network.IP.String(), strconv.Itoa(port))
			if network.IP.To4() != nil {
				v4 = append(v4, url)
			} else {
				v6 = append(v6, url)
			}
		}
	}
	sort.Strings(v4)
	sort.Strings(v6)
	return append(append([]string{}, v4...), v6...)
}

func bridgeLike(name string) bool {
	for _, prefix := range []string{"docker", "br-", "veth", "virbr", "vmnet", "vboxnet", "cni", "flannel", "podman"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// Authorized reports whether a request carries a live session on the shared socket.
func (s *Share) Authorized(r *http.Request) bool {
	cookie, err := r.Cookie(ShareCookie)
	if err != nil {
		return false
	}
	s.mu.Lock()
	sessions := s.sessions
	s.mu.Unlock()
	if sessions == nil {
		return false
	}
	_, found := sessions.Resolve(cookie.Value)
	return found
}

// Login checks the password and starts a session.
//
// It takes the console's own sign-in request — a username and a password — so the
// console's sign-in screen works unchanged. The username is not asked for and not read:
// there is one password and nobody to tell apart.
func (s *Share) Login(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	sessions, record := s.sessions, s.record
	s.mu.Unlock()
	ip := remoteIP(r)
	if sessions == nil || record.Digest == "" {
		writeShareError(w, http.StatusServiceUnavailable, "not_shared", "this console is not shared")
		return
	}
	if sessions.Blocked(ip) {
		writeShareError(w, http.StatusTooManyRequests, "login_throttled",
			"too many wrong passwords; try again in a few minutes")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeShareError(w, http.StatusBadRequest, "invalid_json", "send the password as JSON")
		return
	}
	if !(auth.Password{}).Verify(body.Password, record.Salt, record.Digest) {
		sessions.RecordFailure(ip)
		writeShareError(w, http.StatusUnauthorized, "invalid_credentials", "wrong password")
		return
	}
	sessions.ClearFailures(ip)
	token, err := sessions.Create("shared")
	if err != nil {
		writeShareError(w, http.StatusInternalServerError, "internal", "could not start a session")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: ShareCookie, Value: token, Path: "/", HttpOnly: true,
		// Strict: nothing on another site has any business carrying this cookie here,
		// and the console never arrives by a link it would need it on.
		SameSite: http.SameSiteStrictMode, MaxAge: int(s.SessionTTL.Seconds()),
	})
	if s.Log != nil {
		s.Log.Info("signed in to the shared console", "from", ip)
	}
	writeShareJSON(w, http.StatusOK, map[string]any{"shared": true})
}

// Logout ends the caller's session.
func (s *Share) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(ShareCookie); err == nil {
		s.mu.Lock()
		if s.sessions != nil {
			s.sessions.Drop(cookie.Value)
		}
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name: ShareCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	writeShareJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Refuse answers a control request that has no session. password_only is what tells the
// console's sign-in screen not to ask for a username nobody has.
func (s *Share) Refuse(w http.ResponseWriter, _ *http.Request) {
	writeShareJSON(w, http.StatusUnauthorized, map[string]any{
		"error":         "sign in with the password this cache was shared with",
		"code":          "authentication_required",
		"password_only": true,
	})
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeShareError(w http.ResponseWriter, status int, code, message string) {
	writeShareJSON(w, status, map[string]any{"error": message, "code": code})
}

func writeShareJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// The terminal's half: what `pkgcache share` calls on a running daemon.

// ReadSharing asks a running daemon whether its console is shared.
func ReadSharing(ctx context.Context, state State) (controlapi.ShareState, error) {
	var answer controlapi.ShareState
	err := newProjectAPI(state).do(ctx, http.MethodGet, "/api/v1/local/share", nil, &answer)
	return answer, err
}

// StartSharing shares a running daemon's console, or changes its password.
func StartSharing(
	ctx context.Context, state State, password string, port int,
) (controlapi.ShareState, error) {
	var answer controlapi.ShareState
	err := newProjectAPI(state).do(ctx, http.MethodPut, "/api/v1/local/share",
		map[string]any{"password": password, "port": port}, &answer)
	return answer, err
}

// StopSharing closes a running daemon's shared socket and forgets its password.
func StopSharing(ctx context.Context, state State) (controlapi.ShareState, error) {
	var answer controlapi.ShareState
	err := newProjectAPI(state).do(ctx, http.MethodDelete, "/api/v1/local/share", nil, &answer)
	return answer, err
}
