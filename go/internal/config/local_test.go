package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The loopback requirement is the single invariant that pays for pkgcache having no
// TLS and no accounts, so it is tested as a refusal rather than as a warning, and
// against the address forms somebody would actually type.
func TestLocalRefusesAddressesOtherMachinesCanReach(t *testing.T) {
	refused := []string{
		":41780",
		"0.0.0.0:41780",
		"[::]:41780",
		"192.168.1.10:41780",
		"cache.internal:41780",
		"*:41780",
	}
	for _, address := range refused {
		t.Run("refuse/"+address, func(t *testing.T) {
			s := LocalDefaults()
			s.DataDir = t.TempDir()
			s.Server.UnifiedAddr = address
			err := s.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %q, which other machines can reach", address)
			}
			if !strings.Contains(err.Error(), "local mode refuses to bind") {
				t.Fatalf("error does not explain the refusal: %v", err)
			}
		})
	}

	accepted := []string{
		"127.0.0.1:41780",
		"127.0.0.2:41780",
		"[::1]:41780",
		"localhost:41780",
	}
	for _, address := range accepted {
		t.Run("accept/"+address, func(t *testing.T) {
			s := LocalDefaults()
			s.DataDir = t.TempDir()
			s.Server.UnifiedAddr = address
			s.Server.ProxyAddr = address
			s.Server.AdminAddr = address
			if err := s.Validate(); err != nil {
				t.Fatalf("Validate rejected loopback address %q: %v", address, err)
			}
		})
	}
}

// A routable proxy or admin address is refused too. They are the same socket in the
// shipped profile, but nothing stops a caller setting them apart, and the forward
// proxy is the most dangerous of the three to expose.
func TestLocalChecksEveryAddress(t *testing.T) {
	for _, field := range []string{"proxy", "admin"} {
		t.Run(field, func(t *testing.T) {
			s := LocalDefaults()
			s.DataDir = t.TempDir()
			if field == "proxy" {
				s.Server.ProxyAddr = "0.0.0.0:3142"
			} else {
				s.Server.AdminAddr = "0.0.0.0:8088"
			}
			if err := s.Validate(); err == nil {
				t.Fatalf("Validate accepted a routable %s address", field)
			}
		})
	}
}

// A certificate does not merely fail to help on loopback: with one port it routes
// every plaintext client to the redirect-only handler, so pip and npm break.
func TestLocalRefusesTLS(t *testing.T) {
	s := LocalDefaults()
	s.DataDir = t.TempDir()
	s.Server.TLS = TLS{CertFile: "server.crt", KeyFile: "server.key"}
	err := s.Validate()
	if err == nil {
		t.Fatal("Validate accepted a TLS certificate in local mode")
	}
	if !strings.Contains(err.Error(), "refuses a TLS certificate") {
		t.Fatalf("error does not explain the refusal: %v", err)
	}
}

// LocalDefaults is expressed as a diff against Defaults so that a tunable added to the
// server profile reaches pkgcache automatically. This pins the diff: a new divergence
// has to be added here deliberately, which is what stops the two profiles drifting into
// two products.
func TestLocalDefaultsDifferFromServerDefaultsOnlyWhereIntended(t *testing.T) {
	server, local := Defaults(), LocalDefaults()

	if !local.Local.Enabled {
		t.Fatal("local profile is not marked as local")
	}
	if local.Local.IdleTimeout != 15*time.Minute {
		t.Fatalf("idle timeout = %v, want 15m", local.Local.IdleTimeout)
	}
	if !local.Server.SinglePort {
		t.Fatal("local profile must serve one port")
	}
	if local.Server.TLS.Enabled() {
		t.Fatal("local profile must not configure TLS")
	}
	address := "127.0.0.1:41780"
	for name, got := range map[string]string{
		"unified": local.Server.UnifiedAddr,
		"proxy":   local.Server.ProxyAddr,
		"admin":   local.Server.AdminAddr,
	} {
		if got != address {
			t.Errorf("%s address = %q, want %q", name, got, address)
		}
	}
	if !local.Server.AllowsAnyProxyHost() {
		t.Error("local profile should opt into relaying explicitly, not leave the " +
			"allowlist empty, which is indistinguishable from not having thought about it")
	}
	if local.Log.Format != "text" || local.Log.Access {
		t.Errorf("log = %+v, want text format and no access log", local.Log)
	}

	// Nothing is ever reclaimed unless the user asks. This is the decision most likely
	// to be undone by accident, because every one of these is on in the server profile.
	if local.Maintenance.GCInterval != 0 {
		t.Error("local profile must not collect on a timer")
	}
	if local.Maintenance.EvictInterval != 0 || local.Maintenance.EvictTargetBytes != 0 ||
		local.Maintenance.EvictMinFreeBytes != 0 || local.Maintenance.EvictTTL != 0 {
		t.Errorf("local profile must never evict unasked: %+v", local.Maintenance)
	}

	// Everything not named above is inherited, so a server-profile change is not
	// silently lost. Spot-check the values whose absence would be a real bug.
	if local.Maintenance.GCGrace != server.Maintenance.GCGrace {
		t.Error("gc grace should be inherited from the server profile")
	}
	if local.Catalog.BatchInterval != server.Catalog.BatchInterval ||
		local.Catalog.BatchSize != server.Catalog.BatchSize {
		t.Error("catalog batching should be inherited from the server profile")
	}
	if local.Upstream.RequestTimeout != server.Upstream.RequestTimeout {
		t.Error("upstream timeouts should be inherited from the server profile")
	}
	if local.Auth.SessionTTL != server.Auth.SessionTTL ||
		local.Auth.MaxJSONBytes != server.Auth.MaxJSONBytes {
		t.Error("auth bounds should be inherited from the server profile")
	}
}

func TestLocalDataDirIsPerUser(t *testing.T) {
	t.Setenv(LocalEnvPrefix+"DATA_DIR", "")
	t.Setenv("HOME", "/home/example")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("LOCALAPPDATA", "")

	dir, err := LocalDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir) != "pkgcache" {
		t.Fatalf("data dir %q does not end in pkgcache", dir)
	}
	if dir == DefaultDataDir {
		t.Fatal("local mode must not use the service state directory")
	}
	if runtime.GOOS == "linux" && dir != "/home/example/.local/share/pkgcache" {
		t.Fatalf("data dir = %q, want the XDG data location", dir)
	}
}

func TestLocalDataDirHonoursXDGAndTheExplicitOverride(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG layout is the Linux case")
	}
	t.Setenv(LocalEnvPrefix+"DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "/xdg")
	dir, err := LocalDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/xdg/pkgcache" {
		t.Fatalf("data dir = %q, want /xdg/pkgcache", dir)
	}

	t.Setenv(LocalEnvPrefix+"DATA_DIR", "/somewhere/else")
	dir, err = LocalDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/somewhere/else" {
		t.Fatalf("data dir = %q, want the explicit override", dir)
	}
}

// PKGREG_* belongs to a server somebody else runs. A developer inside a pkgreg-client
// shell has those set, and inheriting PKGREG_DATA_DIR would point this cache at that
// server's state directory.
func TestLoadLocalIgnoresServerEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", home)
	t.Setenv(LocalEnvPrefix+"DATA_DIR", "")
	t.Setenv("PKGREG_DATA_DIR", "/var/lib/somebody-elses-cache")
	t.Setenv("PKGREG_UNIFIED_ADDR", "0.0.0.0:8443")
	t.Setenv("PKGREG_HEADLESS", "true")

	snap, err := LoadLocal(LocalFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if snap.DataDir != filepath.Join(home, "pkgcache") {
		t.Fatalf("data dir = %q, want the per-user directory", snap.DataDir)
	}
	if snap.Server.UnifiedAddr != "127.0.0.1:41780" {
		t.Fatalf("address = %q, want the loopback default", snap.Server.UnifiedAddr)
	}
	if snap.Server.Headless {
		t.Fatal("a server's PKGREG_HEADLESS reached the local profile")
	}
}

func TestLoadLocalAddressForms(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", home)
	t.Setenv(LocalEnvPrefix+"DATA_DIR", "")

	t.Run("bare port", func(t *testing.T) {
		snap, err := LoadLocal(LocalFlags{Addr: "45000"})
		if err != nil {
			t.Fatal(err)
		}
		if snap.LocalAddr() != "127.0.0.1:45000" {
			t.Fatalf("address = %q", snap.LocalAddr())
		}
		if snap.LocalBaseURL() != "http://127.0.0.1:45000" {
			t.Fatalf("base URL = %q", snap.LocalBaseURL())
		}
	})

	t.Run("all three roles move together", func(t *testing.T) {
		snap, err := LoadLocal(LocalFlags{Addr: "127.0.0.1:45001"})
		if err != nil {
			t.Fatal(err)
		}
		if snap.Server.ProxyAddr != snap.Server.UnifiedAddr ||
			snap.Server.AdminAddr != snap.Server.UnifiedAddr {
			t.Fatalf("one port serves all three roles, but they differ: %+v", snap.Server)
		}
	})

	t.Run("routable is refused", func(t *testing.T) {
		if _, err := LoadLocal(LocalFlags{Addr: "0.0.0.0:45002"}); err == nil {
			t.Fatal("LoadLocal accepted a routable address")
		}
	})

	t.Run("nonsense is refused", func(t *testing.T) {
		if _, err := LoadLocal(LocalFlags{Addr: "not-a-port"}); err == nil {
			t.Fatal("LoadLocal accepted an unparseable address")
		}
	})
}

func TestLoadLocalEnvironmentOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", home)
	t.Setenv(LocalEnvPrefix+"DATA_DIR", "")
	t.Setenv(LocalEnvPrefix+"ADDR", "45100")
	t.Setenv(LocalEnvPrefix+"OFFLINE", "yes")
	t.Setenv(LocalEnvPrefix+"IDLE_TIMEOUT", "3m")

	snap, err := LoadLocal(LocalFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if snap.LocalAddr() != "127.0.0.1:45100" {
		t.Fatalf("address = %q", snap.LocalAddr())
	}
	if !snap.Upstream.Offline {
		t.Fatal("PKGCACHE_OFFLINE did not take effect")
	}
	if snap.Local.IdleTimeout != 3*time.Minute {
		t.Fatalf("idle timeout = %v, want 3m", snap.Local.IdleTimeout)
	}

	// A flag still beats the environment.
	idle := 9 * time.Minute
	snap, err = LoadLocal(LocalFlags{IdleTimeout: &idle})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Local.IdleTimeout != idle {
		t.Fatalf("idle timeout = %v, want the flag's %v", snap.Local.IdleTimeout, idle)
	}
}

// The server posture reading would report three findings here, every one of them noise:
// they describe what a stranger reaching the port could do, and Validate has already
// guaranteed there is no such stranger.
func TestLocalPostureIsInformationalOnly(t *testing.T) {
	s := LocalDefaults()
	s.DataDir = t.TempDir()
	issues := s.Posture(false)
	if len(issues) == 0 {
		t.Fatal("local posture said nothing at all")
	}
	worst, found := WorstSeverity(issues)
	if !found || worst != SeverityInfo {
		t.Fatalf("worst local severity = %v, want info", worst)
	}
	var sawSharedHost bool
	for _, issue := range issues {
		if issue.ID == "local_socket_unauthenticated" {
			sawSharedHost = true
		}
	}
	if !sawSharedHost {
		t.Error("local posture must state that the loopback socket is not per-user")
	}
}

// The same snapshot without the local marker must still get the server's reading, or
// a bug in the branch would silence real findings on a real server.
func TestServerPostureIsUnchanged(t *testing.T) {
	s := LocalDefaults()
	s.Local.Enabled = false
	s.Server.UnifiedAddr = "0.0.0.0:8443"
	issues := s.Posture(false)
	worst, found := WorstSeverity(issues)
	if !found || worst != SeverityCritical {
		t.Fatalf("worst severity = %v, want critical for an exposed unauthenticated server", worst)
	}
}
