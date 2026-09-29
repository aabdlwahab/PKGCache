package upstream

import (
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/obs"
)

func chainPool(t *testing.T) *Pool {
	t.Helper()
	return mustPool(t, config.Upstream{
		RequestTimeout:        10 * time.Second,
		ConnectTimeout:        2 * time.Second,
		ResponseHeaderTimeout: 2 * time.Second,
		MaxIdlePerHost:        4,
		UserAgent:             "pkgreg-test/1",
	})
}

func mustPool(t *testing.T, cfg config.Upstream) *Pool {
	t.Helper()
	pool, err := New(cfg, obs.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

// answering serves a fixed status and records that it was asked.
func answering(t *testing.T, status int, body string) (url string, hits *int) {
	t.Helper()
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count++
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL, &count
}

// unreachable returns a URL nothing is listening on — the case the whole chain exists
// for. A server that is started and immediately closed leaves a port that refuses.
func unreachable(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()
	return url
}

func open(t *testing.T, pool *Pool, request Request) (*http.Response, error) {
	t.Helper()
	ctx, cancelCtx := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancelCtx)
	response, cancel, err := pool.Open(ctx, request)
	if cancel != nil {
		t.Cleanup(cancel)
	}
	return response, err
}

// What counts as "cannot be reached" is the whole design of the chain, and being wrong
// in either direction is worse than having no fallbacks at all. This is that table.
func TestChainFallsThroughOnlyWhenTheOriginCannotServe(t *testing.T) {
	cases := []struct {
		name       string
		firstReply func(t *testing.T) (url string, hits *int)
		wantSecond bool
		wantStatus int
	}{
		{
			name: "connection refused",
			firstReply: func(t *testing.T) (string, *int) {
				zero := 0
				return unreachable(t), &zero
			},
			wantSecond: true, wantStatus: http.StatusOK,
		},
		{
			name: "server error",
			firstReply: func(t *testing.T) (string, *int) {
				return answering(t, http.StatusBadGateway, "")
			},
			wantSecond: true, wantStatus: http.StatusOK,
		},
		{
			// A cache in deliberate offline mode answers exactly this. Falling through
			// would quietly reach the internet somebody switched off.
			name: "not found",
			firstReply: func(t *testing.T) (string, *int) {
				return answering(t, http.StatusNotFound, "")
			},
			wantSecond: false, wantStatus: http.StatusNotFound,
		},
		{
			// A misconfigured credential. Going around it hides a problem that will
			// otherwise be found once rather than never.
			name: "forbidden",
			firstReply: func(t *testing.T) (string, *int) {
				return answering(t, http.StatusForbidden, "")
			},
			wantSecond: false, wantStatus: http.StatusForbidden,
		},
		{
			name: "success",
			firstReply: func(t *testing.T) (string, *int) {
				return answering(t, http.StatusOK, "first")
			},
			wantSecond: false, wantStatus: http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, _ := tc.firstReply(t)
			second, secondHits := answering(t, http.StatusOK, "second")

			response, err := open(t, chainPool(t), Request{
				URL: first, Eco: "pypi",
				Fallbacks: []Fallback{{URL: second}},
			})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = response.Body.Close() }()

			if response.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", response.StatusCode, tc.wantStatus)
			}
			if got := *secondHits > 0; got != tc.wantSecond {
				t.Errorf("fell through to the second origin = %v, want %v", got, tc.wantSecond)
			}
		})
	}
}

// A chain that fails everywhere reports the last origin's error rather than a synthetic
// one, so the message names something a person can go and look at.
func TestChainReportsTheLastFailure(t *testing.T) {
	first := unreachable(t)
	second := unreachable(t)
	_, err := open(t, chainPool(t), Request{
		URL: first, Eco: "npm", Fallbacks: []Fallback{{URL: second}},
	})
	if err == nil {
		t.Fatal("a chain with nothing reachable returned no error")
	}
	if !strings.Contains(err.Error(), second) {
		t.Fatalf("error names %v, want the last origin %s", err, second)
	}
}

// Every fallback is tried, not just the first.
func TestChainWalksTheWholeChain(t *testing.T) {
	third, thirdHits := answering(t, http.StatusOK, "third")
	response, err := open(t, chainPool(t), Request{
		URL: unreachable(t), Eco: "npm",
		Fallbacks: []Fallback{{URL: unreachable(t)}, {URL: third}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if *thirdHits != 1 {
		t.Fatalf("third origin hits = %d, want 1", *thirdHits)
	}
}

// A fallback is a different origin and carries its own credential. Sending the first
// origin's to the second would leak a team token to a public registry.
func TestChainUsesEachOriginsOwnCredential(t *testing.T) {
	var seen string
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		}))
	defer server.Close()

	response, err := open(t, chainPool(t), Request{
		URL: unreachable(t), Eco: "pypi",
		Credential: &Credential{Kind: "basic", Username: "team", Password: "team-secret"},
		Fallbacks: []Fallback{{
			URL:        server.URL,
			Credential: &Credential{Kind: "bearer", Token: "public-token"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if !strings.Contains(seen, "public-token") {
		t.Fatalf("Authorization = %q, want the fallback's own credential", seen)
	}
	if strings.Contains(seen, "team-secret") {
		t.Fatal("the first origin's credential was sent to the fallback")
	}
}

// An origin that accepts a connection and then says nothing must not hold the request
// for the whole request timeout before anything else is tried. That budget exists so a
// 2.5 GB body can finish, not so a stalled index can.
func TestChainFailsOverFromAStalledOrigin(t *testing.T) {
	released := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { <-released }))
	// Deferred in this order on purpose: Close waits for outstanding handlers, and this
	// handler only returns once released is closed. LIFO means the release happens
	// first; the other way round is a deadlock in the test's own teardown.
	defer stalled.Close()
	defer close(released)
	second, secondHits := answering(t, http.StatusOK, "second")

	pool := mustPool(t, config.Upstream{
		RequestTimeout:        10 * time.Minute, // the real one: sized for a huge body
		ConnectTimeout:        2 * time.Second,
		ResponseHeaderTimeout: 300 * time.Millisecond,
		MaxIdlePerHost:        4,
	})

	started := time.Now()
	response, err := open(t, pool, Request{
		URL: stalled.URL, Eco: "pypi", Fallbacks: []Fallback{{URL: second}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if *secondHits != 1 {
		t.Fatalf("second origin hits = %d, want 1", *secondHits)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("failover took %s; it waited on the request timeout", elapsed)
	}
}

// No fallbacks is every configuration that predates chains, and it must behave exactly
// as it did: one attempt, and the origin's own answer whatever it is.
func TestNoFallbacksIsUnchangedBehaviour(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusBadGateway} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			url, hits := answering(t, status, "")
			response, err := open(t, chainPool(t), Request{URL: url, Eco: "npm"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != status {
				t.Errorf("status = %d, want %d", response.StatusCode, status)
			}
			if *hits != 1 {
				t.Errorf("origin hits = %d, want exactly one attempt", *hits)
			}
		})
	}
}

// A pkgreg serves TLS with a certificate it mints itself, so a laptop whose middle tier
// is the team's cache cannot verify it from the system store. This is the setting that
// unblocks that, and its absence is what made the tier impossible before.
func TestPoolTrustsAConfiguredCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()

	caPath := filepath.Join(t.TempDir(), "ca.crt")
	certificate := server.Certificate()
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: certificate.Raw,
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	// Without it, the same request fails to verify.
	plain := chainPool(t)
	if _, err := open(t, plain, Request{URL: server.URL, Eco: "pypi"}); err == nil {
		t.Fatal("a self-signed origin verified against the system roots alone")
	}

	trusting := mustPool(t, config.Upstream{
		RequestTimeout: 10 * time.Second, ConnectTimeout: 2 * time.Second,
		ResponseHeaderTimeout: 2 * time.Second, MaxIdlePerHost: 4, CAFile: caPath,
	})
	response, err := open(t, trusting, Request{URL: server.URL, Eco: "pypi"})
	if err != nil {
		t.Fatalf("a configured CA did not make the origin reachable: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
}

// A CA file that cannot be read is a startup error, not a warning. Silently not
// trusting a sibling fails every fetch to it with a certificate error nobody could
// explain from the configuration.
func TestPoolRefusesAnUnreadableCA(t *testing.T) {
	if _, err := New(config.Upstream{
		CAFile: filepath.Join(t.TempDir(), "missing.crt"),
	}, obs.NewMetrics()); err == nil { //nolint:staticcheck // asserting the error
		t.Fatal("a missing ca_file was accepted")
	}
	bad := filepath.Join(t.TempDir(), "bad.crt")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(config.Upstream{CAFile: bad}, obs.NewMetrics()); err == nil {
		t.Fatal("a ca_file with no certificate in it was accepted")
	}
}

// A relayed attempt keeps its URL and changes only its route; the fallback behind it is
// the same URL asked directly. What falls through is decided exactly as for origins.
func TestProxyRoutesOneAttemptAndNotItsFallback(t *testing.T) {
	var seen []string
	var user string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.RequestURI)
		asked := http.Request{Header: http.Header{
			"Authorization": {r.Header.Get("Proxy-Authorization")},
		}}
		user, _, _ = asked.BasicAuth()
		_, _ = w.Write([]byte("via team"))
	}))
	t.Cleanup(proxy.Close)
	origin, originHits := answering(t, http.StatusOK, "direct")
	target := origin + "/debian/pool/demo.deb"
	teamProxy := "http://work@" + strings.TrimPrefix(proxy.URL, "http://")

	response, err := open(t, chainPool(t), Request{
		URL: target, Proxy: teamProxy, Fallbacks: []Fallback{{URL: target}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, response); body != "via team" {
		t.Fatalf("relayed body = %q", body)
	}
	if len(seen) != 1 || seen[0] != target || user != "work" || *originHits != 0 {
		t.Fatalf("proxy saw %v as %q, origin hits %d", seen, user, *originHits)
	}

	response, err = open(t, chainPool(t), Request{
		URL: target, Proxy: unreachable(t), Fallbacks: []Fallback{{URL: target}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, response); body != "direct" || *originHits != 1 {
		t.Fatalf("fallback body = %q, origin hits %d", body, *originHits)
	}

	notFound, _ := answering(t, http.StatusNotFound, "missing")
	response, err = open(t, chainPool(t), Request{
		URL: target, Proxy: notFound, Fallbacks: []Fallback{{URL: target}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound || *originHits != 1 {
		t.Fatalf("a relayed 404 went around the team cache: %d, origin hits %d",
			response.StatusCode, *originHits)
	}
}

func readAll(t *testing.T, response *http.Response) string {
	t.Helper()
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// Bytes are credited to whoever actually sent them: the relay, the fallback that answered
// after the head was unreachable, the host a redirect ended at.
func TestServedByNamesTheSourceThatAnswered(t *testing.T) {
	origin, _ := answering(t, http.StatusOK, "body")
	proxy, _ := answering(t, http.StatusOK, "via proxy")
	target := origin + "/pool/x.deb"

	relayed, err := open(t, chainPool(t), Request{URL: target, Proxy: proxy})
	if err != nil {
		t.Fatal(err)
	}
	_ = readAll(t, relayed)
	if got := ServedBy(relayed); got != proxy {
		t.Errorf("relayed: ServedBy = %s, want the proxy %s", got, proxy)
	}

	fellBack, err := open(t, chainPool(t), Request{
		URL: unreachable(t) + "/pool/x.deb", Fallbacks: []Fallback{{URL: target}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = readAll(t, fellBack)
	if got := ServedBy(fellBack); got != target {
		t.Errorf("fallback: ServedBy = %s, want %s", got, target)
	}

	redirector := httptest.NewServer(http.RedirectHandler(target, http.StatusFound))
	t.Cleanup(redirector.Close)
	redirected, err := open(t, chainPool(t), Request{URL: redirector.URL + "/x"})
	if err != nil {
		t.Fatal(err)
	}
	_ = readAll(t, redirected)
	if got := ServedBy(redirected); got != target {
		t.Errorf("redirect: ServedBy = %s, want %s", got, target)
	}
}

// A connection reset before any response is asked again on a fresh connection; the same
// failure every time still fails, after the retries, rather than hanging.
func TestResetBeforeAnyResponseIsRetried(t *testing.T) {
	var attempts atomic.Int64
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close() // gone before a single byte of response
			}
			return
		}
		_, _ = w.Write([]byte("second time"))
	}))
	t.Cleanup(flaky.Close)
	response, err := open(t, chainPool(t), Request{URL: flaky.URL + "/x.whl"})
	if err != nil {
		t.Fatalf("a reset before the response was not retried: %v", err)
	}
	if body := readAll(t, response); body != "second time" || attempts.Load() != 2 {
		t.Fatalf("body %q after %d attempts", body, attempts.Load())
	}

	var always atomic.Int64
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		always.Add(1)
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(broken.Close)
	if _, err := open(t, chainPool(t), Request{URL: broken.URL + "/x.whl"}); err == nil {
		t.Fatal("an origin that always resets answered")
	}
	if n := always.Load(); n != 1+transportRetries {
		t.Fatalf("asked %d times, want %d", n, 1+transportRetries)
	}
}

// Upstream requests use HTTP/1.1 unless HTTP/2 is asked for, even against a server that
// offers it.
func TestHTTP2IsOptIn(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Proto))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		http2 bool
		proto string
	}{{false, "HTTP/1.1"}, {true, "HTTP/2.0"}} {
		pool := mustPool(t, config.Upstream{
			RequestTimeout: 10 * time.Second, ConnectTimeout: 2 * time.Second,
			ResponseHeaderTimeout: 2 * time.Second, CAFile: caFile, HTTP2: want.http2,
		})
		response, err := open(t, pool, Request{URL: server.URL + "/x"})
		if err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, response); got != want.proto {
			t.Errorf("http2=%v: the origin saw %s, want %s", want.http2, got, want.proto)
		}
	}
}

// A 403 is final — a wrong credential is found by failing, not by going around it —
// except from an attempt marked Optional: a sibling's proxy, which declines what it will
// not relay (and relays nothing on an older pkgcache). There the next attempt answers.
func TestOptionalAttemptFallsThroughARefusal(t *testing.T) {
	refusing, refused := answering(t, http.StatusForbidden, "not through me")
	serving, served := answering(t, http.StatusOK, "from the next place")
	for _, optional := range []bool{true, false} {
		response, err := open(t, chainPool(t), Request{
			URL: refusing, Eco: "apt", Optional: optional,
			Fallbacks: []Fallback{{URL: serving}},
		})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		want, wantBody := http.StatusForbidden, "not through me"
		if optional {
			want, wantBody = http.StatusOK, "from the next place"
		}
		if response.StatusCode != want || string(body) != wantBody {
			t.Errorf("optional=%v: %d %q, want %d %q", optional, response.StatusCode, body, want, wantBody)
		}
	}
	if *refused != 2 || *served != 1 {
		t.Fatalf("refused %d, served %d; want 2 and 1", *refused, *served)
	}
}
