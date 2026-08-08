package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Local mode is pkgreg with no network surface: one loopback port, no certificate,
// no accounts, no projects.
//
// Everything pkgcache leaves out is paid for by a single enforced invariant — it
// refuses to bind an address another machine can reach. That is checked in Validate
// rather than warned about in Posture, because a warning can be ignored and this one
// cannot be: without it, local mode is an unauthenticated control plane on a routable
// port.

// LocalPort is the fixed loopback port pkgcache binds by default.
//
// Fixed rather than ephemeral because settings outlive processes. A .npmrc or a
// pip.conf — written by hand, or by `pkgcache persist` — names a port, and has to keep
// naming the right one across restarts. Where the port is already taken by something
// that is not a pkgcache daemon, the lifecycle layer falls back to an ephemeral one and
// says so; that is precisely the case persistent settings cannot cover.
const LocalPort = 41780

// LocalEnvPrefix namespaces pkgcache's own environment variables.
//
// Deliberately not PKGREG_. A developer working inside a `pkgreg-client` shell already
// has PKGREG_* set, and PKGREG_DATA_DIR among them would point a laptop's private cache
// at a server's state directory. The two programs share a code base, not an environment.
const LocalEnvPrefix = "PKGCACHE_"

// LocalLoopback is the interface pkgcache binds. IPv4 rather than "localhost": the
// name can resolve to ::1 first, and not every client in this system reaches an IPv6
// loopback by default.
const LocalLoopback = "127.0.0.1"

// Local holds the settings that exist only in pkgcache.
//
// Not decodable from YAML — hence `yaml:"-"`. Local mode is not a flag an operator can
// turn on in a server's configuration file; it is a different program's profile, and
// making it settable would mean a stray key could disable the loopback requirement on
// a host that is genuinely serving a network.
type Local struct {
	// Enabled marks the snapshot as pkgcache's. It is what Validate and Posture branch
	// on.
	Enabled bool `yaml:"-"`
	// IdleTimeout is how long the daemon stays up with nothing to do. Zero means it
	// stays up until it is stopped, which is what persistent client settings require.
	IdleTimeout time.Duration `yaml:"-"`
}

// LocalDefaults returns the configuration profile pkgcache serves under.
//
// It is deliberately expressed as a diff against Defaults() rather than as its own
// literal: a tunable added to the server profile should reach pkgcache automatically,
// and the ones that must differ should be visible here as the short list they are.
func LocalDefaults() Snapshot {
	s := Defaults()
	address := net.JoinHostPort(LocalLoopback, strconv.Itoa(LocalPort))

	s.Server.SinglePort = true
	s.Server.UnifiedAddr = address
	// All three name the same socket, because in single-port mode one socket really
	// does serve all three roles. Leaving the other two empty would be equally correct
	// for the listener and would make the console advertise an apt proxy on port 0.
	s.Server.ProxyAddr = address
	s.Server.AdminAddr = address
	// No certificate, ever. A certificate here would not add security on loopback and
	// would actively break the product: the first-byte mux would route plaintext
	// clients to SinglePortPlainHandler, which serves the forward proxy and redirects
	// everything else — so every pip and npm request would be answered with a 308 to
	// an https origin nothing on this machine trusts.
	s.Server.TLS = TLS{}
	// The forward proxy relays wherever this developer's own apt is already pointed.
	// On a loopback socket that is not an open relay, it is the feature — and saying
	// so explicitly is what distinguishes a considered choice from an empty allowlist
	// nobody thought about. See ProxyRelaysAnywhere.
	s.Server.ProxyAllowlist = []string{ProxyRelaysAnywhere}

	// It is going to a terminal, not a log pipeline, and one line per request is noise
	// when the requests are one developer's.
	s.Log = Log{Level: "info", Format: "text", Access: false}
	// One developer, not twenty CI hosts.
	s.Catalog.ReadPoolSize = 4
	s.Upstream.UserAgent = "pkgcache/1"

	// Nothing is ever reclaimed unless the user asks for it. A background process
	// quietly deleting the wheels someone's current work depends on, to hold a number
	// nobody chose, is the wrong default on a machine somebody is sitting in front of.
	// `pkgcache prune` and `pkgcache gc` do exactly what they say, when asked.
	s.Maintenance.GCInterval = 0
	s.Maintenance.EvictInterval = 0
	s.Maintenance.EvictTargetBytes = 0
	s.Maintenance.EvictMinFreeBytes = 0
	s.Maintenance.EvictTTL = 0

	s.Local = Local{Enabled: true, IdleTimeout: 15 * time.Minute}
	return s
}

// LocalDataDir is where one user's cache lives.
//
// Per user, not per machine: /var/lib/pkgreg is a service's state directory and needs
// root to create. A cache nobody else can read is also the only thing keeping the data
// directory private, since the loopback socket in front of it is not per-user.
func LocalDataDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(LocalEnvPrefix + "DATA_DIR")); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: locate home directory: %w\n"+
			"  set %sDATA_DIR to choose the cache location explicitly", err, LocalEnvPrefix)
	}
	switch runtime.GOOS {
	case "windows":
		if base := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); base != "" {
			return filepath.Join(base, "pkgcache"), nil
		}
		return filepath.Join(home, "AppData", "Local", "pkgcache"), nil
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "pkgcache"), nil
	default:
		if base := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); base != "" {
			return filepath.Join(base, "pkgcache"), nil
		}
		return filepath.Join(home, ".local", "share", "pkgcache"), nil
	}
}

// LocalFlags are pkgcache's command-line overrides. As with Flags, a nil pointer means
// the flag was not given, so it never overwrites a lower layer with a zero.
type LocalFlags struct {
	DataDir     string
	Addr        string
	LogLevel    string
	Offline     *bool
	IdleTimeout *time.Duration
}

// LoadLocal assembles pkgcache's Snapshot: profile, then PKGCACHE_* environment, then
// flags.
//
// It reads no configuration file and no PKGREG_* variable. Both omissions are the
// point — a laptop cache with no configuration file cannot be misconfigured, and a
// developer's shell is full of PKGREG_* values that belong to somebody else's server.
func LoadLocal(f LocalFlags) (*Snapshot, error) {
	s := LocalDefaults()
	dir, err := LocalDataDir()
	if err != nil {
		return nil, err
	}
	s.DataDir = dir

	if err := applyLocalEnv(&s); err != nil {
		return nil, err
	}
	if f.DataDir != "" {
		s.DataDir = f.DataDir
	}
	if f.Addr != "" {
		if err := s.setLocalAddr(f.Addr); err != nil {
			return nil, err
		}
	}
	if f.LogLevel != "" {
		s.Log.Level = f.LogLevel
	}
	if f.Offline != nil {
		s.Upstream.Offline = *f.Offline
	}
	if f.IdleTimeout != nil {
		s.Local.IdleTimeout = *f.IdleTimeout
	}

	abs, err := filepath.Abs(s.DataDir)
	if err != nil {
		return nil, fmt.Errorf("config: resolve data directory: %w", err)
	}
	s.DataDir = abs
	// Deliberately no adoptDataDirCertificates: see the TLS comment in LocalDefaults.
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func applyLocalEnv(s *Snapshot) error {
	if v, ok := os.LookupEnv(LocalEnvPrefix + "ADDR"); ok {
		if err := s.setLocalAddr(v); err != nil {
			return err
		}
	}
	if v, ok := os.LookupEnv(LocalEnvPrefix + "LOG_LEVEL"); ok {
		s.Log.Level = v
	}
	if v, ok := os.LookupEnv(LocalEnvPrefix + "OFFLINE"); ok {
		b, err := parseBool(v)
		if err != nil {
			return fmt.Errorf("config: %sOFFLINE: %w", LocalEnvPrefix, err)
		}
		s.Upstream.Offline = b
	}
	if v, ok := os.LookupEnv(LocalEnvPrefix + "IDLE_TIMEOUT"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("config: %sIDLE_TIMEOUT: %w", LocalEnvPrefix, err)
		}
		s.Local.IdleTimeout = d
	}
	return nil
}

// setLocalAddr points all three roles at one socket, accepting either a full
// host:port or a bare port, which is what somebody overriding this actually wants to
// change.
func (s *Snapshot) setLocalAddr(value string) error {
	address := strings.TrimSpace(value)
	if address == "" {
		return fmt.Errorf("config: empty address")
	}
	if !strings.Contains(address, ":") {
		// Port 0 is accepted and means "any free port". It is what the daemon falls
		// back to when the fixed port is taken, and what tests use so they never
		// collide with each other or with a developer's own cache.
		port, err := strconv.Atoi(address)
		if err != nil || port < 0 || port > 65535 {
			return fmt.Errorf("config: %q is neither a host:port nor a port number", value)
		}
		address = net.JoinHostPort(LocalLoopback, address)
	}
	s.Server.UnifiedAddr = address
	s.Server.ProxyAddr = address
	s.Server.AdminAddr = address
	return nil
}

// LocalAddr is the one socket pkgcache serves on.
func (s *Snapshot) LocalAddr() string { return s.Server.UnifiedAddr }

// LocalBaseURL is the origin clients are pointed at. Plain HTTP, which is what removes
// pip's --trusted-host, the CA in the machine trust store, and every other privileged
// setup step. Callers with an actually-bound address — an ephemeral port, in the
// fallback case — should build this from that instead.
func (s *Snapshot) LocalBaseURL() string { return "http://" + s.LocalAddr() }

// validateLocal enforces the invariant that pays for everything pkgcache leaves out.
//
// This refuses rather than warns. Local mode runs with no TLS and no accounts, which is
// safe exactly as long as nothing off this machine can connect; a snapshot that binds
// elsewhere is not a weakly configured pkgcache, it is an unauthenticated pkgreg.
func (s *Snapshot) validateLocal() error {
	for _, addr := range []struct{ field, value string }{
		{"unified_addr", s.Server.UnifiedAddr},
		{"proxy_addr", s.Server.ProxyAddr},
		{"admin_addr", s.Server.AdminAddr},
	} {
		if addr.value == "" {
			continue
		}
		if reachableOffHost(addr.value) {
			return fmt.Errorf(
				"config: local mode refuses to bind %s=%q, which other machines can reach.\n"+
					"  pkgcache serves with no certificate and no accounts, which is safe only\n"+
					"  on loopback. Bind %s, or run `pkgreg serve` for a cache others use.",
				addr.field, addr.value, LocalLoopback)
		}
	}
	if s.Server.TLS.Enabled() {
		return fmt.Errorf(
			"config: local mode refuses a TLS certificate.\n" +
				"  On one port, a certificate makes the plaintext half serve only the apt proxy\n" +
				"  and redirect everything else, so pip and npm would be answered with a 308 to\n" +
				"  an origin this machine does not trust.")
	}
	if s.Local.IdleTimeout < 0 {
		return fmt.Errorf("config: local idle timeout must not be negative")
	}
	return nil
}
