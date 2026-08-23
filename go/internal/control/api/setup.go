package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
	"github.com/aabdlwahab/PKGCache/internal/onboarding"
)

type setupFormat string

const (
	setupShell      setupFormat = "sh"
	setupPowerShell setupFormat = "ps1"
)

func (a *API) getSetupShell(w http.ResponseWriter, r *http.Request) error {
	return a.serveSetup(w, r, setupShell)
}

func (a *API) getSetupPowerShell(w http.ResponseWriter, r *http.Request) error {
	return a.serveSetup(w, r, setupPowerShell)
}

func (a *API) serveSetup(w http.ResponseWriter, r *http.Request, format setupFormat) error {
	project := projectName(r)
	if _, _, err := a.requireView(r, project); err != nil {
		return err
	}
	caPEM, err := a.readCA()
	if err != nil {
		return err
	}
	host, unifiedPort, proxyPort, err := a.clientCoordinates(r)
	if err != nil {
		return err
	}
	cfg := onboarding.Config{
		Project: project, Host: host, UnifiedPort: unifiedPort,
		ProxyPort: proxyPort, CAPEM: caPEM,
	}
	var payload []byte
	switch format {
	case setupShell:
		payload, err = onboarding.Shell(cfg)
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	case setupPowerShell:
		payload, err = onboarding.PowerShell(cfg)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	default:
		return fmt.Errorf("unsupported setup script format %q", format)
	}
	if err != nil {
		return fmt.Errorf("generate setup script: %w", err)
	}
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="pkgreg-%s-setup.%s"`, project, format))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, err = w.Write(payload)
	return err
}

func (a *API) readCA() ([]byte, error) {
	if a.CAFile == "" {
		return nil, control.NewError(http.StatusNotFound,
			"not_found", "CA certificate is not available")
	}
	payload, err := os.ReadFile(filepath.Clean(a.CAFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, control.NewError(http.StatusNotFound,
			"not_found", "CA certificate is not available")
	}
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// caFingerprint is the colon-separated SHA-256 of this instance's CA, which every
// client pins against. One CA serves every project, so this takes no project argument
// and the public coordinates route can answer it.
func (a *API) caFingerprint() (string, error) {
	caPEM, err := a.readCA()
	if err != nil {
		return "", err
	}
	fingerprint, err := onboarding.FingerprintSHA256(caPEM)
	if err != nil {
		return "", fmt.Errorf("fingerprint CA certificate: %w", err)
	}
	return fingerprint, nil
}

func (a *API) onboardingJSON(project string) (map[string]string, error) {
	fingerprint, err := a.caFingerprint()
	if err != nil {
		return nil, err
	}
	base := "/api/v1/projects/" + url.PathEscape(project)
	return map[string]string{
		"ca_url":        "/api/ca.crt",
		"ca_sha256":     fingerprint,
		"setup_sh_url":  base + "/setup.sh",
		"setup_ps1_url": base + "/setup.ps1",
	}, nil
}

// clientCoordinates uses the pinned public origin when one exists. Otherwise the
// request host and configured listener ports match the same convention as the
// descriptor-backed endpoint panel.
func (a *API) clientCoordinates(
	r *http.Request,
) (host string, unifiedPort, proxyPort int, err error) {
	snapshot := a.Config.Current()
	unifiedPort = addressPort(snapshot.Server.UnifiedAddr)
	proxyPort = addressPort(snapshot.Server.ProxyAddr)

	if snapshot.Auth.PublicOrigin != "" {
		origin, err := url.Parse(snapshot.Auth.PublicOrigin)
		if err != nil || origin.Hostname() == "" {
			return "", 0, 0, fmt.Errorf("invalid public origin")
		}
		host = origin.Hostname()
		if snapshot.Server.SinglePort {
			if origin.Port() != "" {
				unifiedPort = addressPort(origin.Host)
			} else if origin.Scheme == "https" {
				unifiedPort = 443
			} else if origin.Scheme == "http" {
				unifiedPort = 80
			}
		}
	} else {
		var err error
		host, err = requestHostname(r.Host)
		if err != nil {
			return "", 0, 0, control.NewError(http.StatusBadRequest,
				"invalid_host", "invalid request host")
		}
	}
	if snapshot.Server.SinglePort {
		proxyPort = unifiedPort
	}
	return host, unifiedPort, proxyPort, nil
}

// clientScheme is the browser-facing scheme, which can differ from the local
// listener when a reverse proxy terminates TLS. PublicOrigin is the authoritative
// value; a trusted forwarded-proto header is the next best signal.
func clientScheme(r *http.Request, snapshot *config.Snapshot) string {
	if snapshot.Auth.PublicOrigin != "" {
		if origin, err := url.Parse(snapshot.Auth.PublicOrigin); err == nil {
			switch strings.ToLower(origin.Scheme) {
			case "http", "https":
				return strings.ToLower(origin.Scheme)
			}
		}
	}
	if r.TLS != nil {
		return "https"
	}
	if snapshot.Server.TrustProxy {
		forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
		switch strings.ToLower(forwarded) {
		case "http", "https":
			return strings.ToLower(forwarded)
		}
	}
	if snapshot.Server.TLS.Enabled() {
		return "https"
	}
	return "http"
}

func requestHostname(authority string) (string, error) {
	if authority == "" || strings.Contains(authority, "@") ||
		strings.ContainsAny(authority, "/\\\r\n\t ") {
		return "", fmt.Errorf("invalid authority")
	}
	if host, rawPort, err := net.SplitHostPort(authority); err == nil {
		host = strings.Trim(host, "[]")
		port, portErr := strconv.Atoi(rawPort)
		if onboarding.ValidHost(host) && portErr == nil && port > 0 && port <= 65535 {
			return host, nil
		}
		return "", fmt.Errorf("invalid host")
	}
	if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
		authority = strings.Trim(authority, "[]")
	}
	if !onboarding.ValidHost(authority) {
		return "", fmt.Errorf("invalid host")
	}
	return authority, nil
}
