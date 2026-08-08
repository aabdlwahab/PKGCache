package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pkgreg.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestDefaultsAreValid(t *testing.T) {
	s := Defaults()
	if err := s.Validate(); err != nil {
		t.Fatalf("shipped defaults do not validate: %v", err)
	}
}

// defaults < file < env < flags, and a layer that says nothing changes nothing.
func TestPrecedence(t *testing.T) {
	cfg := writeFile(t, `
data_dir: /from/file
log:
  level: warn
server:
  unified_addr: ":9443"
  admin_addr: ":9088"
`)
	t.Setenv("PKGREG_LOG_LEVEL", "error")
	t.Setenv("PKGREG_UNIFIED_ADDR", ":10443")

	flagAddr := ":11443"
	s, err := Load(Flags{ConfigFile: cfg, UnifiedAddr: &flagAddr})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if s.Server.UnifiedAddr != ":11443" {
		t.Errorf("flag should beat env and file: got %q", s.Server.UnifiedAddr)
	}
	if s.Log.Level != "error" {
		t.Errorf("env should beat file: got %q", s.Log.Level)
	}
	if s.Server.AdminAddr != ":9088" {
		t.Errorf("file should beat defaults: got %q", s.Server.AdminAddr)
	}
	if s.Server.ProxyAddr != ":3142" {
		t.Errorf("untouched default should survive: got %q", s.Server.ProxyAddr)
	}
	if !strings.HasSuffix(s.DataDir, "/from/file") {
		t.Errorf("data_dir = %q", s.DataDir)
	}
}

// `pkgreg init -data-dir X` then `pkgreg serve -data-dir X` must serve TLS. It did not:
// serve looked only at the configuration, found no certificate pair, and served
// cleartext with the pair sitting unused in X/certs. The console then advertised
// `-server http://…` and pkgreg-client rejected every such command with "server must use
// https", so the whole documented setup dead-ended on a server that had certificates.
func TestDataDirCertificatesAreAdoptedWithoutAConfigFile(t *testing.T) {
	dataDir := t.TempDir()
	certs := filepath.Join(dataDir, "certs")
	if err := os.MkdirAll(certs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ca.crt", "server.crt", "server.key"} {
		if err := os.WriteFile(filepath.Join(certs, name), []byte("pem"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	s, err := Load(Flags{DataDir: &dataDir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !s.Server.TLS.Enabled() {
		t.Fatalf("TLS not enabled: cert=%q key=%q", s.Server.TLS.CertFile, s.Server.TLS.KeyFile)
	}
	if s.Server.TLS.CertFile != filepath.Join(certs, "server.crt") ||
		s.Server.TLS.KeyFile != filepath.Join(certs, "server.key") ||
		s.Server.TLS.CAFile != filepath.Join(certs, "ca.crt") {
		t.Errorf("adopted the wrong paths: %+v", s.Server.TLS)
	}
}

// Adoption must not override an operator who named their own material, and must not
// invent a pair that does not exist — a host with no certificates still has to start.
func TestCertificateAdoptionYieldsToExplicitConfiguration(t *testing.T) {
	dataDir := t.TempDir()
	certs := filepath.Join(dataDir, "certs")
	if err := os.MkdirAll(certs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ca.crt", "server.crt", "server.key"} {
		if err := os.WriteFile(filepath.Join(certs, name), []byte("pem"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := writeFile(t, "data_dir: "+dataDir+`
server:
  tls:
    cert_file: /etc/ssl/mine.crt
    key_file: /etc/ssl/mine.key
`)
	s, err := Load(Flags{ConfigFile: cfg})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Server.TLS.CertFile != "/etc/ssl/mine.crt" || s.Server.TLS.KeyFile != "/etc/ssl/mine.key" {
		t.Errorf("explicit pair was overwritten: %+v", s.Server.TLS)
	}

	empty := t.TempDir()
	bare, err := Load(Flags{DataDir: &empty})
	if err != nil {
		t.Fatalf("Load with no certificates: %v", err)
	}
	if bare.Server.TLS.Enabled() || bare.Server.TLS.CAFile != "" {
		t.Errorf("invented TLS material that is not on disk: %+v", bare.Server.TLS)
	}
}

func TestUnknownConfigKeyIsAnError(t *testing.T) {
	cfg := writeFile(t, "data_dir: /tmp/x\nlgo:\n  level: info\n")
	_, err := Load(Flags{ConfigFile: cfg})
	if err == nil {
		t.Fatal("a misspelled key must fail at startup, not be silently ignored")
	}
	if !strings.Contains(err.Error(), "lgo") {
		t.Fatalf("error should name the offending key: %v", err)
	}
}

func TestMissingConfigFileIsAnError(t *testing.T) {
	if _, err := Load(Flags{ConfigFile: "/nonexistent/pkgreg.yaml"}); err == nil {
		t.Fatal("explicitly naming a missing file must fail")
	}
}

func TestEnvParsing(t *testing.T) {
	t.Setenv("PKGREG_DATA_DIR", t.TempDir())
	t.Setenv("PKGREG_OFFLINE", "yes") // operator spellings, not just Go's
	t.Setenv("PKGREG_TRUST_PROXY", "ON")
	t.Setenv("PKGREG_SINGLE_PORT", "0")
	t.Setenv("PKGREG_REQUEST_TIMEOUT", "45m")
	t.Setenv("PKGREG_BATCH_SIZE", "250")
	t.Setenv("PKGREG_GIT_REFS_TTL", "2m")
	t.Setenv("PKGREG_GIT_MAX_UPLOAD_PACKS", "12")
	t.Setenv("PKGREG_EVICT_TARGET_BYTES", "536870912000")
	t.Setenv("PKGREG_PROXY_ALLOWLIST", "archive.ubuntu.com, dl-cdn.alpinelinux.org ,")

	s, err := Load(Flags{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !s.Upstream.Offline || !s.Server.TrustProxy || s.Server.SinglePort {
		t.Fatalf("boolean parsing: offline=%v trust=%v single=%v",
			s.Upstream.Offline, s.Server.TrustProxy, s.Server.SinglePort)
	}
	if s.Upstream.RequestTimeout != 45*time.Minute {
		t.Fatalf("duration = %v", s.Upstream.RequestTimeout)
	}
	if s.Catalog.BatchSize != 250 {
		t.Fatalf("int = %d", s.Catalog.BatchSize)
	}
	if s.Git.RefsTTL != 2*time.Minute || s.Git.MaxUploadPacks != 12 {
		t.Fatalf("git config = %+v", s.Git)
	}
	if s.Maintenance.EvictTargetBytes != 536870912000 {
		t.Fatalf("int64 = %d", s.Maintenance.EvictTargetBytes)
	}
	if len(s.Server.ProxyAllowlist) != 2 {
		t.Fatalf("allowlist = %v", s.Server.ProxyAllowlist)
	}
}

func TestBadEnvValueIsAnError(t *testing.T) {
	t.Setenv("PKGREG_REQUEST_TIMEOUT", "not-a-duration")
	_, err := Load(Flags{})
	if err == nil {
		t.Fatal("a malformed duration must fail at startup")
	}
	if !strings.Contains(err.Error(), "REQUEST_TIMEOUT") {
		t.Fatalf("error should name the variable: %v", err)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		with func(*Snapshot)
		want string
	}{
		{"no data dir", func(s *Snapshot) { s.DataDir = "" }, "data_dir"},
		{"cert without key", func(s *Snapshot) { s.Server.TLS.CertFile = "c.pem" }, "together"},
		{"root user without password", func(s *Snapshot) { s.Auth.RootUser = "admin" }, "root_password"},
		{"zero session ttl", func(s *Snapshot) { s.Auth.SessionTTL = 0 }, "session_ttl"},
		{"zero git refs ttl", func(s *Snapshot) { s.Git.RefsTTL = 0 }, "git.refs_ttl"},
		{"zero git pack cap", func(s *Snapshot) { s.Git.MaxUploadPacks = 0 }, "git.max_upload_packs"},
		{"bad log format", func(s *Snapshot) { s.Log.Format = "xml" }, "log.format"},
		{"bad log level", func(s *Snapshot) { s.Log.Level = "chatty" }, "log.level"},
		{"gc without grace", func(s *Snapshot) {
			s.Maintenance.GCInterval = time.Hour
			s.Maintenance.GCGrace = 0
		}, "gc_grace"},
		{"bad data plane auth", func(s *Snapshot) {
			s.Projects = map[string]Project{"a": {DataPlaneAuth: "sometimes"}}
		}, "data_plane_auth"},
		{"multi-port without proxy addr", func(s *Snapshot) {
			s.Server.SinglePort = false
			s.Server.ProxyAddr = ""
		}, "proxy_addr"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Defaults()
			s.DataDir = "/tmp/pkgreg"
			c.with(&s)
			err := s.Validate()
			if err == nil {
				t.Fatalf("expected an error mentioning %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q should mention %q", err, c.want)
			}
		})
	}
}

func TestOfflineFor(t *testing.T) {
	s := Defaults()
	s.Projects = map[string]Project{
		"team-a": {Name: "team-a", Offline: true},
		"team-b": {Name: "team-b"},
	}
	if s.OfflineFor("team-b") || !s.OfflineFor("team-a") {
		t.Fatal("per-project soft flag not honoured")
	}
	if s.OfflineFor(GlobalProject) {
		t.Fatal("global should be online")
	}
	// The instance-wide hard mode overrides every project.
	s.Upstream.Offline = true
	for _, p := range []string{"team-a", "team-b", GlobalProject} {
		if !s.OfflineFor(p) {
			t.Fatalf("hard offline mode did not cover %q", p)
		}
	}
}

func TestHasProject(t *testing.T) {
	s := Defaults()
	s.Projects = map[string]Project{"team-a": {Name: "team-a"}}
	if !s.HasProject(GlobalProject) {
		t.Fatal("global is always present")
	}
	if !s.HasProject("team-a") || s.HasProject("nope") {
		t.Fatal("project lookup wrong")
	}
}

func TestStoreApplyIsAtomicAndValidated(t *testing.T) {
	base := Defaults()
	base.DataDir = "/tmp/pkgreg"
	st := NewStore(&base)

	before := st.Current()
	err := st.Apply(func(s *Snapshot) error {
		s.Projects["team-a"] = Project{Name: "team-a"}
		s.Auth.SessionTTL = 0 // makes the snapshot invalid
		return nil
	})
	if err == nil {
		t.Fatal("Apply must reject an invalid result")
	}
	if st.Current() != before {
		t.Fatal("a rejected Apply must not publish anything")
	}
	if _, leaked := before.Projects["team-a"]; leaked {
		t.Fatal("Apply mutated the live snapshot instead of a copy")
	}

	if err := st.Apply(func(s *Snapshot) error {
		s.Projects["team-a"] = Project{Name: "team-a"}
		return nil
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !st.Current().HasProject("team-a") {
		t.Fatal("valid Apply did not publish")
	}
	if _, leaked := before.Projects["team-a"]; leaked {
		t.Fatal("the previous snapshot was mutated — it must stay immutable")
	}
}

func TestStoreObservers(t *testing.T) {
	base := Defaults()
	base.DataDir = "/tmp/pkgreg"
	st := NewStore(&base)

	var got []string
	st.Observe(func(s *Snapshot) { got = append(got, s.Log.Level) })
	if err := st.Apply(func(s *Snapshot) error { s.Log.Level = "debug"; return nil }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := st.Apply(func(s *Snapshot) error { return errBoom }); err == nil {
		t.Fatal("expected the mutation error to propagate")
	}
	if len(got) != 1 || got[0] != "debug" {
		t.Fatalf("observers = %v, want exactly one call with debug", got)
	}
}

var errBoom = errTest("boom")

type errTest string

func (e errTest) Error() string { return string(e) }

// Readers must never tear or race against a concurrent swap.
func TestStoreConcurrentReadWrite(t *testing.T) {
	base := Defaults()
	base.DataDir = "/tmp/pkgreg"
	st := NewStore(&base)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := st.Current()
				_ = s.HasProject("team-a")
				_ = s.OfflineFor("team-a")
			}
		}()
	}
	for i := range 200 {
		if err := st.Apply(func(s *Snapshot) error {
			s.Projects["team-a"] = Project{Name: "team-a", Offline: i%2 == 0}
			return nil
		}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestSetProjects(t *testing.T) {
	base := Defaults()
	base.DataDir = "/tmp/pkgreg"
	st := NewStore(&base)

	src := map[string]Project{"a": {Name: "a"}}
	if err := st.SetProjects(src); err != nil {
		t.Fatalf("SetProjects: %v", err)
	}
	// The store must hold a copy: mutating the caller's map cannot affect it.
	src["b"] = Project{Name: "b"}
	if st.Current().HasProject("b") {
		t.Fatal("SetProjects aliased the caller's map")
	}
	if err := st.SetProjects(nil); err != nil {
		t.Fatalf("SetProjects(nil): %v", err)
	}
	if st.Current().Projects == nil {
		t.Fatal("Projects must never be nil after SetProjects")
	}
}

func TestDerivedPaths(t *testing.T) {
	s := Defaults()
	s.DataDir = t.TempDir()
	if err := s.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	for _, p := range []string{
		filepath.Dir(s.CatalogPath()), filepath.Dir(s.ControlPath()),
		s.CertsDir(), filepath.Join(s.ShuttleDir(), "in"), filepath.Join(s.ShuttleDir(), "out"),
	} {
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			t.Fatalf("missing directory %s: %v", p, err)
		}
	}
	if err := s.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs must be idempotent: %v", err)
	}
}
