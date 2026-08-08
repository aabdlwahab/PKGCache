// Package clientinstaller verifies pkgreg trust, opens temporary package sessions,
// and explicitly applies persistent machine onboarding.
package clientinstaller

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/brightskies/pkgreg/internal/clientbridge"
	"github.com/brightskies/pkgreg/internal/onboarding"
)

const (
	maxCABytes     = 1 << 20
	maxScriptBytes = 4 << 20
)

// Options controls download verification and installer behavior.
type Options struct {
	Server         string
	Project        string
	ExpectedSHA256 string
	CAFile         string
	CookieFile     string
	TokenFile      string
	Host           string
	CacheIP        string
	Shell          string
	Persist        bool
	DockerTrust    bool
	// DockerBuildTrust installs the build-time proxy into the Docker client's own
	// configuration, so apt and apk inside any build on this machine reach the cache.
	DockerBuildTrust  bool
	DryRun            bool
	Uninstall         bool
	Print             bool
	Client            *http.Client
	Stdout            io.Writer
	Stderr            io.Writer
	OperatingSystem   string
	CommandContext    func(context.Context, string, ...string) *exec.Cmd
	KeepTemporaryFile bool
}

// Bundle is a verified platform-specific setup script.
type Bundle struct {
	Script      []byte
	Extension   string
	Fingerprint string
}

// Run starts a temporary configured shell by default. Persist explicitly selects the
// machine-wide setup script path, and DockerTrust the one-file Docker-only path.
func Run(ctx context.Context, options Options) error {
	if options.DockerBuildTrust {
		if options.DockerTrust || options.Persist {
			return errors.New(
				"-docker-build-trust configures builds through the cache's proxy; " +
					"-docker-trust and --persist install certificates — run them separately")
		}
		return buildTrust(options)
	}
	if options.DockerTrust {
		if options.Persist {
			return errors.New(
				"-docker-trust installs one certificate for Docker; --persist configures the " +
					"whole machine including that certificate — choose one")
		}
		return dockerTrust(ctx, options)
	}
	if !options.Persist {
		switch {
		case options.DryRun:
			return errors.New("-dry-run changes only persistent setup; use it with -persist")
		case options.Uninstall:
			return errors.New("-uninstall changes only persistent setup; use it with -persist")
		case options.Print:
			return errors.New("-print shows the persistent setup script; use it with -persist")
		case options.Host != "":
			return errors.New("-host changes persistent machine setup; use it with -persist")
		case options.CacheIP != "":
			return errors.New("-cache-ip changes persistent machine setup; use it with -persist")
		}
		trust, err := fetchTrust(ctx, options)
		if err != nil {
			return err
		}
		aptProxy, err := fetchAptProxy(
			ctx, trust.client, trust.base, trust.cookie, trust.project)
		if err != nil {
			return err
		}
		token, err := readProjectToken(options.TokenFile)
		if err != nil {
			return err
		}
		return clientbridge.Session(ctx, clientbridge.SessionOptions{
			Server: trust.base.String(), Project: trust.project,
			CAPEM: trust.caPEM, CAFingerprint: trust.fingerprint,
			AptProxy: aptProxy, Token: token, Shell: options.Shell,
			OperatingSystem: options.OperatingSystem,
			Stdout:          options.Stdout, Stderr: options.Stderr,
			CommandContext: options.CommandContext,
		})
	}
	if options.TokenFile != "" {
		return errors.New("-token-file is for temporary bridge sessions; persistent setup cannot store a project token")
	}

	bundle, err := Fetch(ctx, options)
	if err != nil {
		return err
	}
	stdout := options.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	if options.Print {
		_, err := stdout.Write(bundle.Script)
		return err
	}
	return Execute(ctx, bundle, options)
}

func readProjectToken(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	file, err := os.Open(name)
	if err != nil {
		return "", fmt.Errorf("read project token file: %w", err)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, (16<<10)+1))
	if err != nil {
		return "", fmt.Errorf("read project token file: %w", err)
	}
	if len(body) > 16<<10 {
		return "", errors.New("project token file is too large")
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		return "", errors.New("project token file is empty")
	}
	if strings.ContainsAny(token, "\r\n") {
		return "", errors.New("project token file must contain exactly one token")
	}
	return token, nil
}

type verifiedTrust struct {
	base        *url.URL
	project     string
	cookie      string
	caPEM       []byte
	fingerprint string
	client      *http.Client
}

// Fetch downloads the public CA, pins it to the operator's out-of-band
// fingerprint, and then downloads the platform script over TLS verified by that
// CA. The first request may be made without normal certificate verification
// because its only output is public CA material that is verified byte-for-byte
// before it is trusted. The setup script never travels over that bootstrap
// connection.
func Fetch(ctx context.Context, options Options) (Bundle, error) {
	trust, err := fetchTrust(ctx, options)
	if err != nil {
		return Bundle{}, err
	}

	goos := options.OperatingSystem
	if goos == "" {
		goos = runtime.GOOS
	}
	extension := "sh"
	if goos == "windows" {
		extension = "ps1"
	}
	scriptURL := *trust.base
	scriptURL.Path = path.Join(
		trust.base.Path, "/api/v1/projects", trust.project, "setup."+extension)
	script, contentType, err := download(
		ctx, trust.client, scriptURL.String(), trust.cookie, maxScriptBytes)
	if isUnauthorized(err) && trust.cookie == "" {
		// The instance enforces control-plane authentication and this caller supplied
		// no session. Rather than fail — which is what --persist did on every
		// authenticated instance, making the CI and shared-host path unusable exactly
		// where it matters most — ask for the read-only guest session the server
		// offers for this purpose and retry once.
		//
		// Safe because of what this script is: the public CA plus this instance's
		// addresses, and nothing else. The server keeps setup.sh on the guest
		// allowlist for the same reason, and the fingerprint check below still has to
		// pass, so a substituted script is caught whether it arrived as a guest or as
		// an administrator.
		if cookie, guestErr := guestSession(ctx, trust); guestErr == nil {
			script, contentType, err = download(
				ctx, trust.client, scriptURL.String(), cookie, maxScriptBytes)
		}
	}
	if err != nil {
		return Bundle{}, fmt.Errorf("download setup script: %w\n"+
			"This instance requires a signed-in caller and guest browsing is off. "+
			"Sign in to the console, then pass that session with -cookie-file.", err)
	}
	if !strings.HasPrefix(contentType, "text/") {
		return Bundle{}, fmt.Errorf("setup script has unexpected content type %q", contentType)
	}
	embeddedCA, err := embeddedCertificate(script)
	if err != nil {
		return Bundle{}, err
	}
	embeddedDisplay, err := onboarding.FingerprintSHA256(embeddedCA)
	if err != nil {
		return Bundle{}, fmt.Errorf("verify embedded CA: %w", err)
	}
	if normalizeFingerprint(embeddedDisplay) != normalizeFingerprint(trust.fingerprint) {
		return Bundle{}, errors.New("setup script embeds a different CA certificate")
	}
	return Bundle{
		Script: script, Extension: extension, Fingerprint: trust.fingerprint,
	}, nil
}

func fetchTrust(ctx context.Context, options Options) (verifiedTrust, error) {
	base, err := parseServer(options.Server)
	if err != nil {
		return verifiedTrust{}, err
	}
	if base.Scheme != "https" && options.Client == nil {
		return verifiedTrust{}, errors.New(
			"server must use https; the client only uses an unverified connection " +
				"to fetch fingerprint-pinned CA material")
	}
	project := options.Project
	if project == "" {
		project = "global"
	}
	if !onboarding.ValidProject(project) {
		return verifiedTrust{}, fmt.Errorf("invalid project %q", project)
	}
	expected, caForTLS, err := expectedFingerprint(options)
	if err != nil {
		return verifiedTrust{}, err
	}
	cookie, err := readCookie(options.CookieFile)
	if err != nil {
		return verifiedTrust{}, err
	}

	caClient := options.Client
	if caClient == nil {
		switch {
		case len(caForTLS) > 0:
			caClient, err = httpClient(caForTLS)
		case base.Scheme == "https":
			caClient = bootstrapHTTPClient()
		}
		if err != nil {
			return verifiedTrust{}, err
		}
	}

	caURL := *base
	caURL.Path = path.Join(base.Path, "/api/ca.crt")
	caPEM, _, err := download(ctx, caClient, caURL.String(), cookie, maxCABytes)
	if err != nil {
		return verifiedTrust{}, fmt.Errorf("download CA: %w", err)
	}
	actualDisplay, err := onboarding.FingerprintSHA256(caPEM)
	if err != nil {
		return verifiedTrust{}, fmt.Errorf("verify downloaded CA: %w", err)
	}
	actual := normalizeFingerprint(actualDisplay)
	if actual != expected {
		return verifiedTrust{}, fmt.Errorf("CA fingerprint mismatch: got %s, want %s",
			actualDisplay, displayFingerprint(expected))
	}

	verifiedClient := options.Client
	if verifiedClient == nil {
		verifiedClient, err = httpClient(caPEM)
		if err != nil {
			return verifiedTrust{}, fmt.Errorf("trust verified CA: %w", err)
		}
	}
	return verifiedTrust{
		base: base, project: project, cookie: cookie,
		caPEM: caPEM, fingerprint: actualDisplay, client: verifiedClient,
	}, nil
}

func fetchAptProxy(
	ctx context.Context,
	client *http.Client,
	base *url.URL,
	cookie, project string,
) (string, error) {
	coordinatesURL := *base
	coordinatesURL.Path = path.Join(base.Path, "/api/v1/coordinates")
	body, _, err := download(ctx, client, coordinatesURL.String(), cookie, 64<<10)
	if err != nil {
		return "", fmt.Errorf("download client coordinates: %w", err)
	}
	var coordinates struct {
		Proxy string `json:"proxy"`
	}
	if err := json.Unmarshal(body, &coordinates); err != nil {
		return "", fmt.Errorf("decode client coordinates: %w", err)
	}
	proxy, err := url.Parse("http://" + coordinates.Proxy)
	if err != nil || proxy.Hostname() == "" || proxy.Port() == "" {
		return "", errors.New("client coordinates contain an invalid proxy address")
	}
	if project != "global" {
		proxy.User = url.User(project)
	}
	return proxy.String(), nil
}

// Execute writes a verified bundle to a private temporary file and invokes the
// platform's native script host.
func Execute(ctx context.Context, bundle Bundle, options Options) error {
	temporary, err := os.CreateTemp("", "pkgreg-client-*."+bundle.Extension)
	if err != nil {
		return fmt.Errorf("create temporary setup script: %w", err)
	}
	name := temporary.Name()
	remove := func() {
		if !options.KeepTemporaryFile {
			_ = os.Remove(name)
		}
	}
	defer remove()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect temporary setup script: %w", err)
	}
	if _, err := temporary.Write(bundle.Script); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary setup script: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary setup script: %w", err)
	}

	goos := options.OperatingSystem
	if goos == "" {
		goos = runtime.GOOS
	}
	program, args, err := command(goos, name, options)
	if err != nil {
		return err
	}
	commandContext := options.CommandContext
	if commandContext == nil {
		commandContext = exec.CommandContext
	}
	cmd := commandContext(ctx, program, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = options.Stdout
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	cmd.Stderr = options.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("setup script failed: %w", err)
	}
	return nil
}

func command(goos, script string, options Options) (name string, args []string, err error) {
	switch goos {
	case "linux", "darwin":
		args := []string{script}
		if options.DryRun {
			args = append(args, "--dry-run")
		}
		if options.Uninstall {
			args = append(args, "--uninstall")
		}
		if options.Host != "" {
			args = append(args, "--host", options.Host)
		}
		if options.CacheIP != "" {
			args = append(args, "--cache-ip", options.CacheIP)
		}
		return "/bin/bash", args, nil
	case "windows":
		args := []string{
			"-NoLogo", "-NoProfile", "-NonInteractive",
			"-ExecutionPolicy", "Bypass", "-File", script,
		}
		if options.DryRun {
			args = append(args, "-DryRun")
		}
		if options.Uninstall {
			args = append(args, "-Uninstall")
		}
		if options.Host != "" {
			args = append(args, "-HostName", options.Host)
		}
		if options.CacheIP != "" {
			args = append(args, "-CacheIP", options.CacheIP)
		}
		return "powershell.exe", args, nil
	default:
		return "", nil, fmt.Errorf("unsupported operating system %q", goos)
	}
}

func parseServer(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.EscapedPath() != "" && parsed.EscapedPath() != "/") {
		return nil, errors.New("server must be an http(s) origin without credentials, query, or fragment")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return parsed, nil
}

func expectedFingerprint(options Options) (fingerprint string, caPEMOut []byte, err error) {
	expected := normalizeFingerprint(options.ExpectedSHA256)
	var caPEM []byte
	if options.CAFile != "" {
		caPEM, err = os.ReadFile(options.CAFile)
		if err != nil {
			return "", nil, fmt.Errorf("read CA file: %w", err)
		}
		display, err := onboarding.FingerprintSHA256(caPEM)
		if err != nil {
			return "", nil, fmt.Errorf("fingerprint CA file: %w", err)
		}
		fileFingerprint := normalizeFingerprint(display)
		if expected == "" {
			expected = fileFingerprint
		} else if expected != fileFingerprint {
			return "", nil, errors.New("CA file does not match the expected fingerprint")
		}
	}
	if len(expected) != 64 {
		return "", nil, errors.New("ca-sha256 must be the 64-hex SHA-256 fingerprint (colons are optional)")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return "", nil, errors.New("ca-sha256 contains non-hexadecimal characters")
	}
	return expected, caPEM, nil
}

func normalizeFingerprint(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, ":", "")
	value = strings.ReplaceAll(value, " ", "")
	return value
}

func displayFingerprint(raw string) string {
	var out strings.Builder
	for i := 0; i+2 <= len(raw); i += 2 {
		if i > 0 {
			out.WriteByte(':')
		}
		out.WriteString(raw[i : i+2])
	}
	return out.String()
}

func httpClient(caPEM []byte) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if len(caPEM) > 0 {
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("CA file contains no usable certificate")
		}
		transport.TLSClientConfig = &tls.Config{ // #nosec G402 -- defaults enforce certificate verification.
			MinVersion: tls.VersionTLS12, RootCAs: roots,
		}
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}, nil
}

// bootstrapHTTPClient is intentionally limited to fetching the public CA. Fetch
// verifies that response against the out-of-band SHA-256 fingerprint, constructs a
// normally verifying client from it, and uses that second client for the executable
// setup script.
func bootstrapHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{ // #nosec G402 -- fingerprint pinning authenticates the only downloaded bytes.
		MinVersion: tls.VersionTLS12, InsecureSkipVerify: true,
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}

func readCookie(file string) (string, error) {
	if file == "" {
		return "", nil
	}
	value, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read cookie file: %w", err)
	}
	cookie := strings.TrimSpace(string(value))
	if cookie == "" || strings.ContainsAny(cookie, "\r\n") {
		return "", errors.New("cookie file must contain one raw Cookie header value")
	}
	return cookie, nil
}

// errUnauthorized marks a refusal the caller can respond to by presenting a session,
// as distinct from a transport failure or a refusal no credential would fix.
var errUnauthorized = errors.New("server requires authentication")

func isUnauthorized(err error) bool { return errors.Is(err, errUnauthorized) }

// guestSession asks for the server's read-only guest session and returns it as a raw
// Cookie header. An instance with guest browsing disabled refuses, which is not an
// error worth surfacing on its own — the caller reports the original 401 instead,
// because "sign in" is the actionable advice in that case.
func guestSession(ctx context.Context, trust verifiedTrust) (string, error) {
	target := *trust.base
	target.Path = path.Join(trust.base.Path, "/api/v1/login/guest")
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, target.String(), http.NoBody)
	if err != nil {
		return "", err
	}
	response, err := trust.client.Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("guest session refused: %s", response.Status)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == sessionCookieName {
			return cookie.Name + "=" + cookie.Value, nil
		}
	}
	return "", errors.New("guest session carried no cookie")
}

// sessionCookieName mirrors auth.SessionCookie. It is duplicated rather than imported
// so the client keeps linking nothing from the server's control plane — the whole
// reason a developer laptop does not need a copy of the server to talk to one.
const sessionCookieName = "pkgreg_session"

func download(
	ctx context.Context,
	client *http.Client,
	target, cookie string,
	limit int64,
) (body []byte, contentType string, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return nil, "", err
	}
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		err := fmt.Errorf("server returned %s: %s",
			response.Status, strings.TrimSpace(string(message)))
		if response.StatusCode == http.StatusUnauthorized {
			err = fmt.Errorf("%w: %w", errUnauthorized, err)
		}
		return nil, "", err
	}
	reader := io.LimitReader(response.Body, limit+1)
	payload, err := io.ReadAll(reader)
	if err != nil {
		return nil, "", err
	}
	if int64(len(payload)) > limit {
		return nil, "", fmt.Errorf("response exceeds %d bytes", limit)
	}
	return payload, response.Header.Get("Content-Type"), nil
}

func embeddedCertificate(script []byte) ([]byte, error) {
	start := bytes.Index(script, []byte("-----BEGIN CERTIFICATE-----"))
	if start < 0 {
		return nil, errors.New("setup script does not embed a CA certificate")
	}
	block, _ := pem.Decode(script[start:])
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("setup script contains an invalid CA certificate")
	}
	return pem.EncodeToMemory(block), nil
}
