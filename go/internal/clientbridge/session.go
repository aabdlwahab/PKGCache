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
	"time"

	"github.com/aabdlwahab/PKGCache/internal/session"
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

// Running is a bridge that is listening, and the way to stop it.
type Running struct {
	// Addr is the loopback address tools are pointed at, host:port.
	Addr string
	// Errors reports a listener failure. Nothing is sent on a clean shutdown.
	Errors <-chan error

	server   *http.Server
	listener net.Listener
}

// Close stops the bridge, allowing in-flight requests a moment to finish.
func (r *Running) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.server.Shutdown(ctx)
}

// Start binds the loopback bridge and serves it until Close.
//
// Split out of Session so that a caller can run something other than a shell against
// it. pkgcache does exactly that: with local caching turned off it is this bridge and
// nothing else, which is what makes "the old client's behaviour" a mode of the merged
// program rather than a second program.
func Start(ctx context.Context, options SessionOptions) (*Running, error) {
	target, err := parseServer(options.Server)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(options.CAPEM) {
		return nil, errors.New("verified CA contains no usable PEM certificate")
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
		return nil, fmt.Errorf("start temporary loopback bridge: %w", err)
	}
	local := listener.Addr().String()
	server := &http.Server{
		Handler: &bridge{
			target: target, local: local, token: options.Token, client: upstream,
		},
		ReadHeaderTimeout: 30 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil &&
			!errors.Is(serveErr, http.ErrServerClosed) {
			serverErrors <- serveErr
		}
	}()
	return &Running{
		Addr: local, Errors: serverErrors, server: server, listener: listener,
	}, nil
}

// Session runs a temporary pkgreg environment. It writes no files, installs no
// certificate, and binds only to an ephemeral IPv4 loopback port.
func Session(ctx context.Context, options SessionOptions) error {
	if options.Project == "" {
		options.Project = "global"
	}
	running, err := Start(ctx, options)
	if err != nil {
		return err
	}
	local := running.Addr
	serverErrors := running.Errors
	listener := running.listener
	server := running.server

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
	_ = server.Shutdown(shutdownCtx) //nolint:contextcheck // detached on purpose: the session context is already cancelled here
	if sessionErr != nil {
		return fmt.Errorf("temporary session: %w", sessionErr)
	}
	_, _ = fmt.Fprintln(stdout, "pkgreg-client: temporary session ended; previous settings restored")
	return nil
}

// sessionShell and sessionEnvironment delegate to internal/session, which pkgcache
// shares. Which variable each tool reads is the same knowledge in both programs; what
// differs is only the base URL and the namespace.
func sessionShell(options SessionOptions) (shell string, args []string, err error) {
	return session.Shell(options.Shell, options.OperatingSystem)
}

func sessionEnvironment(base []string, local string, options SessionOptions) []string {
	return session.Environment(base, session.Options{
		Prefix:         session.PkgregPrefix,
		Kind:           "temporary",
		BaseURL:        "http://" + local,
		Project:        options.Project,
		AptProxy:       options.AptProxy,
		DockerRegistry: local,
		// Deliberately no GitHosts. The bridge could redirect clones the way pkgcache
		// does, but doing it here would change what an installed pkgreg-client does to
		// a user's git configuration, and that belongs in its own release rather than
		// arriving as a side effect of sharing code.
		Extra: [][2]string{
			{session.PkgregPrefix + "SERVER", options.Server},
			{session.PkgregPrefix + "CA_SHA256", options.CAFingerprint},
		},
	})
}
