package upstream

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/obs"
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
