package upstream

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/obs"
	testupstream "github.com/aabdlwahab/PKGCache/internal/testutil/upstream"
)

func newPool(t *testing.T) *Pool {
	t.Helper()
	cfg := config.Defaults().Upstream
	cfg.RequestTimeout = 10 * time.Second
	cfg.ConnectTimeout = 2 * time.Second
	p, poolErr := New(cfg, obs.NewMetrics())
	if poolErr != nil {
		t.Fatal(poolErr)
	}
	t.Cleanup(p.CloseIdleConnections)
	return p
}

func TestOpenStreamsBody(t *testing.T) {
	origin := testupstream.New()
	defer origin.Close()
	body := testupstream.Repeat("payload-", 100_000)
	origin.Serve("/a", body)

	p := newPool(t)
	resp, cancel, err := p.Open(context.Background(), Request{URL: origin.URLFor("/a")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cancel()
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(body) {
		t.Fatalf("got %d bytes, want %d", len(got), len(body))
	}
	if resp.ContentLength != int64(len(body)) {
		t.Fatalf("ContentLength = %d", resp.ContentLength)
	}
}

// The cache is byte-faithful. If the transport negotiated gzip and transparently
// decompressed, the bytes we store would differ from the upstream Content-Length and
// from any digest the index declared — truncating clients and failing integrity
// checks. This asserts we ask for identity and receive exactly what was sent.
func TestByteFaithfulNoTransparentDecompression(t *testing.T) {
	var sawAcceptEncoding string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding = r.Header.Get("Accept-Encoding")
		// Answer with gzip regardless, as a compressing CDN would.
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/octet-stream")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(strings.Repeat("compress me ", 1000)))
		_ = gz.Close()
	}))
	defer origin.Close()

	p := newPool(t)
	resp, cancel, err := p.Open(context.Background(), Request{URL: origin.URL})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cancel()
	defer resp.Body.Close()

	if sawAcceptEncoding != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", sawAcceptEncoding)
	}
	if resp.Uncompressed {
		t.Fatal("the transport decompressed the body; stored bytes would not match upstream")
	}
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Fatal("body is not the raw gzip stream the origin sent")
	}
}

// The anonymous bearer dance is the normal path for a public Docker Hub, ghcr or
// quay pull, not an error branch.
func TestAnonymousBearerDance(t *testing.T) {
	origin := testupstream.New()
	defer origin.Close()
	body := []byte("manifest bytes")
	origin.Handle("/v2/library/alpine/manifests/3.20", testupstream.Behaviour{
		Body: body, RequireBearer: true, ContentType: "application/vnd.oci.image.manifest.v1+json",
	})

	p := newPool(t)
	resp, cancel, err := p.Open(context.Background(), Request{
		URL: origin.URLFor("/v2/library/alpine/manifests/3.20"),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the token exchange", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(body) {
		t.Fatalf("body = %q", got)
	}
	if origin.TokenRequests.Load() != 1 {
		t.Fatalf("token endpoint hit %d times, want 1", origin.TokenRequests.Load())
	}
}

// Tokens are cached per (realm, service, scope): a second pull in the same scope
// must not re-mint one.
func TestBearerTokenIsCached(t *testing.T) {
	origin := testupstream.New()
	defer origin.Close()
	for _, p := range []string{"/v2/a/manifests/1", "/v2/a/manifests/2"} {
		origin.Handle(p, testupstream.Behaviour{Body: []byte("x"), RequireBearer: true})
	}

	pool := newPool(t)
	for _, path := range []string{"/v2/a/manifests/1", "/v2/a/manifests/2"} {
		resp, cancel, err := pool.Open(context.Background(), Request{URL: origin.URLFor(path)})
		if err != nil {
			t.Fatalf("Open %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		cancel()
	}
	if n := origin.TokenRequests.Load(); n != 1 {
		t.Fatalf("token minted %d times, want 1 (cached across pulls in one scope)", n)
	}
}

func TestParseChallenge(t *testing.T) {
	cases := []struct {
		name, in string
		want     map[string]string
		ok       bool
	}{
		{
			name: "docker hub",
			in:   `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull"`,
			want: map[string]string{
				"realm":   "https://auth.docker.io/token",
				"service": "registry.docker.io",
				"scope":   "repository:library/alpine:pull",
			},
			ok: true,
		},
		{
			// A scope legitimately contains a comma, so splitting naively breaks it.
			name: "scope with a comma",
			in:   `Bearer realm="https://x/token",scope="repository:a:pull,push"`,
			want: map[string]string{"realm": "https://x/token", "scope": "repository:a:pull,push"},
			ok:   true,
		},
		{
			name: "unquoted values",
			in:   `Bearer realm=https://x/token, service=reg`,
			want: map[string]string{"realm": "https://x/token", "service": "reg"},
			ok:   true,
		},
		{name: "lowercase scheme", in: `bearer realm="https://x"`, want: map[string]string{"realm": "https://x"}, ok: true},
		{name: "basic is not bearer", in: `Basic realm="x"`, ok: false},
		{name: "empty", in: ``, ok: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseChallenge(c.in)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			for k, want := range c.want {
				if got[k] != want {
					t.Errorf("%s = %q, want %q", k, got[k], want)
				}
			}
		})
	}
}

// A 401 that is not a bearer challenge must surface as-is rather than being retried.
func TestNonBearer401IsReturned(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer origin.Close()

	p := newPool(t)
	resp, cancel, err := p.Open(context.Background(), Request{URL: origin.URL})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the original 401", resp.StatusCode)
	}
}

func TestCredentialsApplied(t *testing.T) {
	var gotAuth string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()
	p := newPool(t)

	t.Run("basic", func(t *testing.T) {
		resp, cancel, err := p.Open(context.Background(), Request{
			URL:        origin.URL,
			Credential: &Credential{Kind: "basic", Username: "u", Password: "p"},
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		resp.Body.Close()
		cancel()
		if gotAuth != BasicAuthHeader("u", "p") {
			t.Fatalf("Authorization = %q", gotAuth)
		}
	})

	t.Run("bearer", func(t *testing.T) {
		resp, cancel, err := p.Open(context.Background(), Request{
			URL:        origin.URL,
			Credential: &Credential{Kind: "bearer", Token: "tok"},
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		resp.Body.Close()
		cancel()
		if gotAuth != "Bearer tok" {
			t.Fatalf("Authorization = %q", gotAuth)
		}
	})
}

// The cancel func returned by Open must bound the transfer; the client itself has no
// global timeout, because that would abort exactly the large downloads the cache
// exists to make cheap.
func TestContextBoundsTransferNotClient(t *testing.T) {
	p := newPool(t)
	if timeout := p.client.Load().Timeout; timeout != 0 {
		t.Fatalf("client.Timeout = %v, want 0 so large bodies are not cut off", timeout)
	}

	blocked := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-blocked
	}))
	defer origin.Close()
	defer close(blocked)

	ctx, cancelCtx := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancelCtx()
	resp, cancel, err := p.Open(ctx, Request{URL: origin.URL})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cancel()
	defer resp.Body.Close()

	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("a stalled body should be cut off by the request context")
	}
}

func TestHostOfIsLowCardinality(t *testing.T) {
	cases := map[string]string{
		"https://registry-1.docker.io/v2/library/alpine/blobs/sha256:abc": "registry-1.docker.io",
		"http://archive.ubuntu.com/ubuntu/pool/main/x.deb":                "archive.ubuntu.com",
		"https://pypi.org": "pypi.org",
		"garbage":          "garbage",
		"":                 "unknown",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConcurrentOpens(t *testing.T) {
	origin := testupstream.New()
	defer origin.Close()
	origin.Handle("/x", testupstream.Behaviour{Body: []byte("body"), RequireBearer: true})

	p := newPool(t)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, cancel, err := p.Open(context.Background(), Request{URL: origin.URLFor("/x")})
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			cancel()
		}()
	}
	wg.Wait()
}
