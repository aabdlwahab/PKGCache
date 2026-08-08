package clientbridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// SessionOptions starts a loopback bridge and an interactive child shell whose
// environment points package tools at it. Exiting that shell restores the parent
// terminal unchanged.
type SessionOptions struct {
	Server          string
	Project         string
	CAPEM           []byte
	CAFingerprint   string
	AptProxy        string
	Token           string
	Shell           string
	OperatingSystem string
	Environment     []string
	Stdin           io.Reader
	Stdout          io.Writer
	Stderr          io.Writer
	CommandContext  func(context.Context, string, ...string) *exec.Cmd
}

// Session runs a temporary pkgreg environment. It writes no files, installs no
// certificate, and binds only to an ephemeral IPv4 loopback port.
func Session(ctx context.Context, options SessionOptions) error {
	target, err := parseServer(options.Server)
	if err != nil {
		return err
	}
	if options.Project == "" {
		options.Project = "global"
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(options.CAPEM) {
		return errors.New("verified CA contains no usable PEM certificate")
	}
	tlsConfig := &tls.Config{ // #nosec G402 -- the caller supplies a fingerprint-verified CA.
		ServerName: target.Hostname(), MinVersion: tls.VersionTLS12, RootCAs: roots,
	}
	upstream := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			TLSClientConfig:       tlsConfig,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   16,
		},
	}

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start temporary loopback bridge: %w", err)
	}
	local := listener.Addr().String()
	handler := &bridge{
		target: target,
		local:  local,
		token:  options.Token,
		client: upstream,
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}
	serverErrors := make(chan error, 1)
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil &&
			!errors.Is(serveErr, http.ErrServerClosed) {
			serverErrors <- serveErr
		}
	}()

	sessionCtx, stopSession := context.WithCancel(ctx)
	defer stopSession()
	program, args, err := sessionShell(options)
	if err != nil {
		_ = listener.Close()
		return err
	}
	commandContext := options.CommandContext
	if commandContext == nil {
		commandContext = exec.CommandContext
	}
	cmd := commandContext(sessionCtx, program, args...)
	cmd.Stdin = options.Stdin
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = options.Stdout
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	cmd.Stderr = options.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	environment := options.Environment
	if environment == nil {
		environment = os.Environ()
	}
	cmd.Env = sessionEnvironment(environment, local, options)

	stdout := options.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	_, _ = fmt.Fprintf(stdout, `pkgreg-client: temporary session ready
  project: %s
  bridge:  http://%s

Run package commands in the shell below. Type exit to return to your previous
environment; no certificate or configuration is installed on this machine.

`, options.Project, local)

	if err := cmd.Start(); err != nil {
		_ = listener.Close()
		return fmt.Errorf("start temporary shell: %w", err)
	}
	shellDone := make(chan error, 1)
	go func() { shellDone <- cmd.Wait() }()

	var sessionErr error
	select {
	case sessionErr = <-shellDone:
	case serveErr := <-serverErrors:
		sessionErr = fmt.Errorf("temporary bridge failed: %w", serveErr)
		stopSession()
		<-shellDone
	case <-ctx.Done():
		stopSession()
		<-shellDone
		sessionErr = ctx.Err()
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	_ = server.Shutdown(shutdownCtx)
	if sessionErr != nil {
		return fmt.Errorf("temporary session: %w", sessionErr)
	}
	_, _ = fmt.Fprintln(stdout, "pkgreg-client: temporary session ended; previous settings restored")
	return nil
}

func sessionShell(options SessionOptions) (string, []string, error) {
	goos := options.OperatingSystem
	if goos == "" {
		goos = runtime.GOOS
	}
	switch goos {
	case "linux", "darwin":
		shell := options.Shell
		if shell == "" {
			shell = strings.TrimSpace(os.Getenv("SHELL"))
		}
		if shell == "" {
			shell = "/bin/sh"
		}
		return shell, []string{"-i"}, nil
	case "windows":
		shell := options.Shell
		if shell == "" {
			shell = "powershell.exe"
		}
		return shell, []string{"-NoLogo"}, nil
	default:
		return "", nil, fmt.Errorf("unsupported operating system %q", goos)
	}
}

func sessionEnvironment(base []string, local string, options SessionOptions) []string {
	remove := map[string]bool{
		"PKGREG_SESSION":         true,
		"PKGREG_SERVER":          true,
		"PKGREG_PROJECT":         true,
		"PKGREG_CA_FILE":         true,
		"PKGREG_CA_SHA256":       true,
		"PKGREG_BRIDGE_URL":      true,
		"PKGREG_GIT_URL":         true,
		"PKGREG_APT_PROXY":       true,
		"PKGREG_FILES_URL":       true,
		"PKGREG_DOCKER_REGISTRY": true,
		"PIP_CERT":               true,
		"PIP_INDEX_URL":          true,
		"UV_NATIVE_TLS":          true,
		"UV_DEFAULT_INDEX":       true,
		"NODE_EXTRA_CA_CERTS":    true,
		"NPM_CONFIG_CAFILE":      true,
		"NPM_CONFIG_REGISTRY":    true,
		"GIT_SSL_CAINFO":         true,
		"NO_PROXY":               true,
		"no_proxy":               true,
	}
	out := make([]string, 0, len(base)+14)
	noProxy := ""
	for _, entry := range base {
		key, value, found := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if upper == "NO_PROXY" && noProxy == "" {
			noProxy = value
		}
		if found && remove[upper] {
			continue
		}
		out = append(out, entry)
	}
	noProxy = appendNoProxy(noProxy, "127.0.0.1", "localhost")
	bridgeURL := "http://" + local
	projectBase := bridgeURL + "/" + options.Project
	values := [][2]string{
		{"PKGREG_SESSION", "temporary"},
		{"PKGREG_SERVER", options.Server},
		{"PKGREG_PROJECT", options.Project},
		{"PKGREG_CA_SHA256", options.CAFingerprint},
		{"PKGREG_BRIDGE_URL", bridgeURL},
		{"PKGREG_DOCKER_REGISTRY", local},
		{"PKGREG_GIT_URL", projectBase + "/git"},
		{"PKGREG_FILES_URL", projectBase + "/files/"},
		{"PIP_INDEX_URL", projectBase + "/pypi/root/pypi/+simple/"},
		{"UV_DEFAULT_INDEX", projectBase + "/pypi/root/pypi/+simple/"},
		{"NPM_CONFIG_REGISTRY", projectBase + "/npm/"},
		{"NO_PROXY", noProxy},
		{"no_proxy", noProxy},
	}
	if options.AptProxy != "" {
		values = append(values, [2]string{"PKGREG_APT_PROXY", options.AptProxy})
	}
	for _, value := range values {
		if value[1] != "" {
			out = append(out, value[0]+"="+value[1])
		}
	}
	return out
}

func appendNoProxy(value string, hosts ...string) string {
	parts := strings.Split(value, ",")
	seen := make(map[string]bool, len(parts)+len(hosts))
	clean := make([]string, 0, len(parts)+len(hosts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		clean = append(clean, part)
	}
	for _, host := range hosts {
		if !seen[host] {
			seen[host] = true
			clean = append(clean, host)
		}
	}
	return strings.Join(clean, ",")
}
