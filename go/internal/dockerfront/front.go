// Package dockerfront answers the Docker Engine API on a socket of its own, and sends the
// image pulls that arrive there through a cache.
//
// pkgcache-docker stands in for the docker command, which covers every program that runs
// one. It cannot cover the programs that talk to the daemon directly — a CI runner's
// Docker executor, compose, anything built on an SDK — because they never run a command
// there is anything to stand in for. What they can all be given is a different socket, so
// that is where this sits: between them and the daemon, forwarding everything untouched
// except POST /images/create. A pull is rewritten with MapImage, the rule a FROM line
// gets, so local/app:1.0 and python:3.12 come from the cache, and the image is then
// tagged with the name that was asked for, which is the name the client looks for next.
//
// It knows one endpoint and nothing else of the API, and an API it does not interpret is
// one it cannot break. That holds only if two things about the transport are right:
// streaming responses are flushed as they arrive, and a connection the client upgrades —
// attach, exec, BuildKit's session — becomes a byte tunnel that ends each direction on
// its own. A runner writes a job's script to the container's stdin, closes that half, and
// reads the output until the container exits; a tunnel that closed both halves at the
// first end it saw would finish every job before it started.
package dockerfront

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/dockerfile"
)

// daemonHost is the authority requests to the daemon carry. Dial decides where they go,
// so it only has to be a valid host name.
const daemonHost = "docker"

var (
	// createRE matches the pull endpoint, with or without the API version a client pins.
	createRE = regexp.MustCompile(`^(/v\d[\d.]*)?/images/create$`)
	// hijackRE matches the endpoints the daemon answers by taking the connection over,
	// which it does whether or not the client asked for an upgrade.
	hijackRE = regexp.MustCompile(`^(/v\d[\d.]*)?/(containers/[^/]+/attach|exec/[^/]+/start)$`)
)

// Options configures a Front.
type Options struct {
	// Registry is the cache's authority as the daemon dials it: 127.0.0.1:41999 for a
	// pkgreg-bridge on this machine. Loopback is what lets the daemon pull over plain
	// HTTP with no CA installed for it.
	Registry string
	// Project scopes every pull, riding the image name. Empty is the global project.
	Project string
	// Dial connects to the real daemon.
	Dial func(ctx context.Context) (net.Conn, error)
	// Notes receives a line for each pull sent through the cache. Nil discards them.
	Notes io.Writer
}

// Front forwards a Docker client's requests to the daemon, pulling images through a cache.
type Front struct {
	registry string
	project  string
	dial     func(ctx context.Context) (net.Conn, error)
	notes    io.Writer
	client   *http.Client
	proxy    *httputil.ReverseProxy
}

// New returns a Front for o.
func New(o Options) *Front {
	f := &Front{registry: o.Registry, project: o.Project, dial: o.Dial, notes: o.Notes}
	if f.notes == nil {
		f.notes = io.Discard
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return f.dial(ctx)
		},
		// Progress, logs and events are streams; compressing them would only buffer them.
		DisableCompression:  true,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
	// No timeout on either: a pull of a large image and a wait on a long job are both
	// answers that take as long as they take.
	f.client = &http.Client{Transport: transport}
	f.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = daemonHost
			r.Out.Host = r.In.Host
		},
		Transport: transport,
		// Every write flushed: events, logs --follow and wait answer over minutes, and an
		// answer held in a buffer is a client that hangs.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, http.StatusBadGateway, "the Docker daemon: "+err.Error())
		},
	}
	return f
}

// ServeHTTP forwards one request, rewriting it only if it is a pull the cache can answer.
func (f *Front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Header.Get("Upgrade") != "" || hijackRE.MatchString(r.URL.Path):
		f.tunnel(w, r)
	case r.Method == http.MethodPost && createRE.MatchString(r.URL.Path):
		f.create(w, r)
	default:
		f.proxy.ServeHTTP(w, r)
	}
}

// pullPlan is one pull sent through the cache.
type pullPlan struct {
	name    string // the repository as asked for, e.g. local/app
	tag     string // its tag, or its digest
	digest  bool
	asked   string // name and tag together, as somebody would write them
	via     string // the same image at the cache
	viaName string // via without its tag or digest
}

// failed explains a failure in terms of the image that was asked for. The daemon's own
// message names the cache's address, because that is where the pull went, and read alone
// it sends whoever reads it to investigate an image nobody asked for.
func (p pullPlan) failed(message string) string {
	return fmt.Sprintf("pull %s through %s: %s", p.asked, p.via, message)
}

// plan reads a pull from its query the way the daemon does, and maps it to the cache. It
// reports false for what the cache should not answer: an import, a pull of every tag, and
// an image from a registry MapImage leaves alone.
func (f *Front) plan(query url.Values) (pullPlan, bool) {
	name, tag := query.Get("fromImage"), query.Get("tag")
	if name == "" || query.Get("fromSrc") != "" {
		return pullPlan{}, false
	}
	// The daemon reads a tag inside fromImage only when the tag parameter is empty.
	name, own := dockerfile.SplitTag(name)
	if tag == "" && own != "" {
		tag = own[1:]
	}
	if tag == "" {
		// Every tag of the repository: docker pull -a. Doing that through the cache would
		// mean listing tags it has no reason to know, so the daemon answers it as asked.
		return pullPlan{}, false
	}
	p := pullPlan{name: name, tag: tag, digest: strings.Contains(tag, ":")}
	separator := ":"
	if p.digest {
		separator = "@"
	}
	p.asked = name + separator + tag
	p.via = dockerfile.MapImage(p.asked, f.registry, f.project)
	if p.via == "" {
		return pullPlan{}, false
	}
	p.viaName = strings.TrimSuffix(p.via, separator+tag)
	return p, true
}

// create answers POST /images/create: a pull through the cache, or anything else as is.
func (f *Front) create(w http.ResponseWriter, r *http.Request) {
	p, ok := f.plan(r.URL.Query())
	if !ok {
		f.proxy.ServeHTTP(w, r)
		return
	}
	version := createRE.FindStringSubmatch(r.URL.Path)[1]
	_, _ = fmt.Fprintf(f.notes, "pkgcache-docker: %s -> %s\n", p.asked, p.via)

	// Credentials for the image's own registry are no business of the cache's: the daemon
	// sends X-Registry-Auth to whatever it pulls from, and that is now the cache.
	header := r.Header.Clone()
	header.Del("X-Registry-Auth")
	query := r.URL.Query()
	query.Set("fromImage", p.viaName)
	query.Set("tag", p.tag)
	response, err := f.call(r.Context(), version+"/images/create", query, header)
	if err != nil {
		writeError(w, http.StatusBadGateway, p.failed(err.Error()))
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		writeError(w, response.StatusCode, p.failed(daemonMessage(response)))
		return
	}

	copyHeader(w.Header(), response.Header)
	// Lines are added to the daemon's answer, so a length it gave would be a lie.
	w.Header().Del("Content-Length")
	w.WriteHeader(http.StatusOK)
	out := progress{w: w, flush: http.NewResponseController(w)}
	if !out.relay(response.Body, p.failed) {
		return
	}
	if p.digest {
		if !f.pullPinned(r, version, out, p) {
			return
		}
	} else if err := f.tag(r.Context(), version, p); err != nil {
		out.fail(p.failed("pulled, but naming it " + p.asked + " failed: " + err.Error()))
		return
	}
	out.status(p.asked + " pulled through " + p.via)
}

// tag gives the pulled image the name that was asked for.
//
// The cache's address is left on it as well. Two jobs pulling one image at once would
// otherwise race: one taking that name away while the other is about to tag from it.
func (f *Front) tag(ctx context.Context, version string, p pullPlan) error {
	query := url.Values{"repo": {p.name}, "tag": {p.tag}}
	response, err := f.call(ctx, version+"/images/"+p.via+"/tag", query, nil)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode/100 != 2 {
		return errors.New(daemonMessage(response))
	}
	return nil
}

// pullPinned names an image pulled by digest by pulling it once more from its own
// registry, exactly as asked and with the client's credentials.
//
// Docker refuses a tag that carries a digest, and knows an image as repo@sha256:… only
// once it has been pulled under that name. Every layer is here by now, so this pull is
// the manifest and the config, and the digest makes it the same image the cache served.
func (f *Front) pullPinned(r *http.Request, version string, out progress, p pullPlan) bool {
	pinned := func(message string) string {
		return fmt.Sprintf("%s is here as %s, but Docker names an image by digest only by "+
			"pulling it from its own registry, and that pull failed: %s", p.asked, p.via, message)
	}
	response, err := f.call(r.Context(), version+"/images/create", r.URL.Query(), r.Header)
	if err != nil {
		out.fail(pinned(err.Error()))
		return false
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		out.fail(pinned(daemonMessage(response)))
		return false
	}
	return out.relay(response.Body, pinned)
}

// call sends one bodiless POST to the daemon.
func (f *Front) call(
	ctx context.Context, path string, query url.Values, header http.Header,
) (*http.Response, error) {
	target := "http://" + daemonHost + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, http.NoBody)
	if err != nil {
		return nil, err
	}
	if header != nil {
		request.Header = header.Clone()
	}
	return f.client.Do(request)
}

// tunnel carries an upgraded connection to the daemon byte for byte.
//
// The request goes as the client wrote it, Upgrade header and all, and whatever the daemon
// answers — 101, or 200 and a raw stream when nobody asked for an upgrade — is bytes like
// everything after it. Each direction ends on its own: a client that has sent all of its
// stdin closes its writing half and is still owed every byte of output.
func (f *Front) tunnel(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "this connection cannot be handed over")
		return
	}
	// Not the request's context. The server cancels that the moment it reads the client's
	// end of input, and a client that has sent all of stdin has ended its input on purpose:
	// that is the half-close this tunnel exists to carry, not a client going away.
	dialing, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	daemon, err := f.dial(dialing)
	cancel()
	if err != nil {
		writeError(w, http.StatusBadGateway, "the Docker daemon: "+err.Error())
		return
	}
	// Before the hijack, which leaves the request's body unreadable.
	if err := r.Write(daemon); err != nil {
		_ = daemon.Close()
		writeError(w, http.StatusBadGateway, "the Docker daemon: "+err.Error())
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = daemon.Close()
		return
	}
	// A client that writes stdin without waiting for the answer may already have had some
	// of it read into the server's buffer along with the request.
	var fromClient io.Reader = client
	if n := buffered.Reader.Buffered(); n > 0 {
		head, _ := buffered.Peek(n)
		fromClient = io.MultiReader(bytes.NewReader(head), client)
	}
	done := make(chan struct{}, 2)
	go pipe(daemon, fromClient, done)
	go pipe(client, daemon, done)
	<-done
	<-done
	_ = client.Close()
	_ = daemon.Close()
}

// pipe copies one direction of a tunnel and passes its end on. A clean end closes only the
// writing half, so the other direction carries on; a broken one closes the connection, so
// the other direction stops instead of waiting on a peer that has gone.
func pipe(dst net.Conn, src io.Reader, done chan<- struct{}) {
	_, err := io.Copy(dst, src)
	if half, ok := dst.(interface{ CloseWrite() error }); ok && err == nil {
		_ = half.CloseWrite()
	} else {
		_ = dst.Close()
	}
	done <- struct{}{}
}

// progress relays a pull's newline-delimited JSON to the client, a line at a time.
type progress struct {
	w     io.Writer
	flush *http.ResponseController
}

// relay copies one pull's progress, explaining any error in it, and reports whether the
// pull succeeded. Docker reports a failed pull as a line in a 200 stream, not as a status.
func (o progress) relay(body io.Reader, explain func(string) string) bool {
	reader := bufio.NewReader(body)
	succeeded := true
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if message, failed := pullError(line); failed {
				succeeded = false
				o.fail(explain(message))
			} else {
				o.write(line)
			}
		}
		if errors.Is(err, io.EOF) {
			return succeeded
		}
		if err != nil {
			o.fail(explain("the daemon's answer was cut off: " + err.Error()))
			return false
		}
	}
}

func (o progress) status(message string) {
	line, _ := json.Marshal(map[string]string{"status": message})
	o.write(append(line, '\r', '\n'))
}

func (o progress) fail(message string) {
	line, _ := json.Marshal(map[string]any{
		"errorDetail": map[string]string{"message": message},
		"error":       message,
	})
	o.write(append(line, '\r', '\n'))
}

func (o progress) write(line []byte) {
	_, _ = o.w.Write(line)
	_ = o.flush.Flush()
}

// pullError reports the error a progress line carries, if it carries one.
func pullError(line []byte) (string, bool) {
	var message struct {
		Error       string `json:"error"`
		ErrorDetail *struct {
			Message string `json:"message"`
		} `json:"errorDetail"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &message) != nil {
		return "", false
	}
	if message.ErrorDetail != nil && message.ErrorDetail.Message != "" {
		return message.ErrorDetail.Message, true
	}
	return message.Error, message.Error != ""
}

// daemonMessage reads the message out of an error response from the daemon.
func daemonMessage(response *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Message != "" {
		return body.Message
	}
	if text := strings.TrimSpace(string(raw)); text != "" {
		return text
	}
	return response.Status
}

// writeError answers in the daemon's own error shape, which every client already reads.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

func copyHeader(dst, src http.Header) {
	for name, values := range src {
		dst[name] = append([]string(nil), values...)
	}
}
