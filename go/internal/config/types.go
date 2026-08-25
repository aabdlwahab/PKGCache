// Package config owns every tunable in the process.
//
// Two rules make it work. First, nothing outside this package reads an environment
// variable — handlers take a *Snapshot. Second, a Snapshot is immutable: changing
// configuration builds a new one and swaps it atomically, so a request either sees
// the whole old configuration or the whole new one, never a mixture.
//
// That replaces the previous design's 5-second registry poll plus a separately
// mtime-cached token file: a project created in the console is routable on the next
// request rather than within five seconds.
package config

import (
	"fmt"
	"log/slog"
	"time"
)

// Snapshot is a complete, immutable view of the configuration.
//
// Never mutate a Snapshot that has been published. Build a new one.
type Snapshot struct {
	DataDir     string             `yaml:"data_dir"`
	Server      Server             `yaml:"server"`
	Log         Log                `yaml:"log"`
	Catalog     Catalog            `yaml:"catalog"`
	Upstream    Upstream           `yaml:"upstream"`
	Git         Git                `yaml:"git"`
	Maintenance Maintenance        `yaml:"maintenance"`
	Auth        Auth               `yaml:"auth"`
	Projects    map[string]Project `yaml:"-"` // owned by the control plane, not the file
	// ProjectUpstreams is project → ecosystem → name → an ordered chain of origins.
	// It is a live projection of control.db and is never decoded from the static
	// configuration file.
	//
	// A chain rather than one URL because an index can have more than one place to look:
	// a laptop's cache asks the team's cache first and the public registry only if that
	// is unreachable. A chain of one, which is every configuration that predates this,
	// behaves exactly as a single URL did.
	ProjectUpstreams map[string]map[string]map[string][]Endpoint `yaml:"-"`
	// ProjectPeers is project → ecosystem → priority-ordered sibling instances.
	ProjectPeers map[string]map[string][]Peer `yaml:"-"`
	// Local is set only by pkgcache. See local.go.
	Local Local `yaml:"-"`
}

// UpstreamCredential is plaintext only in a live immutable Snapshot.
type UpstreamCredential struct {
	Kind     string
	Username string
	Password string
	Token    string
}

// Endpoint is one origin in an upstream chain.
//
// The credential travels with the origin rather than in a parallel map keyed by name,
// which is what a chain makes necessary: two endpoints under one index name — a team
// cache and the public registry behind it — need different credentials, and a map keyed
// by name can only hold one of them.
type Endpoint struct {
	// URL is the origin, without a trailing slash by convention.
	URL string
	// Priority orders the chain, lowest first. Equal priorities fall back to URL order
	// so that a configuration always produces the same chain.
	Priority int
	// Credential authenticates requests to this origin. The zero value is anonymous.
	Credential UpstreamCredential
}

// Anonymous reports whether this endpoint carries no credential.
func (e Endpoint) Anonymous() bool { return e.Credential == UpstreamCredential{} }

// Peer is a digest-addressed sibling cache endpoint.
type Peer struct {
	URL        string
	Priority   int
	Credential UpstreamCredential
}

// Server configures the listeners.
type Server struct {
	// SinglePort serves TLS, plain HTTP and the apt forward proxy on one address by
	// sniffing the first byte of each connection (0x16 means a TLS handshake). One
	// firewall rule instead of three. Set false to bind the three addresses below.
	SinglePort bool `yaml:"single_port"`

	// UnifiedAddr carries docker (/v2/…) plus npm/pypi/git/files
	// (/<project>/<eco>/…) over TLS.
	UnifiedAddr string `yaml:"unified_addr"`
	// ProxyAddr is the apt/apk forward proxy. It cannot share a TLS listener:
	// busybox wget and apt < 1.6 cannot speak to a TLS proxy.
	ProxyAddr string `yaml:"proxy_addr"`
	// AdminAddr serves the console and the control API.
	AdminAddr string `yaml:"admin_addr"`

	// Headless turns off the browser console, landing page and tutorial. Everything
	// a program talks to stays up: the control API, metrics, health and the data
	// plane. For deployments driven entirely by CLI or CI, where a login form on a
	// reachable port is attack surface that buys nothing.
	Headless bool `yaml:"headless"`

	TLS TLS `yaml:"tls"`

	// ReadHeaderTimeout bounds slow-header attacks. There is deliberately no
	// whole-request timeout: a 2.5 GB wheel legitimately takes minutes.
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	// ShutdownGrace is how long in-flight downloads may finish after SIGTERM.
	ShutdownGrace time.Duration `yaml:"shutdown_grace"`
	// TrustProxy honours X-Forwarded-For/Proto. Leave off unless a trusted reverse
	// proxy is in front: otherwise a client can spoof its own login-throttle key.
	TrustProxy bool `yaml:"trust_proxy"`
	// RegistryMirror names the upstream a bare /v2/ request resolves against, which
	// is what lets this instance act as a Docker registry mirror.
	//
	// The OCI routes are namespaced — /v2/dockerhub/library/alpine — because one cache
	// fronts several registries and the first segment picks which. A daemon configured
	// with registry-mirrors does not know that: it asks for /v2/library/alpine and
	// expects an answer. Naming a default upstream here reconciles the two, and is the
	// difference between a Dockerfile saying
	// "cache:8443/dockerhub/library/alpine:3.20" and saying "alpine:3.20".
	//
	// Empty by default: an instance that has not opted in must not start answering for
	// repositories it was never asked about.
	RegistryMirror string `yaml:"registry_mirror"`
	// RegistryAllowlist bounds registry discovery: which registries an OCI pull may
	// reach when the first path segment names a host this cache was never configured
	// with. Entries match like ProxyAllowlist — case-insensitive hosts, a leading
	// "*." admits subdomains — and "*" admits anything.
	//
	// Empty is the default and means every *public* registry: a dotted DNS name with
	// no port. That is what makes "docker pull cache:8443/nvcr.io/nvidia/pytorch"
	// work on an instance nobody configured for nvcr.io, which is the whole point of
	// discovery. What empty deliberately does not include is an address that means
	// something different here than it does to the caller — an IP literal, localhost,
	// a host:port on this machine's network — because the first path segment is
	// chosen by whoever runs the pull, and 169.254.169.254 is a path segment.
	//
	// Listing hosts here narrows discovery to exactly those, and is also how a
	// private registry — "registry.internal:5000" — is opted back in.
	RegistryAllowlist []string `yaml:"registry_allowlist"`
	// ProxyAllowlist restricts the apt forward proxy to these upstream hosts.
	// Empty means relay anywhere, which is the historical behaviour and is only
	// appropriate on a trusted network.
	ProxyAllowlist []string `yaml:"proxy_allowlist"`
}

// TLS configures in-process termination. There is no OpenSSL anywhere in this
// binary; certificates are minted and served by the standard library.
type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	CAFile   string `yaml:"ca_file"`
}

// Enabled reports whether a certificate pair is configured.
func (t TLS) Enabled() bool { return t.CertFile != "" && t.KeyFile != "" }

// Log configures the process logger.
type Log struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // json | text
	Access bool   `yaml:"access"` // one structured line per data-plane request
}

// SlogLevel maps the configured level onto slog's.
func (l Log) SlogLevel() slog.Level {
	switch l.Level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Catalog tunes the metadata store. See catalog.Options for why entry writes batch.
type Catalog struct {
	ReadPoolSize  int           `yaml:"read_pool_size"`
	BatchInterval time.Duration `yaml:"batch_interval"`
	BatchSize     int           `yaml:"batch_size"`
	CacheSize     int           `yaml:"cache_size"`
}

// Upstream tunes outbound fetching.
type Upstream struct {
	// RequestTimeout is generous on purpose: the largest artifact measured in a real
	// deployment is a 2.5 GB CUDA wheel, which over a slow uplink takes many minutes.
	RequestTimeout time.Duration `yaml:"request_timeout"`
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
	// ResponseHeaderTimeout bounds the wait for response headers, not for the body.
	// It is what makes a stalled origin fail over in seconds rather than after
	// RequestTimeout, which is sized for a multi-gigabyte body.
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout"`
	MaxIdlePerHost        int           `yaml:"max_idle_per_host"`
	UserAgent             string        `yaml:"user_agent"`
	// Offline makes every ecosystem serve from cache only and never touch upstream.
	// This is the air-gap hard mode: it overrides every per-project soft flag.
	Offline bool `yaml:"offline"`
	// CAFile adds a certificate authority to the ones outbound requests trust, in
	// addition to the system roots rather than instead of them.
	//
	// This is what lets a cache fetch from another cache. A pkgreg serves TLS with a
	// certificate it mints itself, so a laptop whose middle tier is the team's cache
	// cannot verify it from the system store — and the last tier in the same chain is a
	// public registry that must keep verifying normally.
	CAFile string `yaml:"ca_file"`
}

// Git tunes managed mirror freshness and CPU-heavy upload-pack negotiation.
type Git struct {
	// RefsTTL is the minimum interval between origin fetches for one mirror.
	RefsTTL time.Duration `yaml:"refs_ttl"`
	// MaxUploadPacks bounds concurrent pack generation across repositories.
	MaxUploadPacks int `yaml:"max_upload_packs"`
}

// Maintenance schedules background housekeeping.
type Maintenance struct {
	// GCInterval is how often unreferenced blobs are collected. Zero disables it.
	GCInterval time.Duration `yaml:"gc_interval"`
	// GCGrace protects blobs written but not yet entry-committed. Must comfortably
	// exceed the longest plausible fetch, or a collection could delete content an
	// in-flight request is about to serve.
	GCGrace time.Duration `yaml:"gc_grace"`
	// EvictTargetBytes is the size the store is trimmed back to. Zero disables it.
	EvictTargetBytes int64 `yaml:"evict_target_bytes"`
	// EvictMinFreeBytes triggers eviction when the filesystem drops below it.
	EvictMinFreeBytes int64 `yaml:"evict_min_free_bytes"`
	// EvictInterval schedules policy evaluation. Zero disables scheduled eviction.
	EvictInterval time.Duration `yaml:"evict_interval"`
	// EvictTTL removes entries not accessed within this duration. Zero disables TTL.
	EvictTTL           time.Duration `yaml:"evict_ttl"`
	StatsFlushInterval time.Duration `yaml:"stats_flush_interval"`
}

// Auth configures the control plane.
type Auth struct {
	// RootUser and RootPassword are the break-glass superuser, verified from the
	// environment and never stored. Setting them is what turns enforcement on.
	RootUser     string `yaml:"root_user"`
	RootPassword string `yaml:"root_password"`
	// SessionTTL bounds a console login.
	SessionTTL time.Duration `yaml:"session_ttl"`
	// PublicOrigin is the browser-facing origin when TLS terminates in front.
	// It pins the CSRF origin check and makes session cookies Secure.
	PublicOrigin string `yaml:"public_origin"`
	CookieSecure *bool  `yaml:"cookie_secure"` // nil = derive from PublicOrigin
	// AnonRead lets callers without a session perform safe reads of every project.
	// It is the broad, unscoped form and is off by default; prefer GuestRead, which
	// grants the same kind of access confined to one project and one route set.
	AnonRead bool `yaml:"anon_read"`
	// GuestRead offers a sign-in-free, read-only view of the global project.
	//
	// On by default, because the console's first job is to show a newcomer what the
	// cache holds and how to point their tools at it, and requiring an account for
	// that turns a two-minute look into a ticket. It is strictly bounded: a guest
	// session may issue safe requests to an explicit allowlist of routes, all scoped
	// to the global project, and is refused everything else — accounts, the audit
	// log, tokens, upstreams, snapshots and every mutation. See
	// internal/control/api.guestRoutes, which is the enforcement point.
	GuestRead    bool  `yaml:"guest_read"`
	MaxJSONBytes int64 `yaml:"max_json_bytes"`
}

// Project is one tenant's settings.
type Project struct {
	Name string `yaml:"name"`
	// Offline is the per-project soft flag: serve this project from cache only,
	// without touching the others.
	Offline bool `yaml:"offline"`
	// QuotaBytes caps the project's logical size. Zero means unlimited.
	QuotaBytes int64 `yaml:"quota_bytes"`
	// QuotaArtifacts caps semantic inventory rows. Zero means unlimited.
	QuotaArtifacts int64 `yaml:"quota_artifacts"`
	// RateLimit is requests per second per client. Zero means unlimited.
	RateLimit int `yaml:"rate_limit"`
	// RateBurst is the token-bucket capacity; zero defaults to RateLimit.
	RateBurst int `yaml:"rate_burst"`
	// DataPlaneAuth is "public" (anonymous pulls, the default) or "token".
	DataPlaneAuth string `yaml:"data_plane_auth"`
	Owner         string `yaml:"owner"`
}

// GlobalProject is the implicit default tenant.
const GlobalProject = "global"

// OfflineFor reports whether a project serves from cache only, combining the
// instance-wide hard mode with the project's own soft flag.
func (s *Snapshot) OfflineFor(project string) bool {
	if s.Upstream.Offline {
		return true
	}
	p, ok := s.Projects[project]
	return ok && p.Offline
}

// HasProject reports whether a project is registered. The global project always is.
func (s *Snapshot) HasProject(name string) bool {
	if name == GlobalProject {
		return true
	}
	_, ok := s.Projects[name]
	return ok
}

// Validate rejects a configuration that would fail confusingly at runtime. It runs
// once at startup so an operator gets a usable message instead of a stack trace on
// the first request.
func (s *Snapshot) Validate() error {
	if s.DataDir == "" {
		return fmt.Errorf("config: data_dir is required")
	}
	if s.Server.SinglePort {
		if s.Server.UnifiedAddr == "" {
			return fmt.Errorf("config: server.unified_addr is required")
		}
	} else if s.Server.UnifiedAddr == "" || s.Server.ProxyAddr == "" || s.Server.AdminAddr == "" {
		return fmt.Errorf("config: unified_addr, proxy_addr and admin_addr are all required " +
			"unless server.single_port is set")
	}
	if (s.Server.TLS.CertFile == "") != (s.Server.TLS.KeyFile == "") {
		return fmt.Errorf("config: tls.cert_file and tls.key_file must be set together")
	}
	if s.Auth.RootUser != "" && s.Auth.RootPassword == "" {
		return fmt.Errorf("config: auth.root_password is required when auth.root_user is set")
	}
	if s.Auth.SessionTTL <= 0 {
		return fmt.Errorf("config: auth.session_ttl must be positive")
	}
	if s.Auth.MaxJSONBytes <= 0 {
		return fmt.Errorf("config: auth.max_json_bytes must be positive")
	}
	if s.Git.RefsTTL <= 0 {
		return fmt.Errorf("config: git.refs_ttl must be positive")
	}
	if s.Git.MaxUploadPacks <= 0 {
		return fmt.Errorf("config: git.max_upload_packs must be positive")
	}
	if s.Maintenance.GCInterval > 0 && s.Maintenance.GCGrace <= 0 {
		return fmt.Errorf("config: maintenance.gc_grace must be positive when gc is enabled " +
			"— a zero grace period could collect a blob whose fetch is still in flight")
	}
	switch s.Log.Format {
	case "", "json", "text":
	default:
		return fmt.Errorf("config: log.format must be json or text, got %q", s.Log.Format)
	}
	switch s.Log.Level {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: log.level must be debug|info|warn|error, got %q", s.Log.Level)
	}
	for name, p := range s.Projects {
		switch p.DataPlaneAuth {
		case "", "public", "token":
		default:
			return fmt.Errorf("config: project %q: data_plane_auth must be public or token", name)
		}
	}
	if s.Local.Enabled {
		return s.validateLocal()
	}
	return nil
}
