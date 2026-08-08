package clientbridge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newBridge wires a bridge to a fake cache, skipping the pinning path so the tests
// below are about forwarding behaviour rather than TLS setup.
func newBridge(t *testing.T, cache *httptest.Server) *bridge {
	t.Helper()
	// The fake cache speaks plain HTTP, so the bridge points at it directly; the TLS
	// hop is exercised separately by the trust-bootstrap tests below.
	upstream, err := url.Parse(cache.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := cache.Client()
	// Match the real client: a redirect belongs to the caller, so the bridge relays it
	// rather than chasing it to an address only the caller can reach.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &bridge{
		target: upstream,
		local:  "127.0.0.1:41999",
		client: client,
	}
}

func through(t *testing.T, b *bridge, method, path string) *http.Response {
	t.Helper()
	recorder := httptest.NewRecorder()
	b.ServeHTTP(recorder, httptest.NewRequest(method, path, http.NoBody))
	return recorder.Result()
}

// The cache builds every URL it advertises from the Host header, and answers over TLS,
// so an index arrives claiming https://<bridge>. Left alone, the client would try to
// speak TLS to a plain-HTTP loopback port and the whole scheme would collapse on the
// second request.
func TestTextualResponseRewritesTheBridgeOrigin(t *testing.T) {
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<a href="https://%s/global/pypi/root/pypi/+f/six/six.whl">six</a>`, r.Host)
	}))
	defer cache.Close()
	b := newBridge(t, cache)

	response := through(t, b, http.MethodGet, "/global/pypi/root/pypi/+simple/six/")
	body, _ := io.ReadAll(response.Body)
	if strings.Contains(string(body), "https://127.0.0.1:41999") {
		t.Fatalf("https origin survived the rewrite: %s", body)
	}
	if !strings.Contains(string(body), "http://127.0.0.1:41999/global/pypi") {
		t.Fatalf("body does not point back at the bridge: %s", body)
	}
	if got := response.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q, want %d after rewriting", got, len(body))
	}
}

// Artifacts are the reason the bridge cannot simply buffer everything: a wheel or an
// image layer can be gigabytes.
func TestBinaryResponseIsNotBuffered(t *testing.T) {
	payload := strings.Repeat("\x00\xff binary ", 4096)
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = io.WriteString(w, payload)
	}))
	defer cache.Close()
	b := newBridge(t, cache)

	response := through(t, b, http.MethodGet, "/global/files/artifact.bin")
	body, _ := io.ReadAll(response.Body)
	if string(body) != payload {
		t.Fatalf("binary body changed: got %d bytes, want %d", len(body), len(payload))
	}
	if got := response.Header.Get("Content-Length"); got != strconv.Itoa(len(payload)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(payload))
	}
}

// Docker asks for a manifest by HEAD to learn its size before fetching it. Recomputing
// the length from an empty body reported zero and the pull failed with
// "Target.Size must be greater than zero".
func TestHeadPreservesUpstreamContentLength(t *testing.T) {
	const manifestSize = 1721
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Content-Length", strconv.Itoa(manifestSize))
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
		w.WriteHeader(http.StatusOK)
	}))
	defer cache.Close()
	b := newBridge(t, cache)

	response := through(t, b, http.MethodHead, "/v2/library/alpine/manifests/3.20")
	if got := response.Header.Get("Content-Length"); got != strconv.Itoa(manifestSize) {
		t.Fatalf("HEAD Content-Length = %q, want %d", got, manifestSize)
	}
}

// A manifest is verified by the client against the digest in this header, so it is not
// the bridge's to rewrite even though "…+json" otherwise looks textual.
func TestDigestCommittedBodyIsNeverRewritten(t *testing.T) {
	const body = `{"note":"https://127.0.0.1:41999 appears here","v":2}`
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("b", 64))
		_, _ = io.WriteString(w, body)
	}))
	defer cache.Close()
	b := newBridge(t, cache)

	response := through(t, b, http.MethodGet, "/v2/library/alpine/manifests/3.20")
	got, _ := io.ReadAll(response.Body)
	if string(got) != body {
		t.Fatalf("digest-committed body was altered:\n got %s\nwant %s", got, body)
	}
}

// The Host header is what makes the cache advertise the bridge rather than itself.
func TestHostHeaderIsTheBridgeAuthority(t *testing.T) {
	var seen string
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer cache.Close()
	b := newBridge(t, cache)

	through(t, b, http.MethodGet, "/global/npm/left-pad")
	if seen != b.local {
		t.Fatalf("cache saw Host %q, want %q", seen, b.local)
	}
}

// Escaping has to survive the hop untouched: an npm scope and a PyPI "+f" segment both
// address different routes if their encoding changes on the way through.
func TestEscapedPathIsForwardedVerbatim(t *testing.T) {
	var seen string
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
	}))
	defer cache.Close()
	b := newBridge(t, cache)

	for _, path := range []string{
		"/global/npm/@babel%2Fcore",
		"/global/pypi/root/pypi/%2Bf/six/six-1.17.0.whl.metadata",
		"/global/files/name+with+plus",
	} {
		through(t, b, http.MethodGet, path)
		if seen != path {
			t.Errorf("cache saw %q, want %q", seen, path)
		}
	}
}

// Holding the credential in one process is the point: it stops being copied into
// .npmrc, pip.conf and a CI variable per tool.
func TestTokenIsAttachedButNeverOverridesTheCaller(t *testing.T) {
	var seen string
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer cache.Close()
	b := newBridge(t, cache)
	b.token = "id.secret"

	through(t, b, http.MethodGet, "/global/files/x")
	if seen != "Bearer id.secret" {
		t.Fatalf("Authorization = %q", seen)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/global/files/x", http.NoBody)
	request.Header.Set("Authorization", "Bearer caller-supplied")
	b.ServeHTTP(recorder, request)
	if seen != "Bearer caller-supplied" {
		t.Fatalf("the bridge overrode a caller's own credential: %q", seen)
	}
}

func TestRedirectsAreRelayedNotFollowed(t *testing.T) {
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/from" {
			w.Header().Set("Location", "https://"+r.Host+"/to")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer cache.Close()
	b := newBridge(t, cache)

	response := through(t, b, http.MethodGet, "/from")
	if response.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", response.StatusCode)
	}
	if got := response.Header.Get("Location"); got != "http://127.0.0.1:41999/to" {
		t.Fatalf("Location = %q, want the bridge's own http origin", got)
	}
}

// ---- trust bootstrap --------------------------------------------------------

func TestFetchPinnedCAAcceptsOnlyTheMatchingFingerprint(t *testing.T) {
	caPEM, caDER := selfSignedCA(t)
	cache := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ca.crt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(caPEM)
	}))
	defer cache.Close()
	target, err := url.Parse(cache.URL)
	if err != nil {
		t.Fatal(err)
	}

	correct := sha256.Sum256(caDER)
	pool, err := fetchPinnedCA(target, target.Hostname(), correct)
	if err != nil || pool == nil {
		t.Fatalf("matching fingerprint rejected: %v", err)
	}

	var wrong [32]byte
	wrong[0] = correct[0] ^ 0xff
	if _, err := fetchPinnedCA(target, target.Hostname(), wrong); err == nil {
		t.Fatal("a mismatched fingerprint was accepted")
	}
}

func TestClientTLSRequiresAnAnchor(t *testing.T) {
	target, _ := url.Parse("https://cache.example:8443")
	if _, err := clientTLS(target, "cache.example", "", ""); err == nil {
		t.Fatal("the bridge accepted a configuration with nothing to verify against")
	}
}

func selfSignedCA(t *testing.T) (pemBytes, der []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "pkgreg test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err = x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), der
}

// ---- unit -------------------------------------------------------------------

func TestTextualContentClassification(t *testing.T) {
	for _, c := range []struct {
		mediaType string
		want      bool
	}{
		{"text/html; charset=utf-8", true},
		{"application/json", true},
		{"application/vnd.oci.image.index.v1+json", true},
		{"application/octet-stream", false},
		{"application/gzip", false},
		{"application/x-git-upload-pack-advertisement", false},
		{"", false},
	} {
		if got := textualContent(c.mediaType); got != c.want {
			t.Errorf("textualContent(%q) = %v, want %v", c.mediaType, got, c.want)
		}
	}
}

func TestParseServerRejectsPlaintextAndJunk(t *testing.T) {
	if _, err := parseServer("http://cache:8443"); err == nil {
		t.Error("a plaintext cache URL was accepted; the bridge exists to terminate TLS")
	}
	if _, err := parseServer("://nonsense"); err == nil {
		t.Error("a malformed URL was accepted")
	}
	parsed, err := parseServer("cache.internal:8443")
	if err != nil || parsed.Scheme != "https" || parsed.Host != "cache.internal:8443" {
		t.Errorf("bare authority = %+v, %v", parsed, err)
	}
}

func TestParseFingerprintAcceptsTheFormatsOperatorsPasteIn(t *testing.T) {
	want := sha256.Sum256([]byte("x"))
	spellings := []string{
		fmt.Sprintf("%x", want),
		strings.ToUpper(fmt.Sprintf("%x", want)),
		formatFingerprint(want),
		"sha256:" + fmt.Sprintf("%x", want),
	}
	for _, spelling := range spellings {
		got, err := parseFingerprint(spelling)
		if err != nil || got != want {
			t.Errorf("parseFingerprint(%q) = %x, %v", spelling, got, err)
		}
	}
	if _, err := parseFingerprint("deadbeef"); err == nil {
		t.Error("a short fingerprint was accepted")
	}
}

func TestEnvScriptPointsEveryToolAtTheBridge(t *testing.T) {
	for _, shell := range []string{"sh", "powershell"} {
		script := envScript(shell, "127.0.0.1:41999", "team-a")
		for _, want := range []string{
			"PIP_INDEX_URL", "UV_DEFAULT_INDEX", "NPM_CONFIG_REGISTRY",
			"http://127.0.0.1:41999/team-a/pypi/root/pypi/+simple/",
		} {
			if !strings.Contains(script, want) {
				t.Errorf("%s script missing %q:\n%s", shell, want, script)
			}
		}
		// The entire point is that no certificate variable is needed.
		for _, unwanted := range []string{"PIP_CERT", "NODE_EXTRA_CA_CERTS", "CAFILE"} {
			if strings.Contains(script, unwanted) {
				t.Errorf("%s script still sets %q", shell, unwanted)
			}
		}
	}
}

func TestTLSMinimumVersion(t *testing.T) {
	target, _ := url.Parse("https://cache.example:8443")
	config, err := clientTLS(target, "cache.example", writeTempCA(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if config.MinVersion < tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x", config.MinVersion)
	}
	if config.InsecureSkipVerify {
		t.Fatal("verification is disabled on the real traffic path")
	}
}

func writeTempCA(t *testing.T) string {
	t.Helper()
	pemBytes, _ := selfSignedCA(t)
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
