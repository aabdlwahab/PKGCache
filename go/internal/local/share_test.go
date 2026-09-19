//go:build !windows

package local

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/app"
)

const sharePassword = "correct horse battery"

// newShare wires the shared socket the way the daemon does, over a real app, and serves
// the loopback control plane beside it. The shared socket binds loopback here, which is
// all a test machine is sure to have; what it answers is the same on every interface.
func newShare(t *testing.T) (*Share, *httptest.Server, *app.App) {
	t.Helper()
	snap := testSnapshot(t, 0)
	instance, err := app.Open(snap)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	share := shareFor(instance, snap.DataDir)
	share.Start()
	t.Cleanup(share.Close)
	loopback := httptest.NewServer(instance.API)
	t.Cleanup(loopback.Close)
	return share, loopback, instance
}

func shareFor(instance *app.App, dataDir string) *Share {
	share := &Share{
		DataDir: dataDir, SessionTTL: time.Hour, ReadHeaderTimeout: 5 * time.Second,
		listen: func(port int) (net.Listener, error) {
			return net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		},
	}
	share.Handler = instance.SharedHandler(share)
	instance.API.Share = share
	return share
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

// call sends one JSON request and decodes a JSON answer into out, when there is one.
func call(t *testing.T, client *http.Client, method, target, body string, out any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	if out != nil && strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: %v in %s", method, target, err, data)
		}
	}
	return response
}

type shareAnswer struct {
	Enabled bool     `json:"enabled"`
	Port    int      `json:"port"`
	URLs    []string `json:"urls"`
	Error   string   `json:"error"`
	Code    string   `json:"code"`
}

func enable(t *testing.T, loopback *httptest.Server, password string, port int) (int, shareAnswer) {
	t.Helper()
	var answer shareAnswer
	body := `{"password":` + strconv.Quote(password) + `,"port":` + strconv.Itoa(port) + `}`
	response := call(t, loopback.Client(), http.MethodPut,
		loopback.URL+"/api/v1/local/share", body, &answer)
	return response.StatusCode, answer
}

// browser is a client with a cookie jar, as a browser on the other machine would be.
func browser() *http.Client {
	jar := &memoryJar{}
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

// memoryJar keeps every cookie for every host. net/http/cookiejar would do, but refuses
// nothing this test needs refused and pulls in a public-suffix question it does not ask.
type memoryJar struct{ cookies []*http.Cookie }

func (j *memoryJar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	for _, cookie := range cookies {
		kept := j.cookies[:0]
		for _, existing := range j.cookies {
			if existing.Name != cookie.Name {
				kept = append(kept, existing)
			}
		}
		j.cookies = kept
		if cookie.MaxAge >= 0 && cookie.Value != "" {
			j.cookies = append(j.cookies, cookie)
		}
	}
}

func (j *memoryJar) Cookies(*url.URL) []*http.Cookie { return j.cookies }

func signIn(t *testing.T, client *http.Client, base, password string) int {
	t.Helper()
	return call(t, client, http.MethodPost, base+"/api/v1/login",
		`{"username":"","password":`+strconv.Quote(password)+`}`, nil).StatusCode
}

func TestConsoleIsNotSharedUntilSomebodySharesIt(t *testing.T) {
	share, loopback, _ := newShare(t)
	var answer shareAnswer
	if status := call(t, loopback.Client(), http.MethodGet,
		loopback.URL+"/api/v1/local/share", "", &answer).StatusCode; status != http.StatusOK {
		t.Fatalf("GET share: %d", status)
	}
	if answer.Enabled || len(answer.URLs) != 0 || answer.Port != SharePort {
		t.Fatalf("a fresh cache reads as %+v", answer)
	}
	if share.Shared() {
		t.Fatal("a fresh cache holds its daemon up for a console nobody shared")
	}
}

// The whole point: the shared socket's controls are behind the password, and everything
// that is not the console is refused whether or not somebody has it.
func TestSharedConsoleAsksForThePasswordAndServesNothingElse(t *testing.T) {
	share, loopback, _ := newShare(t)
	port := freePort(t)
	if status, answer := enable(t, loopback, sharePassword, port); status != http.StatusOK ||
		!answer.Enabled || answer.Port != port {
		t.Fatalf("share: %d %+v", status, answer)
	}
	if !share.Shared() {
		t.Fatal("a shared console does not hold the daemon up")
	}
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := browser()

	if status := call(t, client, http.MethodGet, base+"/healthz", "", nil).StatusCode; status != http.StatusOK {
		t.Errorf("healthz: %d", status)
	}
	// The console's files load without a session: its sign-in screen is one of them.
	if status := call(t, client, http.MethodGet, base+"/console", "", nil).StatusCode; status != http.StatusOK {
		t.Errorf("console page: %d", status)
	}
	var refusal map[string]any
	if status := call(t, client, http.MethodGet, base+"/api/v1/projects", "", &refusal).StatusCode; status != http.StatusUnauthorized ||
		refusal["password_only"] != true {
		t.Fatalf("projects without a session: %d %v", status, refusal)
	}
	for _, path := range []string{"/metrics", "/global/pypi/simple/six/"} {
		if status := call(t, client, http.MethodGet, base+path, "", nil).StatusCode; status != http.StatusNotFound {
			t.Errorf("%s without a session: %d", path, status)
		}
	}
	if status := signIn(t, client, base, "not the password"); status != http.StatusUnauthorized {
		t.Fatalf("a wrong password: %d", status)
	}
	if status := signIn(t, client, base, sharePassword); status != http.StatusOK {
		t.Fatalf("the right password: %d", status)
	}

	if status := call(t, client, http.MethodGet, base+"/api/v1/projects", "", nil).StatusCode; status != http.StatusOK {
		t.Fatalf("projects with a session: %d", status)
	}
	var me map[string]any
	call(t, client, http.MethodGet, base+"/api/v1/me", "", &me)
	if me["shared"] != true {
		t.Errorf("me on the shared socket: %v", me)
	}
	// Packages stay this machine's, password or not.
	if status := call(t, client, http.MethodGet, base+"/global/pypi/simple/six/", "", nil).StatusCode; status != http.StatusNotFound {
		t.Errorf("a package route with a session: %d", status)
	}
	// And so does the apt proxy, which would otherwise relay the network to wherever this
	// machine can reach.
	proxied := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) { return url.Parse(base) },
	}}
	response, err := proxied.Get("http://deb.debian.org/debian/dists/stable/Release")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("a forward-proxy request: %d", response.StatusCode)
	}

	if status := call(t, client, http.MethodPost, base+"/api/v1/logout", "", nil).StatusCode; status != http.StatusOK {
		t.Fatalf("logout: %d", status)
	}
	if status := call(t, client, http.MethodGet, base+"/api/v1/projects", "", nil).StatusCode; status != http.StatusUnauthorized {
		t.Errorf("projects after signing out: %d", status)
	}
}

// Loopback is the machine's own, and sharing changes nothing about it.
func TestLoopbackIsUntouchedBySharing(t *testing.T) {
	_, loopback, _ := newShare(t)
	enable(t, loopback, sharePassword, freePort(t))
	if status := call(t, loopback.Client(), http.MethodGet,
		loopback.URL+"/api/v1/projects", "", nil).StatusCode; status != http.StatusOK {
		t.Fatalf("loopback projects while shared: %d", status)
	}
}

func TestChangingThePasswordSignsEverybodyOut(t *testing.T) {
	_, loopback, _ := newShare(t)
	port := freePort(t)
	enable(t, loopback, sharePassword, port)
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := browser()
	if status := signIn(t, client, base, sharePassword); status != http.StatusOK {
		t.Fatalf("sign in: %d", status)
	}
	if status, _ := enable(t, loopback, "a different one entirely", 0); status != http.StatusOK {
		t.Fatalf("change the password: %d", status)
	}
	if status := call(t, client, http.MethodGet, base+"/api/v1/projects", "", nil).StatusCode; status != http.StatusUnauthorized {
		t.Errorf("an old session after the password changed: %d", status)
	}
	if status := signIn(t, client, base, sharePassword); status != http.StatusUnauthorized {
		t.Errorf("the old password: %d", status)
	}
	if status := signIn(t, client, base, "a different one entirely"); status != http.StatusOK {
		t.Errorf("the new password: %d", status)
	}
}

func TestStoppingClosesTheSocketAndForgetsThePassword(t *testing.T) {
	share, loopback, _ := newShare(t)
	port := freePort(t)
	enable(t, loopback, sharePassword, port)
	var answer shareAnswer
	if status := call(t, loopback.Client(), http.MethodDelete,
		loopback.URL+"/api/v1/local/share", "", &answer).StatusCode; status != http.StatusOK || answer.Enabled {
		t.Fatalf("stop sharing: %d %+v", status, answer)
	}
	if _, err := os.Stat(SharePath(share.DataDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the password is still on disk: %v", err)
	}
	if share.Shared() {
		t.Error("an unshared console still holds the daemon up")
	}
	if answer.Port != port {
		t.Errorf("stopping forgot the port chosen: %d, want %d", answer.Port, port)
	}
	address := "127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("the shared socket is still accepting connections")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A console somebody shared is still shared when the idle exit brings the daemon back.
func TestSharingOutlivesTheDaemon(t *testing.T) {
	share, loopback, instance := newShare(t)
	port := freePort(t)
	enable(t, loopback, sharePassword, port)
	share.Close()

	again := shareFor(instance, share.DataDir)
	again.Start()
	t.Cleanup(again.Close)
	if state := again.State(); !state.Enabled || state.Port != port || state.Error != "" {
		t.Fatalf("after a restart: %+v", state)
	}
	if status := signIn(t, browser(), "http://127.0.0.1:"+strconv.Itoa(port), sharePassword); status != http.StatusOK {
		t.Fatalf("the password after a restart: %d", status)
	}
}

func TestARefusedShareChangesNothing(t *testing.T) {
	share, loopback, _ := newShare(t)
	if status, answer := enable(t, loopback, "short", freePort(t)); status != http.StatusBadRequest ||
		answer.Code != "weak_password" {
		t.Errorf("a short password: %d %+v", status, answer)
	}
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	taken := held.Addr().(*net.TCPAddr).Port
	if status, answer := enable(t, loopback, sharePassword, taken); status != http.StatusConflict ||
		answer.Code != "port_taken" {
		t.Errorf("a taken port: %d %+v", status, answer)
	}
	if _, err := os.Stat(SharePath(share.DataDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused share was recorded: %v", err)
	}
	if share.State().Enabled {
		t.Error("a refused share reads as shared")
	}
}

// Somebody guessing gets a handful of tries, not a dictionary.
func TestGuessingIsThrottled(t *testing.T) {
	_, loopback, _ := newShare(t)
	port := freePort(t)
	enable(t, loopback, sharePassword, port)
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := browser()
	for range 5 {
		signIn(t, client, base, "guess")
	}
	if status := signIn(t, client, base, sharePassword); status != http.StatusTooManyRequests {
		t.Fatalf("the right password after five guesses: %d", status)
	}
}

// A shared console must not vanish with the idle exit: nothing on the machines it was
// shared with can start the daemon again.
func TestSharingHoldsOffTheIdleExit(t *testing.T) {
	snap := testSnapshot(t, 0)
	instance, err := app.Open(snap)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	var last atomic.Int64
	last.Store(time.Now().Add(-time.Hour).UnixNano())
	var held atomic.Bool
	held.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idle := watchIdle(ctx, instance, &last, 10*time.Millisecond, held.Load)
	select {
	case <-idle:
		t.Fatal("the daemon went idle with its console shared")
	case <-time.After(2500 * time.Millisecond):
	}
	held.Store(false)
	select {
	case <-idle:
	case <-time.After(3 * time.Second):
		t.Fatal("the daemon never went idle once sharing stopped")
	}
}
