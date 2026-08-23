package trust

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/onboarding"
)

// authority mints a throwaway CA so the fingerprint under test is a real one.
func authority(t *testing.T) (pemBytes []byte, fingerprint string) {
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
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	display, err := onboarding.FingerprintSHA256(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	return pemBytes, display
}

// serving answers /api/ca.crt with the given bytes.
func serving(t *testing.T, caPEM []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ca.crt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(caPEM)
	}))
	t.Cleanup(server.Close)
	return server
}

// The whole point: bytes fetched over an unverified connection are only believed once
// they match a fingerprint that arrived by another route.
func TestFetchAcceptsAMatchingFingerprint(t *testing.T) {
	caPEM, fingerprint := authority(t)
	server := serving(t, caPEM)

	verified, err := Fetch(context.Background(), Options{
		Server: server.URL, ExpectedSHA256: fingerprint, Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(verified.CAPEM) != string(caPEM) {
		t.Error("the returned CA is not the one served")
	}
	if verified.Fingerprint != fingerprint {
		t.Errorf("fingerprint = %q, want %q", verified.Fingerprint, fingerprint)
	}
	if verified.Base.Host == "" {
		t.Error("the origin was not parsed")
	}
}

// An attacker in the middle can serve any CA they like; what they cannot do is make it
// hash to the value you were given.
func TestFetchRefusesASubstitutedCA(t *testing.T) {
	_, expected := authority(t)
	substituted, _ := authority(t)
	server := serving(t, substituted)

	_, err := Fetch(context.Background(), Options{
		Server: server.URL, ExpectedSHA256: expected, Client: server.Client(),
	})
	if err == nil {
		t.Fatal("a CA with the wrong fingerprint was accepted")
	}
	if !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("error does not explain the refusal: %v", err)
	}
}

// Colons, spaces and case are how people paste a fingerprint. All of them must compare
// equal, or the check fails for a reason that looks like an attack and is not.
func TestFingerprintFormsAreEquivalent(t *testing.T) {
	caPEM, display := authority(t)
	server := serving(t, caPEM)
	bare := Normalize(display)

	for _, form := range []string{display, bare, strings.ToLower(bare), " " + display + " "} {
		if _, err := Fetch(context.Background(), Options{
			Server: server.URL, ExpectedSHA256: form, Client: server.Client(),
		}); err != nil {
			t.Errorf("fingerprint form %q was refused: %v", form, err)
		}
	}
	if Display(bare) != display {
		t.Errorf("Display(%q) = %q, want %q", bare, Display(bare), display)
	}
}

// A CA on disk supplies its own fingerprint, and disagreeing with an explicit one is an
// error rather than a preference.
func TestExpectedFingerprintFromFile(t *testing.T) {
	caPEM, display := authority(t)
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	expected, fromFile, err := ExpectedFingerprint("", path)
	if err != nil {
		t.Fatal(err)
	}
	if expected != Normalize(display) || len(fromFile) == 0 {
		t.Fatalf("expected = %q, ca bytes = %d", expected, len(fromFile))
	}
	if _, _, err := ExpectedFingerprint(display, path); err != nil {
		t.Errorf("an agreeing pair was refused: %v", err)
	}
	_, other := authority(t)
	if _, _, err := ExpectedFingerprint(other, path); err == nil {
		t.Error("a CA file that contradicts the expected fingerprint was accepted")
	}
}

func TestExpectedFingerprintRejectsNonsense(t *testing.T) {
	for _, value := range []string{"", "abc", strings.Repeat("z", 64)} {
		if _, _, err := ExpectedFingerprint(value, ""); err == nil {
			t.Errorf("%q was accepted as a fingerprint", value)
		}
	}
}

func TestParseServerRejectsMoreThanAnOrigin(t *testing.T) {
	for _, raw := range []string{
		"https://user:pass@cache:8443",
		"https://cache:8443/?x=1",
		"https://cache:8443/#f",
		"https://cache:8443/some/path",
		"ftp://cache",
		"cache:8443",
		"",
	} {
		if _, err := ParseServer(raw); err == nil {
			t.Errorf("ParseServer(%q) was accepted", raw)
		}
	}
	parsed, err := ParseServer(" https://cache.internal:8443/ ")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "cache.internal:8443" || parsed.Path != "" {
		t.Fatalf("parsed = %+v", parsed)
	}
}

// The CA is added to the system roots rather than replacing them, because a chain whose
// last endpoint is a public registry has to keep verifying normally.
func TestPoolKeepsTheSystemRoots(t *testing.T) {
	caPEM, _ := authority(t)
	roots, err := Pool(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	system, err := x509.SystemCertPool()
	if err != nil {
		t.Skip("no system certificate pool on this host")
	}
	if len(system.Subjects()) > 0 && //nolint:staticcheck // comparing pool size is the point
		len(roots.Subjects()) <= len(system.Subjects()) {
		t.Fatal("the CA replaced the system roots instead of adding to them")
	}
}

func TestPoolRejectsRubbish(t *testing.T) {
	if _, err := Pool([]byte("not a certificate")); err == nil {
		t.Fatal("Pool accepted material with no certificate in it")
	}
}

// A 401 on the CA leg is reported as such, so a caller can answer it with a session
// rather than treating an authenticated instance as unreachable.
func TestFetchSurfacesUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	_, expected := authority(t)

	_, err := Fetch(context.Background(), Options{
		Server: server.URL, ExpectedSHA256: expected, Client: server.Client(),
	})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}
