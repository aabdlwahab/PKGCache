// Command pkgreg-bridge is an unprivileged loopback bridge to a pkgreg cache.
//
// It exists to remove one specific piece of client setup: installing a certificate
// authority into the machine's trust store, and telling pip that a private host is
// safe to talk to. Both of those need root, both are per-machine, and both have to be
// redone when the cache's certificate is rotated.
//
// The bridge replaces them with a process the developer runs as themselves. Tools are
// pointed at http://127.0.0.1:<port> instead of https://cache:8443, and the bridge
// carries the request the rest of the way over verified TLS. That works because every
// shell client in this system already accepts loopback, and native Linux dockerd
// normally does too:
//
//   - pip ignores a plain-HTTP index on a named host unless --trusted-host is passed,
//     and accepts one on 127.0.0.1 with no flag at all;
//   - native Linux dockerd ships 127.0.0.0/8 in its default insecure-registry list;
//     Docker Desktop and remote daemons do not share the terminal's loopback;
//   - npm, uv, and git over http need nothing either way.
//
// What it does NOT do is remove the certificate from the picture. It moves the trust
// anchor out of the system store and into this process, where an ordinary user can
// manage it and where rotating the server's certificate touches nothing on the client.
//
// It is deliberately a separate program with no pkgreg imports and no dependencies
// beyond the standard library. It is an alternative to the CA setup script, not a
// replacement for it: if `pkgreg-bridge check` fails, the documented fallback is the
// existing script, and because the bridge never writes to the system there is nothing
// to undo first.
package clientbridge

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// defaultPort is high, fixed, and outside the ranges the tools themselves use.
	// It has to be fixed rather than allocated: PIP_INDEX_URL and NPM_CONFIG_REGISTRY
	// are static strings in config files, so a port that moved between runs would
	// invalidate every one of them.
	defaultPort = 41999

	// rewriteCap bounds how much of a textual response the bridge will buffer in order
	// to rewrite it. Indexes are the only responses that carry URLs; the largest real
	// one is a few megabytes, and the cache itself refuses to buffer a document over
	// 64 MiB, so matching that keeps the two in step.
	rewriteCap = 64 << 20
)

// Main runs the standalone bridge command.
func Main(argv []string, stdout, stderr io.Writer) int {
	return run(argv, stdout, stderr)
}

func run(argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pkgreg-bridge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		server    = fs.String("server", envOr("PKGREG_SERVER", ""), "cache base URL, e.g. https://cache.internal:8443")
		project   = fs.String("project", envOr("PKGREG_PROJECT", "global"), "project to generate client settings for")
		caFile    = fs.String("ca", envOr("PKGREG_CA_FILE", ""), "PEM file holding the cache's CA certificate")
		caSHA256  = fs.String("ca-sha256", envOr("PKGREG_CA_SHA256", ""), "SHA-256 fingerprint of the cache's CA, as shown by the console")
		port      = fs.Int("port", envInt("PKGREG_BRIDGE_PORT", defaultPort), "loopback port to listen on")
		tokenFile = fs.String("token-file", envOr("PKGREG_TOKEN_FILE", ""), "file holding a project token to attach to every request")
		check     = fs.Bool("check", false, "verify the cache is reachable and usable, then exit")
		printEnv  = fs.Bool("print-env", false, "print the environment settings that point tools at the bridge, then exit")
		shellKind = fs.String("shell", "sh", "format for -print-env: sh or powershell")
	)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, `pkgreg-bridge — run a pkgreg cache on localhost, without installing a CA

usage:
  pkgreg-bridge -server https://cache:8443 [-project NAME] [-ca-sha256 FP | -ca FILE]

  -check       probe the cache and report whether the bridge can replace CA setup
  -print-env   print the environment settings to apply, then exit

The bridge never writes to the system. To stop using it, stop the process and unset
the environment settings; nothing needs uninstalling.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if *server == "" {
		_, _ = fmt.Fprintln(stderr, "pkgreg-bridge: -server is required")
		return 2
	}
	target, err := parseServer(*server)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pkgreg-bridge: %v\n", err)
		return 2
	}
	if *port < 1 || *port > 65535 {
		_, _ = fmt.Fprintln(stderr, "pkgreg-bridge: -port must be between 1 and 65535")
		return 2
	}

	local := "127.0.0.1:" + strconv.Itoa(*port)
	if *printEnv {
		_, _ = fmt.Fprint(stdout, envScript(*shellKind, local, *project))
		return 0
	}

	tlsConfig, err := clientTLS(target, target.Hostname(), *caFile, *caSHA256)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pkgreg-bridge: %v\n", err)
		return 2
	}
	token, err := readToken(*tokenFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pkgreg-bridge: %v\n", err)
		return 2
	}

	b := &bridge{
		target: target,
		local:  local,
		token:  token,
		client: &http.Client{
			// The bridge is transparent: a redirect belongs to the client that asked
			// for it, not to us.
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
		},
	}

	if *check {
		return b.check(stdout, stderr, *project)
	}
	return b.serve(stdout, stderr, *project)
}

// bridge forwards loopback requests to one cache over verified TLS.
type bridge struct {
	target *url.URL
	local  string // the authority tools are configured with, e.g. 127.0.0.1:41999
	token  string
	client *http.Client
}

func (b *bridge) serve(stdout, stderr io.Writer, project string) int {
	// Loopback only, never 0.0.0.0. The bridge carries a project credential and the
	// socket itself is unauthenticated, so exposing it to the network would hand that
	// credential to anyone who can reach the port.
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", b.local)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pkgreg-bridge: listen on %s: %v\n", b.local, err)
		if isAddrInUse(err) {
			_, _ = fmt.Fprintf(stderr,
				"another process is already using port %s; pass -port to choose another\n",
				portOf(b.local))
		}
		return 1
	}

	server := &http.Server{
		Handler:           b,
		ReadHeaderTimeout: 30 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	_, _ = fmt.Fprintf(stdout, "pkgreg-bridge: http://%s -> %s (project %s)\n",
		b.local, b.target, project)
	_, _ = fmt.Fprint(stdout, "apply these settings in the shell that runs your tools:\n\n")
	_, _ = fmt.Fprint(stdout, envScript("sh", b.local, project))
	_, _ = fmt.Fprint(stdout, "\npress Ctrl-C to stop; nothing on this machine has been modified\n")

	select {
	case err := <-errs:
		_, _ = fmt.Fprintf(stderr, "pkgreg-bridge: %v\n", err)
		return 1
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	return 0
}

func (b *bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	outbound := *b.target
	outbound.Path = ""
	outbound.RawPath = ""
	// Use the escaped form: an npm scope arrives as @babel%2Fcore and must stay that
	// way, and an OCI reference carries a colon the cache resolves itself.
	requestURI := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		requestURI += "?" + r.URL.RawQuery
	}

	proxied, err := http.NewRequestWithContext(
		r.Context(), r.Method, outbound.String()+requestURI, r.Body)
	if err != nil {
		http.Error(w, "pkgreg-bridge: build request: "+err.Error(), http.StatusBadGateway)
		return
	}
	copyHeaders(proxied.Header, r.Header)
	proxied.Header.Del("Accept-Encoding") // see rewriteBody: we need to read what we forward
	proxied.Header.Set("Accept-Encoding", "identity")
	if b.token != "" && proxied.Header.Get("Authorization") == "" {
		// Holding the credential here is the point: it stays in one place instead of
		// being copied into .npmrc, pip.conf and a CI variable per tool.
		proxied.Header.Set("Authorization", "Bearer "+b.token)
	}
	// The cache builds the URLs it advertises from the Host header. Sending the
	// bridge's own authority is what makes an index point back here rather than at a
	// server name this machine has no reason to trust.
	proxied.Host = b.local

	response, err := b.client.Do(proxied)
	if err != nil {
		http.Error(w, "pkgreg-bridge: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = response.Body.Close() }()

	body, rewritten, err := b.rewriteBody(r.Method, response)
	if err != nil {
		http.Error(w, "pkgreg-bridge: "+err.Error(), http.StatusBadGateway)
		return
	}

	out := w.Header()
	copyHeaders(out, response.Header)
	// The cache answers over TLS, so every self-reference it emits says https. The
	// bridge speaks plain HTTP on loopback, so exactly one token has to change: its
	// own origin. This is a literal substitution, not an understanding of npm or PyPI
	// formats, which is what keeps the bridge free of protocol knowledge.
	for _, name := range []string{"Location", "Link", "Content-Location"} {
		if value := out.Get(name); value != "" {
			out.Set(name, strings.ReplaceAll(value, b.httpsOrigin(), b.httpOrigin()))
		}
	}
	if rewritten {
		out.Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(response.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	if rewritten {
		_, _ = w.Write(body)
		return
	}
	_, _ = io.Copy(w, response.Body)
}

// rewriteBody buffers and rewrites a textual response, and leaves everything else
// streaming.
//
// The distinction matters more than it looks: a wheel or an image layer can be
// gigabytes, and buffering one to search it for URLs would turn a cache into a memory
// bomb. Only documents carry URLs, and documents announce themselves by content type.
func (b *bridge) rewriteBody(
	method string, response *http.Response,
) (body []byte, rewritten bool, err error) {
	// A HEAD carries no body, and its Content-Length is the answer rather than a
	// description of what follows. Docker asks for a manifest by HEAD to learn its
	// size, so recomputing that length from an empty body reports zero and the pull
	// fails with "Target.Size must be greater than zero".
	if method == http.MethodHead {
		return nil, false, nil
	}
	// Content committed to a digest must arrive byte-for-byte. An OCI manifest is
	// "…+json" and would otherwise qualify as textual, but the client verifies it
	// against the digest in this header, so it is not ours to touch.
	if response.Header.Get("Docker-Content-Digest") != "" {
		return nil, false, nil
	}
	if !textualContent(response.Header.Get("Content-Type")) {
		return nil, false, nil
	}
	if response.ContentLength > rewriteCap {
		return nil, false, nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, rewriteCap+1))
	if err != nil {
		return nil, false, fmt.Errorf("read response: %w", err)
	}
	if int64(len(raw)) > rewriteCap {
		return nil, false, fmt.Errorf("document over %d bytes", rewriteCap)
	}
	replaced := strings.ReplaceAll(string(raw), b.httpsOrigin(), b.httpOrigin())
	return []byte(replaced), true, nil
}

func (b *bridge) httpsOrigin() string { return "https://" + b.local }
func (b *bridge) httpOrigin() string  { return "http://" + b.local }

// check reports whether the bridge can stand in for the certificate setup on this
// machine, and says what to do instead when it cannot.
func (b *bridge) check(stdout, stderr io.Writer, project string) int {
	type probe struct {
		name string
		path string
	}
	probes := []probe{
		{"readiness", "/readyz"},
		{"pypi index", "/" + project + "/pypi/root/pypi/+simple/"},
		{"npm root", "/" + project + "/npm/"},
		{"OCI registry", "/v2/"},
	}
	failed := 0
	for _, p := range probes {
		status, err := b.probe(p.path)
		switch {
		case err != nil:
			_, _ = fmt.Fprintf(stdout, "  %-14s FAIL  %v\n", p.name, err)
			failed++
		case status >= 500:
			_, _ = fmt.Fprintf(stdout, "  %-14s FAIL  HTTP %d\n", p.name, status)
			failed++
		default:
			_, _ = fmt.Fprintf(stdout, "  %-14s ok    HTTP %d\n", p.name, status)
		}
	}
	if failed > 0 {
		_, _ = fmt.Fprintf(stderr, `
%d of %d checks failed, so the bridge cannot replace certificate setup here.

If this is a connectivity failure, fix it before choosing another mode. A managed
build host can explicitly preview and apply persistent setup:

  pkgreg-client --persist -server %s -project %s -ca-sha256 <fingerprint> -dry-run
  sudo pkgreg-client --persist -server %s -project %s -ca-sha256 <fingerprint>

Nothing needs undoing first: the bridge does not modify this machine.
`, failed, len(probes), b.target, project, b.target, project)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "\nthe cache is reachable and verified; run the bridge and apply:\n\n%s",
		envScript("sh", b.local, project))
	return 0
}

func (b *bridge) probe(path string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.target.String()+path, http.NoBody)
	if err != nil {
		return 0, err
	}
	request.Host = b.local
	if b.token != "" {
		request.Header.Set("Authorization", "Bearer "+b.token)
	}
	response, err := b.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, nil
}

// ---- TLS -------------------------------------------------------------------

// clientTLS builds the verification policy for the hop the bridge still makes over the
// network. One of two anchors is required — there is no mode that skips verification,
// because a bridge that trusted anything would be strictly worse than the CA install
// it replaces.
func clientTLS(target *url.URL, serverName, caFile, caSHA256 string) (*tls.Config, error) {
	base := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	switch {
	case caFile != "":
		encoded, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(encoded) {
			return nil, fmt.Errorf("%s contains no PEM certificate", caFile)
		}
		base.RootCAs = pool
		return base, nil

	case caSHA256 != "":
		// Pinning by fingerprint means onboarding is one copy-pasteable string rather
		// than a file to move around, and the console already displays it in this form.
		//
		// The cache presents only its leaf certificate, so the pin cannot be checked
		// against the handshake: the CA is the thing the client is expected to already
		// hold. Instead the CA is fetched from the cache over an unverified connection
		// and checked against the pin before it is trusted for anything. That exchange
		// carries no request of ours and no credential, and a certificate is public;
		// an attacker who intercepts it can only serve a CA whose fingerprint will not
		// match, which fails closed.
		want, err := parseFingerprint(caSHA256)
		if err != nil {
			return nil, err
		}
		pool, err := fetchPinnedCA(target, serverName, want)
		if err != nil {
			return nil, err
		}
		base.RootCAs = pool
		return base, nil
	}
	return nil, errors.New("one of -ca or -ca-sha256 is required; " +
		"the fingerprint is shown in the console next to the CA download")
}

// fetchPinnedCA bootstraps trust from a fingerprint, and returns a pool holding only
// the CA that matched it. Every later request is verified normally against that pool —
// signature, validity dates and host name included.
func fetchPinnedCA(target *url.URL, serverName string, want [32]byte) (*x509.CertPool, error) {
	bootstrap := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			// #nosec G402 -- the response is a public certificate that is rejected
			// unless it matches the operator-supplied fingerprint below.
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, ServerName: serverName, MinVersion: tls.VersionTLS12,
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, target.String()+"/api/ca.crt", http.NoBody)
	if err != nil {
		return nil, err
	}
	response, err := bootstrap.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch CA from %s: %w", target, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch CA from %s: HTTP %d", target, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	block, _ := pem.Decode(body)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("the cache did not return a PEM certificate")
	}
	got := sha256.Sum256(block.Bytes)
	if got != want {
		return nil, fmt.Errorf(
			"CA fingerprint mismatch: the cache offered %s, you pinned %s.\n"+
				"Either the fingerprint is wrong or this is not the cache you meant to reach",
			formatFingerprint(got), formatFingerprint(want))
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA: %w", err)
	}
	if !certificate.IsCA {
		return nil, errors.New("the certificate the cache returned is not a CA")
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return pool, nil
}

func parseFingerprint(value string) ([32]byte, error) {
	var out [32]byte
	cleaned := strings.ToLower(strings.NewReplacer(":", "", " ", "", "-", "").Replace(value))
	cleaned = strings.TrimPrefix(cleaned, "sha256")
	raw, err := hex.DecodeString(cleaned)
	if err != nil || len(raw) != 32 {
		return out, fmt.Errorf("-ca-sha256 must be a 64-character SHA-256 fingerprint")
	}
	copy(out[:], raw)
	return out, nil
}

// formatFingerprint renders a fingerprint the way the console and openssl show it, so
// an operator can compare the two by eye.
func formatFingerprint(sum [32]byte) string {
	raw := strings.ToUpper(hex.EncodeToString(sum[:]))
	var b strings.Builder
	for i := 0; i < len(raw); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(raw[i : i+2])
	}
	return b.String()
}

// ---- client settings --------------------------------------------------------

// envScript prints the settings that point each tool at the bridge.
//
// These are the same variables the CA setup script writes, with two differences: the
// URLs are loopback, and none of the *_CA / *_CERT variables are needed at all, which
// is the entire point of the exercise.
func envScript(shell, local, project string) string {
	base := "http://" + local
	pypi := base + "/" + project + "/pypi/root/pypi/+simple/"
	npm := base + "/" + project + "/npm/"
	files := base + "/" + project + "/files/"
	var b strings.Builder
	if strings.EqualFold(shell, "powershell") {
		for _, kv := range [][2]string{
			{"PKGREG_SESSION", "temporary"},
			{"PKGREG_PROJECT", project},
			{"PKGREG_BRIDGE_URL", base},
			{"PKGREG_DOCKER_REGISTRY", local},
			{"PKGREG_GIT_URL", base + "/" + project + "/git"},
			{"PIP_INDEX_URL", pypi},
			{"UV_DEFAULT_INDEX", pypi},
			{"NPM_CONFIG_REGISTRY", npm},
			{"PKGREG_FILES_URL", files},
		} {
			_, _ = fmt.Fprintf(&b, "$env:%s = %q\n", kv[0], kv[1])
		}
		return b.String()
	}
	for _, kv := range [][2]string{
		{"PKGREG_SESSION", "temporary"},
		{"PKGREG_PROJECT", project},
		{"PKGREG_BRIDGE_URL", base},
		{"PKGREG_DOCKER_REGISTRY", local},
		{"PKGREG_GIT_URL", base + "/" + project + "/git"},
		{"PIP_INDEX_URL", pypi},
		{"UV_DEFAULT_INDEX", pypi},
		{"NPM_CONFIG_REGISTRY", npm},
		{"PKGREG_FILES_URL", files},
	} {
		_, _ = fmt.Fprintf(&b, "export %s=%s\n", kv[0], kv[1])
	}
	return b.String()
}

// ---- plumbing ---------------------------------------------------------------

// hopByHop headers belong to a single connection and must not be relayed.
var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true,
}

func copyHeaders(dst, src http.Header) {
	for name, values := range src {
		if hopByHop[http.CanonicalHeaderKey(name)] {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

// textualContent reports whether a body may carry URLs the bridge has to rewrite.
// Artifact bodies never do, and must keep streaming.
func textualContent(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	switch {
	case mediaType == "":
		return false
	case strings.HasPrefix(mediaType, "text/"):
		return true
	case mediaType == "application/json", strings.HasSuffix(mediaType, "+json"):
		return true
	case mediaType == "application/xml", strings.HasSuffix(mediaType, "+xml"):
		return true
	}
	return false
}

func parseServer(raw string) (*url.URL, error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid -server %q: %w", raw, err)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("-server must be https; the bridge exists to terminate TLS for you")
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("invalid -server %q: no host", raw)
	}
	return &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}, nil
}

func readToken(path string) (string, error) {
	if path == "" {
		return strings.TrimSpace(os.Getenv("PKGREG_TOKEN")), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil {
		return value
	}
	return fallback
}

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

func portOf(authority string) string {
	_, port, err := net.SplitHostPort(authority)
	if err != nil {
		return authority
	}
	return port
}
