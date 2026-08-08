package pki

import (
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCreateAndReuseCA(t *testing.T) {
	dir := t.TempDir()

	ca, created, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	if !created {
		t.Fatal("first call should report the CA as created")
	}
	if !ca.Cert.IsCA || ca.Cert.MaxPathLen != 0 || !ca.Cert.MaxPathLenZero {
		t.Fatalf("CA constraints wrong: IsCA=%v pathlen=%d", ca.Cert.IsCA, ca.Cert.MaxPathLen)
	}

	// Reuse is the whole point: minting a new CA would invalidate trust that has
	// already been distributed to every build host.
	again, created, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if created {
		t.Fatal("second call must reuse, not mint")
	}
	if !again.Cert.Equal(ca.Cert) {
		t.Fatal("reloaded a different CA certificate")
	}
}

func TestCAKeyIsNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := LoadOrCreateCA(dir); err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	for _, f := range []string{CAKeyFile} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("stat %s: %v", f, err)
		}
		if fi.Mode().Perm() != keyMode {
			t.Fatalf("%s mode = %v, want %v", f, fi.Mode().Perm(), os.FileMode(keyMode))
		}
	}
	fi, err := os.Stat(filepath.Join(dir, CACertFile))
	if err != nil {
		t.Fatalf("stat cert: %v", err)
	}
	if fi.Mode().Perm() != certMode {
		t.Fatalf("cert mode = %v, want %v", fi.Mode().Perm(), os.FileMode(certMode))
	}
}

// Half a CA must never be silently completed: doing so would break every client that
// already trusts the surviving half.
func TestHalfCARefuses(t *testing.T) {
	for _, remove := range []string{CAKeyFile, CACertFile} {
		t.Run("missing "+remove, func(t *testing.T) {
			dir := t.TempDir()
			if _, _, err := LoadOrCreateCA(dir); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if err := os.Remove(filepath.Join(dir, remove)); err != nil {
				t.Fatalf("remove: %v", err)
			}
			if _, _, err := LoadOrCreateCA(dir); err == nil {
				t.Fatal("expected a refusal, not a silent re-mint")
			}
		})
	}
}

func TestIssueServerAndVerifyChain(t *testing.T) {
	dir := t.TempDir()
	ca, _, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	sans, err := ca.IssueServer([]string{"localhost", "127.0.0.1", "cache.example.com", "10.0.0.5"})
	if err != nil {
		t.Fatalf("IssueServer: %v", err)
	}
	if !slices.Contains(sans, "DNS:localhost") || !slices.Contains(sans, "IP:127.0.0.1") {
		t.Fatalf("SANs = %v", sans)
	}

	pool := x509.NewCertPool()
	caPEM, _ := os.ReadFile(filepath.Join(dir, CACertFile))
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("CA certificate is not loadable into a pool")
	}
	leafPEM, _ := os.ReadFile(filepath.Join(dir, ServerCertFile))
	block, _ := pemBlock(leafPEM)
	leaf, err := x509.ParseCertificate(block)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	for _, name := range []string{"localhost", "cache.example.com"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name}); err != nil {
			t.Fatalf("chain does not verify for %s: %v", name, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "evil.example.com"}); err == nil {
		t.Fatal("leaf verified for a name that is not in its SANs")
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("leaf EKU = %v, want serverAuth only", leaf.ExtKeyUsage)
	}
}

// Spike S4: the material must actually work for a real TLS client, not merely parse.
func TestServesRealTLS(t *testing.T) {
	dir := t.TempDir()
	ca, _, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	if _, err := ca.IssueServer([]string{"localhost", "127.0.0.1"}); err != nil {
		t.Fatalf("IssueServer: %v", err)
	}

	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, ServerCertFile), filepath.Join(dir, ServerKeyFile))
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	caPEM, _ := os.ReadFile(filepath.Join(dir, CACertFile))
	pool.AppendCertsFromPEM(caPEM)

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
	// Reach it by 127.0.0.1 so the IP SAN is what gets validated.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	resp, err := client.Get("https://127.0.0.1:" + port)
	if err != nil {
		t.Fatalf("TLS handshake against our own material failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// And an untrusting client must still reject it — the CA is private, not magic.
	if _, err := http.Get("https://127.0.0.1:" + port); err == nil { //nolint:bodyclose
		t.Fatal("a client without the CA should not trust this certificate")
	}
}

func TestReissueKeepsCA(t *testing.T) {
	dir := t.TempDir()
	ca, _, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	if _, err := ca.IssueServer([]string{"localhost"}); err != nil {
		t.Fatalf("first issue: %v", err)
	}
	firstLeaf, _ := os.ReadFile(filepath.Join(dir, ServerCertFile))
	firstCA, _ := os.ReadFile(filepath.Join(dir, CACertFile))

	// Adding a hostname must replace the leaf and leave the CA alone.
	reloaded, created, err := LoadOrCreateCA(dir)
	if err != nil || created {
		t.Fatalf("reload: err=%v created=%v", err, created)
	}
	if _, err := reloaded.IssueServer([]string{"localhost", "newhost.example.com"}); err != nil {
		t.Fatalf("re-issue: %v", err)
	}
	secondLeaf, _ := os.ReadFile(filepath.Join(dir, ServerCertFile))
	secondCA, _ := os.ReadFile(filepath.Join(dir, CACertFile))

	if string(firstCA) != string(secondCA) {
		t.Fatal("re-issuing the leaf changed the CA")
	}
	if string(firstLeaf) == string(secondLeaf) {
		t.Fatal("re-issue did not replace the leaf")
	}
}

func TestIssueServerRequiresNames(t *testing.T) {
	dir := t.TempDir()
	ca, _, _ := LoadOrCreateCA(dir)
	if _, err := ca.IssueServer(nil); err == nil {
		t.Fatal("expected an error for an empty SAN list")
	}
	if _, err := ca.IssueServer([]string{"", "  "}); err == nil {
		t.Fatal("blank names should not count as SANs")
	}
}

func TestDiscoverSANs(t *testing.T) {
	got := DiscoverSANs(t.Context(), "cache.example.com", "cache.example.com", "10.1.2.3")
	if !slices.Contains(got, "localhost") || !slices.Contains(got, "127.0.0.1") {
		t.Fatalf("loopback names missing: %v", got)
	}
	if !slices.Contains(got, "cache.example.com") || !slices.Contains(got, "10.1.2.3") {
		t.Fatalf("extras missing: %v", got)
	}
	seen := map[string]int{}
	for _, s := range got {
		seen[s]++
		if seen[s] > 1 {
			t.Fatalf("duplicate SAN %q in %v", s, got)
		}
	}
}

func TestSplitSANs(t *testing.T) {
	dns, ips := splitSANs([]string{"B.example.com", "127.0.0.1", "localhost", "a.example.com.", "::1", "127.0.0.1"})
	if dns[0] != "localhost" {
		t.Fatalf("localhost should sort first (it becomes the CN): %v", dns)
	}
	if !slices.Contains(dns, "b.example.com") {
		t.Fatalf("names should be lowercased: %v", dns)
	}
	if !slices.Contains(dns, "a.example.com") {
		t.Fatalf("trailing dot should be stripped: %v", dns)
	}
	if len(ips) != 2 {
		t.Fatalf("ips = %v, want 2 deduplicated", ips)
	}
}

// An existing deployment's ca.key came from `openssl genrsa`, which emits PKCS#1.
// Reusing it unchanged is what lets a migrating host keep its distributed trust.
func TestLoadsOpenSSLStylePKCS1Key(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := LoadOrCreateCA(dir); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pkcs8, err := os.ReadFile(filepath.Join(dir, CAKeyFile))
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	der, _ := pemBlock(pkcs8)
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rsaPriv, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("expected an RSA CA key, got %T", parsed)
	}
	if err := writePEM(filepath.Join(dir, CAKeyFile), "RSA PRIVATE KEY",
		x509.MarshalPKCS1PrivateKey(rsaPriv), keyMode); err != nil {
		t.Fatalf("write pkcs1: %v", err)
	}

	reloaded, created, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("loading a PKCS#1 key failed: %v", err)
	}
	if created {
		t.Fatal("should have reused the existing CA")
	}
	if _, err := reloaded.IssueServer([]string{"localhost"}); err != nil {
		t.Fatalf("issuing with a PKCS#1 CA key failed: %v", err)
	}
}

func pemBlock(b []byte) ([]byte, []byte) {
	block, rest := pem.Decode(b)
	if block == nil {
		return nil, rest
	}
	return block.Bytes, rest
}
