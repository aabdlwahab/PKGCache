package dockerfront

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const cache = "127.0.0.1:41999"

// call is one request the fake daemon received.
type call struct {
	method, path string
	query        url.Values
	auth         string
}

// daemon is a fake Docker daemon: it answers pulls with a progress stream, tags, and the
// two hijacked endpoints, and records what it was asked.
type daemon struct {
	*httptest.Server
	mu    sync.Mutex
	calls []call
	// pull answers one POST /images/create; nil writes a successful stream.
	pull func(w http.ResponseWriter, image string)
}

func newDaemon(t *testing.T) *daemon {
	t.Helper()
	d := &daemon{}
	d.Server = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.Close)
	return d
}

func (d *daemon) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.calls = append(d.calls, call{
		method: r.Method, path: r.URL.Path, query: r.URL.Query(),
		auth: r.Header.Get("X-Registry-Auth"),
	})
	d.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1.43")
	switch {
	case strings.HasSuffix(path, "/attach"), strings.HasSuffix(path, "/start"):
		d.hijack(w, r)
	case path == "/images/create":
		image := r.URL.Query().Get("fromImage") + ":" + r.URL.Query().Get("tag")
		if d.pull != nil {
			d.pull(w, image)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"status\":\"Pulling from %s\"}\r\n", r.URL.Query().Get("fromImage"))
		fmt.Fprintf(w, "{\"status\":\"Status: Downloaded newer image for %s\"}\r\n", image)
	case strings.HasSuffix(path, "/tag"):
		w.WriteHeader(http.StatusCreated)
	default:
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"answered":%q}`, r.Method+" "+r.URL.RequestURI())
	}
}

// hijack answers attach and exec the way the daemon does: it takes the connection over,
// reads stdin until the client closes its writing half, and only then writes the output.
func (d *daemon) hijack(w http.ResponseWriter, r *http.Request) {
	// exec start's options are a JSON body, read before the connection is taken over.
	_, _ = io.ReadAll(r.Body)
	conn, buffered, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	if r.Header.Get("Upgrade") != "" {
		fmt.Fprint(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\n"+
			"Connection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	} else {
		fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n")
	}
	stdin, _ := io.ReadAll(buffered)
	fmt.Fprintf(conn, "got: %s", stdin)
}

func (d *daemon) recorded() []call {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]call(nil), d.calls...)
}

// front serves a Front, in project ci, in front of d.
func front(t *testing.T, d *daemon) *httptest.Server {
	t.Helper()
	address := d.Listener.Addr().String()
	f := New(Options{
		Registry: cache,
		Project:  "ci",
		Dial: func(ctx context.Context) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", address)
		},
	})
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return server
}

func post(t *testing.T, server *httptest.Server, path string) (int, string) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, server.URL+path, http.NoBody)
	request.Header.Set("X-Registry-Auth", "credentials-for-the-image's-own-registry")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

// An image pushed to the cache is named local/… in a job, and Docker would read that as
// a Docker Hub user. The pull has to go to the cache, inside the front's project, and the
// image has to end up under the name the client asked for, because that is the name it
// inspects and runs next.
func TestPullOfAPushedImageGoesThroughTheCacheAndKeepsItsName(t *testing.T) {
	d := newDaemon(t)
	status, body := post(t, front(t, d), "/v1.43/images/create?fromImage=local%2Fapp&tag=1.0")

	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	calls := d.recorded()
	if len(calls) != 2 {
		t.Fatalf("the daemon was asked %d things, want a pull and a tag: %+v", len(calls), calls)
	}
	pull, tag := calls[0], calls[1]
	if got := pull.query.Get("fromImage"); got != cache+"/ci/local/app" {
		t.Errorf("pulled %q, want the cache's ci project", got)
	}
	if pull.query.Get("tag") != "1.0" {
		t.Errorf("pulled tag %q, want 1.0", pull.query.Get("tag"))
	}
	if pull.auth != "" {
		t.Errorf("the image registry's credentials went to the cache: %q", pull.auth)
	}
	if tag.path != "/v1.43/images/"+cache+"/ci/local/app:1.0/tag" ||
		tag.query.Get("repo") != "local/app" || tag.query.Get("tag") != "1.0" {
		t.Errorf("tagged with %s %v, want the cache's image named local/app:1.0", tag.path, tag.query)
	}
	if !strings.Contains(body, "Downloaded newer image") ||
		!strings.Contains(body, "local/app:1.0 pulled through "+cache+"/ci/local/app:1.0") {
		t.Errorf("the client was not shown the pull and its outcome:\n%s", body)
	}
}

// A job's image: is most often an official Docker Hub image, which is the case a cache in
// front of a runner exists for. It has to arrive as the cache's Docker Hub path, library/
// included, exactly as a FROM line naming it would.
func TestOfficialImageIsFetchedFromTheCachesDockerHub(t *testing.T) {
	d := newDaemon(t)
	post(t, front(t, d), "/images/create?fromImage=python&tag=3.12")

	calls := d.recorded()
	if len(calls) == 0 || calls[0].query.Get("fromImage") != cache+"/ci/dockerhub/library/python" {
		t.Fatalf("pulled %+v, want the cache's Docker Hub library/python", calls)
	}
	if calls[len(calls)-1].path != "/images/"+cache+"/ci/dockerhub/library/python:3.12/tag" {
		t.Errorf("an unversioned request must stay unversioned: %+v", calls[len(calls)-1])
	}
}

// Newer clients, a runner's executor among them, send the name Docker normalized:
// docker.io/local/app for local/app. Read as written, that is Docker Hub's user "local",
// and a pushed image is looked for where it never was. Each normalized name has to land
// where its short form does.
func TestNormalizedNamesGoWhereTheirShortFormsDo(t *testing.T) {
	for sent, want := range map[string]string{
		"docker.io/local/app":      cache + "/ci/local/app",
		"docker.io/library/python": cache + "/ci/dockerhub/library/python",
		"docker.io/nvidia/cuda":    cache + "/ci/dockerhub/nvidia/cuda",
	} {
		t.Run(sent, func(t *testing.T) {
			d := newDaemon(t)
			post(t, front(t, d), "/images/create?fromImage="+url.QueryEscape(sent)+"&tag=1.0")

			calls := d.recorded()
			if len(calls) != 2 || calls[0].query.Get("fromImage") != want {
				t.Fatalf("pulled %+v, want %s", calls, want)
			}
			if calls[1].path != "/images/"+want+":1.0/tag" {
				t.Errorf("tagged %s", calls[1].path)
			}
		})
	}
}

// The daemon takes a tag inside fromImage when the tag parameter is empty, and so does the
// front; otherwise python:3.12 sent that way would read as a pull of every tag.
func TestTagInsideFromImageIsRead(t *testing.T) {
	d := newDaemon(t)
	post(t, front(t, d), "/images/create?fromImage=local%2Fapp%3A2.0")

	calls := d.recorded()
	if len(calls) == 0 || calls[0].query.Get("fromImage") != cache+"/ci/local/app" ||
		calls[0].query.Get("tag") != "2.0" {
		t.Fatalf("pulled %+v, want the cache's local/app tag 2.0", calls)
	}
}

// What the cache should not answer reaches the daemon exactly as sent, credentials and
// all: a pull of every tag, an import, and a registry only this machine can route to.
func TestPullsTheCacheShouldNotAnswerReachTheDaemonAsSent(t *testing.T) {
	for _, path := range []string{
		"/images/create?fromImage=python",                         // docker pull -a
		"/images/create?fromSrc=-&repo=imported&tag=1",            // docker import
		"/images/create?fromImage=localhost%3A5000%2Fapp&tag=1.0", // a registry here
		"/images/create?fromImage=10.0.0.5%3A5000%2Fapp&tag=1.0",  // or on this network
	} {
		t.Run(path, func(t *testing.T) {
			d := newDaemon(t)
			post(t, front(t, d), path)

			calls := d.recorded()
			if len(calls) != 1 {
				t.Fatalf("the daemon was asked %d things, want just this one: %+v", len(calls), calls)
			}
			want, _ := url.Parse(path)
			if calls[0].query.Encode() != want.Query().Encode() {
				t.Errorf("the daemon got %v, want %v", calls[0].query, want.Query())
			}
			if calls[0].auth == "" {
				t.Errorf("the client's credentials were dropped from a pull the cache did not answer")
			}
		})
	}
}

// Docker refuses a tag that carries a digest, so an image pinned by one is named the only
// way Docker allows: one more pull, from its own registry, as asked and with the client's
// credentials. Its layers are here by then, so that pull is the manifest and the config.
func TestDigestPinIsNamedByPullingItFromItsOwnRegistry(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	d := newDaemon(t)
	status, body := post(t, front(t, d), "/images/create?fromImage=python&tag="+digest)

	calls := d.recorded()
	if status != http.StatusOK || len(calls) != 2 {
		t.Fatalf("status %d, calls %+v:\n%s", status, calls, body)
	}
	if calls[0].query.Get("fromImage") != cache+"/ci/dockerhub/library/python" ||
		calls[0].query.Get("tag") != digest || calls[0].auth != "" {
		t.Errorf("the first pull was %+v, want the cache, by digest, without credentials", calls[0])
	}
	if calls[1].query.Get("fromImage") != "python" || calls[1].query.Get("tag") != digest ||
		calls[1].auth == "" {
		t.Errorf("the second pull was %+v, want python by digest, as the client asked", calls[1])
	}
}

// A pull Docker could not complete arrives as a line in a 200 stream, naming the cache's
// address. Shown as it is, it reads as a broken cache and an image nobody asked for; it is
// rewritten to name both, and nothing is tagged.
func TestFailedPullNamesTheImageAskedFor(t *testing.T) {
	d := newDaemon(t)
	d.pull = func(w http.ResponseWriter, image string) {
		fmt.Fprintf(w, "{\"status\":\"Pulling from %s\"}\r\n", image)
		fmt.Fprint(w, "{\"errorDetail\":{\"message\":\"manifest unknown\"},\"error\":\"manifest unknown\"}\r\n")
	}
	_, body := post(t, front(t, d), "/images/create?fromImage=local%2Fmissing&tag=1.0")

	want := "pull local/missing:1.0 through " + cache + "/ci/local/missing:1.0: manifest unknown"
	if !strings.Contains(body, want) {
		t.Errorf("the error did not name what was asked for:\n%s", body)
	}
	if calls := d.recorded(); len(calls) != 1 {
		t.Errorf("a failed pull was tagged: %+v", calls)
	}
}

// The same, for a daemon that refuses the pull outright with an error status.
func TestRefusedPullKeepsItsStatusAndNamesTheImageAskedFor(t *testing.T) {
	d := newDaemon(t)
	d.pull = func(w http.ResponseWriter, _ string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"repository does not exist"}`)
	}
	status, body := post(t, front(t, d), "/images/create?fromImage=local%2Fmissing&tag=1.0")

	if status != http.StatusNotFound {
		t.Errorf("status %d, want the daemon's 404", status)
	}
	if !strings.Contains(body, "pull local/missing:1.0 through "+cache) {
		t.Errorf("the error did not name what was asked for: %s", body)
	}
}

// Everything that is not a pull is the daemon's business, and reaches it as sent.
func TestOtherRequestsReachTheDaemonAsSent(t *testing.T) {
	d := newDaemon(t)
	response, err := http.Get(front(t, d).URL + "/v1.43/containers/json?all=1")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)

	if string(body) != `{"answered":"GET /v1.43/containers/json?all=1"}` {
		t.Errorf("the daemon answered %s", body)
	}
}

// Events, logs --follow and wait are answers that arrive over minutes. Held in a buffer
// until there is enough of them, they are a client that hangs.
func TestStreamingAnswersArriveAsTheyAreWritten(t *testing.T) {
	second := make(chan struct{})
	streaming := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "first\n")
		w.(http.Flusher).Flush()
		<-second
		fmt.Fprint(w, "second\n")
	}))
	defer streaming.Close()
	defer close(second)
	address := streaming.Listener.Addr().String()
	server := httptest.NewServer(New(Options{
		Registry: cache,
		Dial: func(ctx context.Context) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", address)
		},
	}))
	defer server.Close()

	response, err := http.Get(server.URL + "/v1.43/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	line := make(chan string, 1)
	go func() {
		text, _ := bufio.NewReader(response.Body).ReadString('\n')
		line <- text
	}()
	select {
	case text := <-line:
		if text != "first\n" {
			t.Errorf("read %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first line was held back until the answer was complete")
	}
}

// A runner writes a job's script to the container's stdin, closes its writing half, and
// reads the output until the container exits. The tunnel has to pass that half-close on
// and keep the other direction open; closing both at the first end would lose the output
// of every job. The daemon here writes nothing until stdin has ended, so it fails unless
// both are right. The script is sent before the daemon answers, as a hurried client does,
// so some of it arrives in the same read as the request.
func TestUpgradedConnectionEndsEachDirectionOnItsOwn(t *testing.T) {
	for name, request := range map[string]string{
		"attach with an upgrade": "POST /v1.43/containers/job/attach?stdin=1&stream=1 HTTP/1.1\r\n" +
			"Host: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n",
		"exec start without one": "POST /v1.43/exec/job/start HTTP/1.1\r\n" +
			"Host: docker\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}",
	} {
		t.Run(name, func(t *testing.T) {
			server := front(t, newDaemon(t))
			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

			fmt.Fprint(conn, request+"echo the job's script")
			if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			answer, err := io.ReadAll(conn)
			if err != nil {
				t.Fatalf("read: %v (got %q)", err, answer)
			}
			if !strings.HasSuffix(string(answer), "got: echo the job's script") {
				t.Errorf("the daemon's output did not come back after stdin ended:\n%q", answer)
			}
		})
	}
}
