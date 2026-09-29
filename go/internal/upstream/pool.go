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
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"syscall"
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
	// Proxy is an HTTP forward proxy this attempt is sent through, with the URL left as
	// it is. Empty means a direct connection.
	//
	// This is how an apt request reaches a team's cache: apt names the mirror itself, so
	// there is no origin to swap for the team's — the request has to travel through the
	// team's own forward proxy instead. Per attempt, because the fallback behind it is
	// the same URL fetched directly.
	Proxy string
	// Optional marks an attempt that may decline: its 403 moves on to the next attempt
	// instead of being the answer. See openChain.
	Optional bool
}

// Fallback is an alternate origin for a request, with its own credential and route.
type Fallback struct {
	URL        string
	Credential *Credential
	Proxy      string
	Optional   bool
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
	idle    time.Duration
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
		cfg: cfg, metrics: m, ua: ua, timeout: cfg.RequestTimeout, idle: cfg.BodyIdleTimeout,
		tokens: newTokenCache(),
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
		// whole request timeout before any fallback is tried — a budget that exists so a
		// 2.5 GB wheel can finish, not so a stalled index can. The body's silences are
		// bounded separately, by BodyIdleTimeout; see watchIdle.
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		// HTTP/1.1 unless configured otherwise: a connection per request, so one slow or
		// reset connection holds up one transfer rather than every one to that host.
		// See config.Upstream.HTTP2 for the measurements.
		ForceAttemptHTTP2: cfg.HTTP2,
		// Chosen per request, never from the environment. An http_proxy inherited by a
		// daemon would silently reroute every origin it fetches from; a relay has to be
		// something this cache was configured with.
		Proxy: proxyFromContext,
	}
	if !cfg.HTTP2 {
		// An empty map rather than nil is what keeps HTTP/2 off even where a TLS config
		// would otherwise let the transport negotiate it.
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
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
	resp.Body = watchIdle(resp.Body, p.idle, cancel)
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
//     a problem that will otherwise be found once rather than never. The exception is an
//     attempt marked Optional: a sibling's proxy, which carries no credential and relays
//     apt for public names only — and relays nothing at all on an older pkgcache. Its 403
//     means "not through me", and the next attempt is the answer.
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
		next.Proxy = fallback.Proxy
		next.Optional = fallback.Optional
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
		case (resp.StatusCode >= 500 || resp.StatusCode == http.StatusForbidden && attempt.Optional) && !last:
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

// transportRetries is how many more times a request is sent after its connection failed
// before any response arrived.
const transportRetries = 2

// openOne is a single origin's attempt, including the bearer-token dance.
func (p *Pool) openOne(ctx context.Context, method string, r Request) (*http.Response, error) {
	resp, err := p.do(ctx, method, r)
	for retry := 1; err != nil && retry <= transportRetries && retryable(ctx, method, r, err); retry++ {
		// A CDN resetting a connection before it answered — pypi.nvidia.com did, twice in
		// one build, while the same request a minute later took 0.2 s — failed the whole
		// fetch, and every build asking for that wheel with it. Nothing had arrived, so
		// asking again, on a fresh connection, repeats nothing.
		select {
		case <-time.After(time.Duration(retry) * time.Second):
		case <-ctx.Done():
			return nil, err
		}
		resp, err = p.do(ctx, method, r)
	}
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

// retryable reports a request whose connection failed before it was answered: reset or
// closed by the other side. Only for a request that is safe to repeat, and not for a
// timeout — an origin that is not answering is the fallback chain's business, and waiting
// out its timeout twice more would only delay that.
func retryable(ctx context.Context, method string, r Request, err error) bool {
	if ctx.Err() != nil || len(r.Body) > 0 || (method != http.MethodGet && method != http.MethodHead) {
		return false
	}
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.EOF)
}

func (p *Pool) do(ctx context.Context, method string, r Request) (*http.Response, error) {
	if r.Proxy != "" {
		proxy, err := url.Parse(r.Proxy)
		if err != nil || proxy.Host == "" {
			return nil, fmt.Errorf("upstream: invalid proxy %q for %s", r.Proxy, r.URL)
		}
		ctx = context.WithValue(ctx, proxyKey{}, proxy)
	}
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

// proxyKey carries one attempt's forward proxy from do to the transport.
type proxyKey struct{}

func proxyFromContext(req *http.Request) (*url.URL, error) {
	proxy, _ := req.Context().Value(proxyKey{}).(*url.URL)
	return proxy, nil
}

func (p *Pool) observeError(r Request, code string) {
	if p.metrics == nil {
		return
	}
	p.metrics.UpstreamErrors.WithLabelValues(hostOf(r.URL), code).Inc()
}

// ServedBy names where a response actually came from, for the metrics that say where
// bytes were fetched: the proxy it was relayed through, or else the URL it was finally
// fetched from — a fallback's rather than the first one tried, and past any redirect.
//
// Counting against the URL first asked for put every byte of a chain on its head, so a
// dashboard read "all from the team cache" on a day the team cache was down and every
// byte had come from the public registry behind it.
func ServedBy(resp *http.Response) string {
	if resp == nil || resp.Request == nil {
		return ""
	}
	if proxy, ok := resp.Request.Context().Value(proxyKey{}).(*url.URL); ok && proxy != nil {
		return proxy.String()
	}
	return resp.Request.URL.String()
}

// CountBytes records bytes pulled from an upstream, for the bytes-saved calculation.
func (p *Pool) CountBytes(eco, rawURL string, n int64) {
	if p.metrics == nil || n <= 0 {
		return
	}
	p.metrics.UpstreamBytes.WithLabelValues(eco, hostOf(rawURL)).Add(float64(n))
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
