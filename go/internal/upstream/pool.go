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
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/obs"
	"github.com/aabdlwahab/PKGCache/internal/trust"
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
	// Fallbacks are the places to look when URL cannot be reached, in order.
	//
	// This is how a laptop asks its team's cache first and the public registry only if
	// that is down. It is a property of the request rather than of the pool because
	// only the ecosystem layer knows which origins are interchangeable for a given
	// path — the pool knows how to try them, not which they are.
	Fallbacks []Fallback
}

// Fallback is an alternate origin for a request, with its own credential.
type Fallback struct {
	URL        string
	Credential *Credential
}

// Pool issues outbound requests.
//
// One http.Client, shared. Go's Transport pools connections per host internally, so
// a second client would only fragment that pooling and double the idle sockets.
type Pool struct {
	// client is swapped, never mutated. Trust can change while the process runs — a
	// laptop is pointed at a team cache whose CA it has just pinned — and a root pool
	// cannot be edited under a handshake. ReloadTrust builds a whole new client and
	// swaps this pointer: requests already in flight keep the client they started on,
	// and the next one gets the new roots. See ReloadTrust.
	client  atomic.Pointer[http.Client]
	cfg     config.Upstream
	metrics *obs.Metrics
	ua      string
	timeout time.Duration
	tokens  *tokenCache
}

// New builds a pool from configuration.
// New builds the pool.
//
// A CA file that cannot be read is an error rather than a warning: a cache configured to
// trust a sibling and silently not trusting it would fail every fetch to that sibling
// with a certificate error nobody could explain from the configuration.
func New(cfg config.Upstream, m *obs.Metrics) (*Pool, error) {
	ua := cfg.UserAgent
	if ua == "" {
		ua = "pkgreg/1"
	}
	pool := &Pool{
		cfg: cfg, metrics: m, ua: ua, timeout: cfg.RequestTimeout, tokens: newTokenCache(),
	}
	client, err := pool.build(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	pool.client.Store(client)
	return pool, nil
}

// ReloadTrust rebuilds the outbound client so it trusts what caFile holds now.
//
// This exists for pkgcache: configuring a team cache pins that cache's own CA, and until
// this the running daemon went on not trusting it — every fetch to the new middle tier
// failed with a certificate error that no configuration explained, until somebody
// restarted the process. An empty path returns to the system roots alone.
//
// The old client is left to its in-flight requests and its idle connections are closed,
// so nothing is cut off mid-download and nothing is kept warm against roots that have
// been replaced.
func (p *Pool) ReloadTrust(caFile string) error {
	client, err := p.build(caFile)
	if err != nil {
		return err
	}
	previous := p.client.Swap(client)
	if previous != nil {
		previous.CloseIdleConnections()
	}
	return nil
}

// build assembles one client. Every tunable comes from the configuration this pool was
// made with; only the trust file varies, which is the whole point of taking it as an
// argument rather than reading p.cfg.CAFile.
func (p *Pool) build(caFile string) (*http.Client, error) {
	cfg := p.cfg
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   cfg.ConnectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// The cache is byte-faithful. Go's Transport would otherwise add
		// Accept-Encoding: gzip and transparently decompress, so the bytes we store
		// would differ from the upstream Content-Length and from the digest the
		// index declared — truncating clients and failing integrity checks.
		DisableCompression:  true,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: cfg.MaxIdlePerHost,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: cfg.ConnectTimeout,
		// Bounds the wait for a response *header*, not the body. Without it, an origin
		// that accepts a connection and then says nothing holds the request for the
		// whole 20-minute request timeout before any fallback is tried — and 20 minutes
		// is a budget that exists so a 2.5 GB wheel can finish, not so a stalled index
		// can. The body still gets unlimited time.
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if caFile != "" {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("upstream: read ca_file %s: %w", caFile, err)
		}
		roots, err := trust.Pool(caPEM)
		if err != nil {
			return nil, fmt.Errorf("upstream: ca_file %s: %w", caFile, err)
		}
		transport.TLSClientConfig = &tls.Config{ // #nosec G402 -- defaults verify certificates.
			MinVersion: tls.VersionTLS12, RootCAs: roots,
		}
	}
	return &http.Client{
		Transport: transport,
		// No client-level timeout: it would apply to the whole body transfer and
		// abort exactly the large downloads this cache exists to make cheap.
		// Deadlines come per-request from the context instead.
		Timeout: 0,
	}, nil
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

	resp, err := p.openChain(ctx, method, r)
	if err != nil {
		cancel()
		return nil, nil, err
	}

	if resp.StatusCode >= 400 {
		p.observeError(r, strconv.Itoa(resp.StatusCode))
	}
	return resp, cancel, nil
}

// openChain tries r, then each fallback, and returns the first attempt that produces an
// answer this cache should act on.
//
// What counts as "cannot be reached" is the whole design, and being wrong in either
// direction is worse than having no fallbacks at all:
//
//   - A transport error — connection refused, DNS failure, TLS failure, a timeout — is
//     the case this exists for. Try the next origin.
//   - A 5xx is the same kind of thing at a higher layer: the origin is there and cannot
//     serve. Try the next.
//   - A 404 is NOT. A cache in deliberate offline mode answers exactly that, and
//     falling through would quietly reach the internet that somebody switched off.
//   - A 401 or 403 is NOT. That is a misconfigured credential, and going around it hides
//     a problem that will otherwise be found once rather than never.
//
// The last attempt's answer is returned whatever it is, so a chain that fails everywhere
// reports the final origin's error rather than a synthetic one.
func (p *Pool) openChain(ctx context.Context, method string, r Request) (*http.Response, error) {
	attempts := make([]Request, 0, len(r.Fallbacks)+1)
	attempts = append(attempts, r)
	for _, fallback := range r.Fallbacks {
		next := r
		next.URL = fallback.URL
		next.Credential = fallback.Credential
		next.Fallbacks = nil
		attempts = append(attempts, next)
	}

	var lastErr error
	for i, attempt := range attempts {
		last := i == len(attempts)-1
		resp, err := p.openOne(ctx, method, attempt)
		switch {
		case err != nil:
			lastErr = err
			if last {
				return nil, err
			}
		case resp.StatusCode >= 500 && !last:
			// Drained and closed before moving on: an unread body holds the connection
			// out of the pool until the transport gives up on it.
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("upstream: %s %s: %s", method, attempt.URL, resp.Status)
		default:
			return resp, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("upstream: no origin configured for %s", r.URL)
	}
	return nil, lastErr
}

// openOne is a single origin's attempt, including the bearer-token dance.
func (p *Pool) openOne(ctx context.Context, method string, r Request) (*http.Response, error) {
	resp, err := p.do(ctx, method, r)
	if err != nil {
		return nil, err
	}
	// A registry answering 401 with a bearer challenge expects us to fetch an anonymous
	// token and retry. This is the normal path for Docker Hub, ghcr and quay, not an
	// error — and it happens per origin, before any decision about falling back.
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		if token, ok := p.tokens.resolve(ctx, p, challenge); ok {
			_ = resp.Body.Close()
			authed := r
			authed.Headers = cloneHeaders(r.Headers)
			authed.Headers.Set("Authorization", "Bearer "+token)
			return p.do(ctx, method, authed)
		}
	}
	return resp, nil
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

	resp, err := p.client.Load().Do(req)
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
func (p *Pool) Client() *http.Client { return p.client.Load() }

// CloseIdleConnections releases pooled sockets. Used at shutdown and by tests.
func (p *Pool) CloseIdleConnections() { p.client.Load().CloseIdleConnections() }

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
