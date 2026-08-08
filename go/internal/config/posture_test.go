package config

import (
	"strings"
	"testing"
)

func TestReachableOffHost(t *testing.T) {
	cases := []struct {
		address string
		want    bool
	}{
		{"", true},
		{":8443", true},
		{"0.0.0.0:8443", true},
		{"[::]:8443", true},
		{"127.0.0.1:8443", false},
		{"[::1]:8443", false},
		{"localhost:8443", false},
		{"LocalHost:8443", false},
		{"127.0.0.1", false},
		{"10.0.0.5:8443", true},
		{"cache.internal:8443", true},
		// Unparseable addresses report reachable: guessing "safe" about an address we
		// do not understand is how a warning goes missing on the host that needed it.
		{"not a real address", true},
	}
	for _, tc := range cases {
		if got := reachableOffHost(tc.address); got != tc.want {
			t.Errorf("reachableOffHost(%q) = %v, want %v", tc.address, got, tc.want)
		}
	}
}

// ids reduces a posture report to the finding identifiers, so a test asserts on what
// was found rather than on prose that is expected to be edited.
func ids(issues []PostureIssue) []string {
	out := make([]string, 0, len(issues))
	for _, issue := range issues {
		out = append(out, issue.ID)
	}
	return out
}

func has(issues []PostureIssue, id string) bool {
	for _, issue := range issues {
		if issue.ID == id {
			return true
		}
	}
	return false
}

// TestDefaultConfigurationIsReportedUnsafe is the regression test for the audit's
// central finding: `pkgreg init` followed by `pkgreg serve` produced an instance that
// answered anonymous control-plane writes and relayed arbitrary HTTP, and every
// diagnostic called it healthy.
func TestDefaultConfigurationIsReportedUnsafe(t *testing.T) {
	snapshot := Defaults()
	issues := snapshot.Posture(false)

	if !has(issues, "auth_disabled") {
		t.Errorf("default config with no accounts did not report auth_disabled; got %v",
			ids(issues))
	}
	if !has(issues, "open_proxy") {
		t.Errorf("default config with an empty allowlist did not report open_proxy; got %v",
			ids(issues))
	}
	worst, found := WorstSeverity(issues)
	if !found || worst != SeverityCritical {
		t.Errorf("default posture worst severity = %v (found=%v), want critical", worst, found)
	}
	for _, issue := range issues {
		if strings.TrimSpace(issue.Remedy) == "" {
			t.Errorf("%s has no remedy; a warning nobody can act on is noise", issue.ID)
		}
	}
}

func TestAuthenticatedInstanceDropsTheAuthFinding(t *testing.T) {
	snapshot := Defaults()
	if has(snapshot.Posture(true), "auth_disabled") {
		t.Error("auth_disabled reported for an instance with accounts")
	}
}

// TestLoopbackDowngradesRatherThanSilences: binding to loopback is a legitimate way to
// run without authentication, but it must still be visible — a host that is safe today
// because of where it binds is one configuration edit from not being.
func TestLoopbackDowngradesRatherThanSilences(t *testing.T) {
	snapshot := Defaults()
	snapshot.Server.UnifiedAddr = "127.0.0.1:8443"

	issues := snapshot.Posture(false)
	if has(issues, "auth_disabled") || has(issues, "open_proxy") {
		t.Errorf("loopback instance reported network-exposure findings: %v", ids(issues))
	}
	if !has(issues, "auth_disabled_loopback") || !has(issues, "open_proxy_loopback") {
		t.Errorf("loopback instance went silent instead of downgrading: %v", ids(issues))
	}
	if worst, _ := WorstSeverity(issues); worst != SeverityWarn {
		t.Errorf("loopback worst severity = %v, want warn", worst)
	}
}

func TestConfiguredAllowlistClearsTheProxyFinding(t *testing.T) {
	snapshot := Defaults()
	snapshot.Server.ProxyAllowlist = []string{"archive.ubuntu.com"}
	issues := snapshot.Posture(true)
	if len(issues) != 0 && !has(issues, "cleartext_admin") {
		t.Errorf("a restricted allowlist still reported %v", ids(issues))
	}
	if has(issues, "open_proxy") {
		t.Error("open_proxy reported for a configured allowlist")
	}
}

// TestExplicitRelayAnywhereIsInformationalNotCritical: "*" is how an operator says they
// know. Treating a deliberate decision as a critical finding is how findings get
// ignored, so it downgrades to info — but it never disappears.
func TestExplicitRelayAnywhereIsInformationalNotCritical(t *testing.T) {
	snapshot := Defaults()
	snapshot.Server.ProxyAllowlist = []string{ProxyRelaysAnywhere}
	issues := snapshot.Posture(true)

	if !has(issues, "proxy_relays_anywhere") {
		t.Errorf(`["*"] produced no finding at all: %v`, ids(issues))
	}
	if has(issues, "open_proxy") {
		t.Error(`["*"] still reported as an unacknowledged open proxy`)
	}
	for _, issue := range issues {
		if issue.ID == "proxy_relays_anywhere" && issue.Severity != SeverityInfo {
			t.Errorf("explicit opt-in severity = %v, want info", issue.Severity)
		}
	}
}

func TestAllowsAnyProxyHost(t *testing.T) {
	cases := []struct {
		list []string
		want bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{"archive.ubuntu.com"}, false},
		{[]string{"*"}, true},
		{[]string{" * "}, true},
		{[]string{"archive.ubuntu.com", "*"}, true},
		// "*.example.com" is a subdomain rule, not the relay-anywhere opt-in.
		{[]string{"*.example.com"}, false},
	}
	for _, tc := range cases {
		server := Server{ProxyAllowlist: tc.list}
		if got := server.AllowsAnyProxyHost(); got != tc.want {
			t.Errorf("AllowsAnyProxyHost(%v) = %v, want %v", tc.list, got, tc.want)
		}
	}
}

func TestCleartextAdminFindingRespectsPublicOrigin(t *testing.T) {
	snapshot := Defaults()
	snapshot.Server.ProxyAllowlist = []string{"archive.ubuntu.com"}
	if !has(snapshot.Posture(true), "cleartext_admin") {
		t.Error("no TLS and no public origin did not report cleartext_admin")
	}

	// TLS terminating in front is a supported deployment, not a finding.
	snapshot.Auth.PublicOrigin = "https://cache.example.com"
	if has(snapshot.Posture(true), "cleartext_admin") {
		t.Error("cleartext_admin reported despite an https public origin")
	}
}
