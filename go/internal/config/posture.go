package config

import (
	"net"
	"strings"
)

// Posture answers one question: if this configuration were served on this network,
// what could a stranger who can reach the port do?
//
// It lives in config because both `serve` and `doctor` must answer it identically.
// They did not: doctor reported a fresh instance "healthy" while serve was in fact
// exposing an unauthenticated control plane and an open HTTP relay on every interface.
// Two readings of the same fields is one reading too many, so there is now exactly one,
// and the two commands differ only in what they do with the answer — serve warns and
// keeps running, doctor fails.

// Severity ranks a posture finding.
type Severity int

const (
	// SeverityInfo records a deliberate choice worth stating out loud.
	SeverityInfo Severity = iota
	// SeverityWarn is a weak configuration that is not immediately exploitable.
	SeverityWarn
	// SeverityCritical means an unauthenticated stranger on this network can abuse
	// this instance right now.
	SeverityCritical
)

// String renders a severity for logs and reports.
func (s Severity) String() string {
	switch s {
	case SeverityCritical:
		return "critical"
	case SeverityWarn:
		return "warn"
	default:
		return "info"
	}
}

// PostureIssue is one finding, with the remedy attached. The remedy is part of the
// finding on purpose: a warning an operator cannot act on immediately is a warning they
// learn to scroll past.
type PostureIssue struct {
	ID       string
	Severity Severity
	Summary  string
	Remedy   string
}

// ProxyRelaysAnywhere is the explicit opt-in that restores relay-to-any-host.
//
// It exists so that "I know, and I mean it" is expressible. Without it the only way to
// keep the historical behaviour is to leave the allowlist empty, which is
// indistinguishable from never having thought about it — and a warning that cannot be
// silenced by a deliberate decision is a warning that gets ignored.
const ProxyRelaysAnywhere = "*"

// AllowsAnyProxyHost reports whether the allowlist explicitly opts into relaying
// anywhere.
func (s *Server) AllowsAnyProxyHost() bool {
	for _, entry := range s.ProxyAllowlist {
		if strings.TrimSpace(entry) == ProxyRelaysAnywhere {
			return true
		}
	}
	return false
}

// reachableOffHost reports whether a listener address accepts connections from other
// machines. An empty host, "0.0.0.0" or "::" is every interface; anything that resolves
// only to loopback is not reachable from off-box.
//
// Unparseable or unresolvable addresses report true. Guessing "safe" about an address
// we do not understand is how a warning goes missing on the one host that needed it.
func reachableOffHost(address string) bool {
	if strings.TrimSpace(address) == "" {
		return true
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		// No port at all, or a malformed pair: treat the whole string as the host.
		host = address
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" || host == "0.0.0.0" || host == "::" || host == "*" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback()
	}
	if strings.EqualFold(host, "localhost") {
		return false
	}
	// A named host that is not "localhost" is presumed routable. Resolving it here
	// would make a diagnostic depend on DNS being up.
	return true
}

// AdminReachableOffHost reports whether the console and control API accept connections
// from other machines.
func (s *Snapshot) AdminReachableOffHost() bool {
	if s.Server.SinglePort {
		return reachableOffHost(s.Server.UnifiedAddr)
	}
	return reachableOffHost(s.Server.AdminAddr)
}

// ProxyReachableOffHost reports whether the apt/apk forward proxy accepts connections
// from other machines.
func (s *Snapshot) ProxyReachableOffHost() bool {
	if s.Server.SinglePort {
		return reachableOffHost(s.Server.UnifiedAddr)
	}
	return reachableOffHost(s.Server.ProxyAddr)
}

// Posture lists everything about this configuration that weakens the instance,
// most severe first.
//
// authEnabled comes from the caller because it depends on stored accounts, which live
// in the control database rather than in configuration — this package deliberately
// knows nothing about storage.
func (s *Snapshot) Posture(authEnabled bool) []PostureIssue {
	if s.Local.Enabled {
		return s.localPosture()
	}
	var issues []PostureIssue

	if !authEnabled {
		if s.AdminReachableOffHost() {
			issues = append(issues, PostureIssue{
				ID:       "auth_disabled",
				Severity: SeverityCritical,
				Summary: "control-plane authentication is off and the console and API " +
					"are reachable from other machines: any caller can create projects, " +
					"mint tokens, read upstream credentials and run maintenance",
				Remedy: "run `pkgreg init` to provision a superuser, or bind the admin " +
					"listener to 127.0.0.1",
			})
		} else {
			issues = append(issues, PostureIssue{
				ID:       "auth_disabled_loopback",
				Severity: SeverityWarn,
				Summary:  "control-plane authentication is off, but only loopback can reach it",
				Remedy:   "run `pkgreg init` before exposing this instance to a network",
			})
		}
	}

	switch {
	case len(s.Server.ProxyAllowlist) == 0 && s.ProxyReachableOffHost():
		issues = append(issues, PostureIssue{
			ID:       "open_proxy",
			Severity: SeverityCritical,
			Summary: "server.proxy_allowlist is empty, so the apt/apk forward proxy will " +
				"fetch any http:// URL for any caller that can reach it — including " +
				"addresses only this host can route to, such as cloud instance metadata",
			Remedy: "list the repositories you mirror in server.proxy_allowlist, or set " +
				`it to ["*"] to accept relaying anywhere as a deliberate choice`,
		})
	case len(s.Server.ProxyAllowlist) == 0:
		issues = append(issues, PostureIssue{
			ID:       "open_proxy_loopback",
			Severity: SeverityWarn,
			Summary:  "server.proxy_allowlist is empty; only loopback can reach the proxy",
			Remedy:   "set server.proxy_allowlist before exposing this instance to a network",
		})
	case s.Server.AllowsAnyProxyHost():
		issues = append(issues, PostureIssue{
			ID:       "proxy_relays_anywhere",
			Severity: SeverityInfo,
			Summary:  `server.proxy_allowlist is ["*"]: this host will relay plaintext HTTP anywhere`,
			Remedy:   "replace it with the repositories you actually mirror when you can",
		})
	}

	// No certificate pair at all means the console, the control API and every package
	// response cross the network in the clear. Distinct from the single-port cleartext
	// branch, which is now proxy-only — this is the case where there is no TLS to
	// redirect to.
	if !s.Server.TLS.Enabled() && s.AdminReachableOffHost() &&
		!strings.HasPrefix(strings.ToLower(s.Auth.PublicOrigin), "https://") {
		issues = append(issues, PostureIssue{
			ID:       "cleartext_admin",
			Severity: SeverityWarn,
			Summary: "no TLS certificate is configured, so console logins and API traffic " +
				"cross the network in the clear",
			Remedy: "run `pkgreg init` to mint a certificate, or set auth.public_origin " +
				"when TLS terminates in front of this process",
		})
	}

	return issues
}

// localPosture reads pkgcache's posture, which is a different question from a
// server's.
//
// The server reading would report three findings here — no authentication, an open
// relay, cleartext admin — and every one of them would be noise. They describe what a
// stranger who can reach the port could do, and in local mode Validate has already
// guaranteed there is no such stranger: an address another machine can reach is a
// startup error, not a warning.
//
// What is left is worth saying exactly once, at Info, because it is the one property a
// reader might reasonably have assumed differently.
func (s *Snapshot) localPosture() []PostureIssue {
	issues := []PostureIssue{{
		ID:       "local_mode",
		Severity: SeverityInfo,
		Summary: "local mode: bound to " + s.LocalAddr() + ", with no certificate and no " +
			"accounts, and refusing to bind an address other machines can reach",
	}}
	// Binding to loopback limits the network, not the machine. On a shared host every
	// local user can drive this daemon and spend this cache. The data directory is
	// created 0700 so its contents stay private, but the socket in front of it is not
	// per-user and this must not be presented as isolation it does not provide.
	issues = append(issues, PostureIssue{
		ID:       "local_socket_unauthenticated",
		Severity: SeverityInfo,
		Summary: "a loopback socket is not per-user: any local account on this machine " +
			"can use this cache",
		Remedy: "on a shared or multi-user host, run `pkgreg serve` with tokens instead",
	})
	return issues
}

// WorstSeverity reports the highest severity present, and whether there was anything.
func WorstSeverity(issues []PostureIssue) (Severity, bool) {
	worst, found := SeverityInfo, false
	for _, issue := range issues {
		if !found || issue.Severity > worst {
			worst, found = issue.Severity, true
		}
	}
	return worst, found
}
