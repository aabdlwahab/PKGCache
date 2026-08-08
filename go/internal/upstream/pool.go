// Package upstream is the outbound HTTP boundary: one pooled client, credential
// resolution, and the anonymous bearer-token dance OCI registries require.
//
// Everything here streams. No response body is ever buffered whole, because the
// largest artifact in a real deployment is a 2.5 GB CUDA wheel and there may be
// several in flight at once.
package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/obs"
)

// ErrOffline is returned when a fetch is attempted while the cache is serving from
// its own content only.
var ErrOffline = errors.New("upstream: offline")

// Request is one outbound fetch.
type Request struct {
	URL     string
	Method  string // defaults to GET
	Headers http.Header
	// Body is replayable request content. Outbound protocol exchanges such as a
	// Git LFS batch POST are small and may need one bearer-auth retry, so a byte
	// slice is deliberate here rather than a one-shot io.Reader.
	Body []byte
	// Credential authenticates the request. Nil means anonymous.
	Credential *Credential
	// Eco labels metrics and logs.
	Eco string
}

// Pool issues outbound requests.
//
// One http.Client, shared. Go's Transport pools connections per host internally, so
// a second client would only fragment that pooling and double the idle sockets.
type Pool struct {
	client  *http.Client
	ua      string
	timeout time.Duration
	metrics *obs.Metrics
	tokens  *tokenCache
}

// New builds a pool from configuration.
func New(cfg config.Upstream, m *obs.Metrics) *Pool {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   cfg.ConnectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// The cache is byte-faithful. Go's Transport would otherwise add
		// Accept-Encoding: gzip and transparently decompress, so the bytes we store
		// would differ from the upstream Content-Length and from the digest the
		// index declared — truncating clients and failing integrity checks.
		DisableCompression:    true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   cfg.MaxIdlePerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   cfg.ConnectTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = "pkgreg/1"
	}
	return &Pool{
		client: &http.Client{
			Transport: transport,
			// No client-level timeout: it would apply to the whole body transfer and
			// abort exactly the large downloads this cache exists to make cheap.
			// Deadlines come per-request from the context instead.
			Timeout: 0,
		},
		ua:      ua,
		timeout: cfg.RequestTimeout,
		metrics: m,
		tokens:  newTokenCache(),
	}
}

// Open issues a request and returns the live response with its body unread.
//
// The caller must close the body. The returned context.CancelFunc bounds the whole
// transfer and must be called once the body is consumed — returning it explicitly,
// rather than deferring inside, is what lets a caller stream a multi-gigabyte body
// after this function has returned.
func (p *Pool) Open(ctx context.Context, r Request) (*http.Response, context.CancelFunc, error) {
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)

	resp, err := p.do(ctx, method, r)
	if err != nil {
		cancel()
		return nil, nil, err
	}

	// A registry answering 401 with a bearer challenge expects us to fetch an
	// anonymous token and retry. This is the normal path for Docker Hub, ghcr and
	// quay, not an error.
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		if token, ok := p.tokens.resolve(ctx, p, challenge); ok {
			_ = resp.Body.Close()
			authed := r
			authed.Headers = cloneHeaders(r.Headers)
			authed.Headers.Set("Authorization", "Bearer "+token)
			resp, err = p.do(ctx, method, authed)
			if err != nil {
				cancel()
				return nil, nil, err
			}
		}
	}

	if resp.StatusCode >= 400 {
		p.observeError(r, strconv.Itoa(resp.StatusCode))
	}
	return resp, cancel, nil
}

func (p *Pool) do(ctx context.Context, method string, r Request) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.URL, bytes.NewReader(r.Body))
	if err != nil {
		return nil, fmt.Errorf("upstream: build request for %s: %w", r.URL, err)
	}
	for k, vs := range r.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("User-Agent", p.ua)
	// Belt and braces alongside DisableCompression: some proxies honour an explicit
	// identity even when the transport would not have asked for gzip.
	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "identity")
	}
	if r.Credential != nil {
		r.Credential.apply(req)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		p.observeError(r, "transport")
		return nil, fmt.Errorf("upstream: %s %s: %w", method, r.URL, err)
	}
	return resp, nil
}

func (p *Pool) observeError(r Request, code string) {
	if p.metrics == nil {
		return
	}
	p.metrics.UpstreamErrors.WithLabelValues(hostOf(r.URL), code).Inc()
}

// CountBytes records bytes pulled from an upstream, for the bytes-saved calculation.
func (p *Pool) CountBytes(eco, url string, n int64) {
	if p.metrics == nil || n <= 0 {
		return
	}
	p.metrics.UpstreamBytes.WithLabelValues(eco, hostOf(url)).Add(float64(n))
}

// Client exposes the shared client for callers that must drive a request themselves
// (the peer protocol's batch probe). Prefer Open.
func (p *Pool) Client() *http.Client { return p.client }

// CloseIdleConnections releases pooled sockets. Used at shutdown and by tests.
func (p *Pool) CloseIdleConnections() { p.client.CloseIdleConnections() }

func cloneHeaders(h http.Header) http.Header {
	if h == nil {
		return http.Header{}
	}
	return h.Clone()
}

// hostOf is a metric label, so it must stay low-cardinality: host only, never the
// full URL, which would create one time series per artifact.
func hostOf(rawURL string) string {
	s := rawURL
	if i := indexOf(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := indexOf(s, "/"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "unknown"
	}
	return s
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
