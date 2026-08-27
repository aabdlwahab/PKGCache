package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Precedence, lowest to highest: defaults, config file, environment, flags.
// Anything a flag does not set is left at whatever the lower layer decided, so
// partially specifying a layer is always safe.

// DefaultDataDir is where state lives when nothing says otherwise.
const DefaultDataDir = "/var/lib/pkgreg"

// envPrefix namespaces every environment variable this program reads.
const envPrefix = "PKGREG_"

// Defaults returns a usable configuration for a single-host deployment.
func Defaults() Snapshot {
	return Snapshot{
		DataDir: DefaultDataDir,
		Server: Server{
			SinglePort:  true,
			UnifiedAddr: ":8443",
			ProxyAddr:   ":3142",
			AdminAddr:   ":8088",
			// Long enough for a slow client to finish its headers, short enough that
			// an idle socket cannot be parked forever.
			ReadHeaderTimeout: 30 * time.Second,
			ShutdownGrace:     30 * time.Second,
			TrustProxy:        false,
		},
		Log: Log{Level: "info", Format: "json", Access: true},
		Catalog: Catalog{
			ReadPoolSize:  8,
			BatchInterval: 100 * time.Millisecond,
			BatchSize:     500,
			CacheSize:     4096,
		},
		Upstream: Upstream{
			RequestTimeout: 20 * time.Minute,
			ConnectTimeout: 30 * time.Second,
			// Generous for an index a slow origin has to generate, and still two orders
			// of magnitude below the request timeout.
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdlePerHost:        32,
			UserAgent:             "pkgreg/1",
		},
		Git: Git{
			RefsTTL:        60 * time.Second,
			MaxUploadPacks: 8,
		},
		Maintenance: Maintenance{
			GCInterval: 6 * time.Hour,
			// An hour comfortably exceeds the longest plausible single fetch, so a
			// collection can never race a download that has committed its blob but
			// not yet its entry row.
			GCGrace:            time.Hour,
			EvictInterval:      30 * time.Minute,
			StatsFlushInterval: 30 * time.Second,
		},
		Auth: Auth{
			SessionTTL: 12 * time.Hour,
			// Read-only browsing of the global project without an account. See
			// Auth.GuestRead for what that does and does not include.
			GuestRead:    true,
			MaxJSONBytes: 64 << 20,
		},
		Projects:         map[string]Project{},
		ProjectUpstreams: map[string]map[string]map[string][]Endpoint{},
		ProjectPeers:     map[string]map[string][]Peer{},
	}
}

// Flags are the command-line overrides. A nil field means "not specified", which is
// what keeps a flag from silently overwriting a file or environment setting with a
// zero value.
type Flags struct {
	ConfigFile  string
	DataDir     *string
	UnifiedAddr *string
	ProxyAddr   *string
	AdminAddr   *string
	AdminOnly   *bool
	SinglePort  *bool
	Headless    *bool
	LogLevel    *string
	LogFormat   *string
	Offline     *bool
}

// Load assembles a Snapshot from every source and validates the result.
func Load(f Flags) (*Snapshot, error) {
	s := Defaults()

	if f.ConfigFile != "" {
		if err := applyFile(&s, f.ConfigFile); err != nil {
			return nil, err
		}
	} else if p := os.Getenv(envPrefix + "CONFIG"); p != "" {
		if err := applyFile(&s, p); err != nil {
			return nil, err
		}
	}
	if err := applyEnv(&s); err != nil {
		return nil, err
	}
	applyFlags(&s, f)

	if s.DataDir != "" {
		abs, err := filepath.Abs(s.DataDir)
		if err != nil {
			return nil, fmt.Errorf("config: resolve data_dir: %w", err)
		}
		s.DataDir = abs
	}
	if s.Projects == nil {
		s.Projects = map[string]Project{}
	}
	s.adoptDataDirCertificates()
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// adoptDataDirCertificates makes `pkgreg init` followed by `pkgreg serve -data-dir X`
// serve TLS, which it previously did not.
//
// init mints certs/ca.crt, certs/server.crt and certs/server.key at fixed paths inside
// the data directory, and writes a starter config naming them. Anyone who then started
// the server with -data-dir instead of -config — the shorter, more obvious form, and the
// one init's own output uses for other commands — got a server with no certificate pair,
// which means plain HTTP only. Nothing said so. The CA was still served, so the console
// and the tutorial rendered a complete-looking start command reading
// `-server http://host:port`, and pkgreg-client rejected every one of them with "server
// must use https": the certificates were sitting unused, one directory away, while the
// page insisted the setup was ready.
//
// So the paths are adopted here rather than in the serve command: config is the only
// place that decides what a tunable's value is, and doing it here means `serve` and
// `doctor` cannot disagree about whether this host has TLS — they did, and doctor
// reported a healthy certificate for a server that was answering in cleartext.
//
// Only files that exist are adopted, and only when neither half was configured. An
// operator who names their own pair keeps it; one who has no certificates still gets a
// plain-HTTP server rather than a startup failure.
func (s *Snapshot) adoptDataDirCertificates() {
	if s.DataDir == "" {
		return
	}
	if s.Server.TLS.CAFile == "" {
		if ca := filepath.Join(s.CertsDir(), "ca.crt"); fileExists(ca) {
			s.Server.TLS.CAFile = ca
		}
	}
	if s.Server.TLS.CertFile != "" || s.Server.TLS.KeyFile != "" {
		return
	}
	cert := filepath.Join(s.CertsDir(), "server.crt")
	key := filepath.Join(s.CertsDir(), "server.key")
	if fileExists(cert) && fileExists(key) {
		s.Server.TLS.CertFile = cert
		s.Server.TLS.KeyFile = key
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// applyFile overlays a YAML file. A missing file named explicitly is an error; the
// caller decides whether to name one.
func applyFile(s *Snapshot, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // a typo'd key is a startup error, not a silent no-op
	if err := dec.Decode(s); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	return nil
}

// envMap declares every environment variable, so the set is greppable in one place
// and `pkgreg doctor` can enumerate it.
func applyEnv(s *Snapshot) error {
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(envPrefix + key); ok {
			*dst = v
		}
	}
	num := func(key string, dst *int) error {
		v, ok := os.LookupEnv(envPrefix + key)
		if !ok {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: %s%s: %w", envPrefix, key, err)
		}
		*dst = n
		return nil
	}
	num64 := func(key string, dst *int64) error {
		v, ok := os.LookupEnv(envPrefix + key)
		if !ok {
			return nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("config: %s%s: %w", envPrefix, key, err)
		}
		*dst = n
		return nil
	}
	boolean := func(key string, dst *bool) error {
		v, ok := os.LookupEnv(envPrefix + key)
		if !ok {
			return nil
		}
		b, err := parseBool(v)
		if err != nil {
			return fmt.Errorf("config: %s%s: %w", envPrefix, key, err)
		}
		*dst = b
		return nil
	}
	dur := func(key string, dst *time.Duration) error {
		v, ok := os.LookupEnv(envPrefix + key)
		if !ok {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("config: %s%s: %w", envPrefix, key, err)
		}
		*dst = d
		return nil
	}

	str("DATA_DIR", &s.DataDir)
	str("UNIFIED_ADDR", &s.Server.UnifiedAddr)
	str("PROXY_ADDR", &s.Server.ProxyAddr)
	str("ADMIN_ADDR", &s.Server.AdminAddr)
	str("TLS_CERT", &s.Server.TLS.CertFile)
	str("TLS_KEY", &s.Server.TLS.KeyFile)
	str("TLS_CA", &s.Server.TLS.CAFile)
	str("LOG_LEVEL", &s.Log.Level)
	str("LOG_FORMAT", &s.Log.Format)
	str("USER_AGENT", &s.Upstream.UserAgent)
	str("UPSTREAM_CA", &s.Upstream.CAFile)
	str("ROOT_USER", &s.Auth.RootUser)
	str("ROOT_PASSWORD", &s.Auth.RootPassword)
	str("REGISTRY_MIRROR", &s.Server.RegistryMirror)
	str("PUBLIC_ORIGIN", &s.Auth.PublicOrigin)

	for _, e := range []struct {
		key string
		dst *bool
	}{
		{"SINGLE_PORT", &s.Server.SinglePort},
		{"HEADLESS", &s.Server.Headless},
		{"TRUST_PROXY", &s.Server.TrustProxy},
		{"LOG_ACCESS", &s.Log.Access},
		{"OFFLINE", &s.Upstream.Offline},
		{"ANON_READ", &s.Auth.AnonRead},
		{"GUEST_READ", &s.Auth.GuestRead},
	} {
		if err := boolean(e.key, e.dst); err != nil {
			return err
		}
	}
	// Tri-state: unset means "derive from the public origin's scheme".
	if v, ok := os.LookupEnv(envPrefix + "COOKIE_SECURE"); ok {
		b, err := parseBool(v)
		if err != nil {
			return fmt.Errorf("config: %sCOOKIE_SECURE: %w", envPrefix, err)
		}
		s.Auth.CookieSecure = &b
	}

	for _, e := range []struct {
		key string
		dst *time.Duration
	}{
		{"READ_HEADER_TIMEOUT", &s.Server.ReadHeaderTimeout},
		{"SHUTDOWN_GRACE", &s.Server.ShutdownGrace},
		{"BATCH_INTERVAL", &s.Catalog.BatchInterval},
		{"REQUEST_TIMEOUT", &s.Upstream.RequestTimeout},
		{"CONNECT_TIMEOUT", &s.Upstream.ConnectTimeout},
		{"RESPONSE_HEADER_TIMEOUT", &s.Upstream.ResponseHeaderTimeout},
		{"GIT_REFS_TTL", &s.Git.RefsTTL},
		{"GC_INTERVAL", &s.Maintenance.GCInterval},
		{"GC_GRACE", &s.Maintenance.GCGrace},
		{"EVICT_INTERVAL", &s.Maintenance.EvictInterval},
		{"EVICT_TTL", &s.Maintenance.EvictTTL},
		{"STATS_FLUSH_INTERVAL", &s.Maintenance.StatsFlushInterval},
		{"SESSION_TTL", &s.Auth.SessionTTL},
	} {
		if err := dur(e.key, e.dst); err != nil {
			return err
		}
	}

	for _, e := range []struct {
		key string
		dst *int
	}{
		{"READ_POOL_SIZE", &s.Catalog.ReadPoolSize},
		{"BATCH_SIZE", &s.Catalog.BatchSize},
		{"CACHE_SIZE", &s.Catalog.CacheSize},
		{"MAX_IDLE_PER_HOST", &s.Upstream.MaxIdlePerHost},
		{"GIT_MAX_UPLOAD_PACKS", &s.Git.MaxUploadPacks},
	} {
		if err := num(e.key, e.dst); err != nil {
			return err
		}
	}

	for _, e := range []struct {
		key string
		dst *int64
	}{
		{"EVICT_TARGET_BYTES", &s.Maintenance.EvictTargetBytes},
		{"EVICT_MIN_FREE_BYTES", &s.Maintenance.EvictMinFreeBytes},
		{"MAX_JSON_BYTES", &s.Auth.MaxJSONBytes},
	} {
		if err := num64(e.key, e.dst); err != nil {
			return err
		}
	}

	if v, ok := os.LookupEnv(envPrefix + "PROXY_ALLOWLIST"); ok {
		s.Server.ProxyAllowlist = splitList(v)
	}
	if v, ok := os.LookupEnv(envPrefix + "REGISTRY_ALLOWLIST"); ok {
		s.Server.RegistryAllowlist = splitList(v)
	}
	return nil
}

func applyFlags(s *Snapshot, f Flags) {
	setStr(&s.DataDir, f.DataDir)
	setStr(&s.Server.UnifiedAddr, f.UnifiedAddr)
	setStr(&s.Server.ProxyAddr, f.ProxyAddr)
	setStr(&s.Server.AdminAddr, f.AdminAddr)
	setStr(&s.Log.Level, f.LogLevel)
	setStr(&s.Log.Format, f.LogFormat)
	setBool(&s.Server.SinglePort, f.SinglePort)
	setBool(&s.Server.Headless, f.Headless)
	setBool(&s.Upstream.Offline, f.Offline)
}

func setStr(dst, v *string) {
	if v != nil {
		*dst = *v
	}
}

func setBool(dst, v *bool) {
	if v != nil {
		*dst = *v
	}
}

// parseBool accepts the spellings operators actually type, not just Go's.
func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("not a boolean: %q", v)
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Paths derived from the data directory. Centralised so the layout is described in
// exactly one place.

// BlobRoot is the content-addressed store and the managed directories.
func (s *Snapshot) BlobRoot() string { return s.DataDir }

// CatalogPath is the metadata database.
func (s *Snapshot) CatalogPath() string { return filepath.Join(s.DataDir, "db", "catalog.db") }

// ControlPath is the control-plane database.
func (s *Snapshot) ControlPath() string { return filepath.Join(s.DataDir, "db", "control.db") }

// ControlKeyPath is the host-local key used to seal upstream credentials.
func (s *Snapshot) ControlKeyPath() string { return filepath.Join(s.DataDir, "db", "host.key") }

// CertsDir holds the CA and the serving certificate.
func (s *Snapshot) CertsDir() string { return filepath.Join(s.DataDir, "certs") }

// ShuttleDir stages air-gap exports and imports.
func (s *Snapshot) ShuttleDir() string { return filepath.Join(s.DataDir, "shuttle") }

// EnsureDirs creates the data-directory layout.
//
// Local mode differs twice: 0700 rather than 0755, because the loopback socket in
// front of this tree is not per-user and the file mode is the only thing keeping one
// developer's cache out of another's reach; and no certs directory, because pkgcache
// refuses TLS outright.
func (s *Snapshot) EnsureDirs() error {
	perm := fs.FileMode(0o755)
	dirs := []string{
		s.DataDir,
		filepath.Join(s.DataDir, "db"),
		s.CertsDir(),
		filepath.Join(s.ShuttleDir(), "in"),
		filepath.Join(s.ShuttleDir(), "out"),
	}
	if s.Local.Enabled {
		perm = 0o700
		dirs = []string{
			s.DataDir,
			filepath.Join(s.DataDir, "db"),
			filepath.Join(s.ShuttleDir(), "in"),
			filepath.Join(s.ShuttleDir(), "out"),
		}
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, perm); err != nil {
			// The default data directory lives under /var/lib, so the first thing a
			// new user meets is a bare "permission denied" on a path they never
			// chose. Naming the two ways forward here costs nothing and saves that
			// person a detour through the source.
			if errors.Is(err, fs.ErrPermission) {
				if s.Local.Enabled {
					return fmt.Errorf(
						"config: cannot create %s: %w\n"+
							"  pkgcache keeps its cache in a directory you own; pick another with\n"+
							"      %sDATA_DIR=~/pkgcache pkgcache ...\n"+
							"  and run it again", d, err, LocalEnvPrefix)
				}
				return fmt.Errorf(
					"config: cannot create %s: %w\n"+
						"  either run as root:      sudo pkgreg ...\n"+
						"  or pick a directory you own:\n"+
						"      pkgreg ... -data-dir ~/pkgreg\n"+
						"      PKGREG_DATA_DIR=~/pkgreg pkgreg ...\n"+
						"  and run it again", d, err)
			}
			return fmt.Errorf("config: create %s: %w", d, err)
		}
	}
	// MkdirAll honours the process umask, so a directory that already existed — or one
	// created under a umask of 022 — keeps a mode this cache should not have.
	if s.Local.Enabled {
		if err := os.Chmod(s.DataDir, perm); err != nil {
			return fmt.Errorf("config: restrict %s: %w", s.DataDir, err)
		}
	}
	return nil
}
