package feed

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testSigningKey makes a small, fast key. 2048 bits is not what an instance ships with —
// GenerateKey defaults to 4096 — but a test that spent ten seconds on RSA would be a test
// people skip.
func testSigningKey(t *testing.T) *PGPKey {
	t.Helper()
	key, err := GenerateKey(KeyOptions{
		Name: "pkgreg test", Email: "test@example.invalid", RSABits: 2048,
	})
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return key
}

func TestGenerateKeyRefusesAnAnonymousKey(t *testing.T) {
	// The name is what somebody sees when they inspect what they have trusted.
	if _, err := GenerateKey(KeyOptions{Email: "x@example.invalid"}); err == nil {
		t.Fatal("a key with no name must be refused")
	}
}

func TestKeyRoundTripsThroughItsArmoredForm(t *testing.T) {
	key := testSigningKey(t)
	private, err := key.ArmoredPrivate()
	if err != nil {
		t.Fatalf("ArmoredPrivate: %v", err)
	}
	if !strings.Contains(string(private), "PGP PRIVATE KEY BLOCK") {
		t.Error("the private key is not in armored form")
	}

	loaded, err := LoadKey(private)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if loaded.Fingerprint() != key.Fingerprint() {
		t.Errorf("fingerprint changed across a round trip: %s then %s",
			key.Fingerprint(), loaded.Fingerprint())
	}
}

func TestLoadKeyRefusesAPublicKey(t *testing.T) {
	// Handing the server a public key would leave it unable to sign, and the failure
	// would otherwise surface later as an unsigned repository.
	key := testSigningKey(t)
	public, err := key.ArmoredPublic()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(public); err == nil {
		t.Fatal("a public key cannot sign and must be refused as a signing key")
	}
}

func TestFingerprintIsFullAndUppercase(t *testing.T) {
	// Short key IDs have been forgeable since 2016, so this is the whole fingerprint.
	got := testSigningKey(t).Fingerprint()
	if len(got) != 40 {
		t.Errorf("fingerprint = %q (%d chars), want 40", got, len(got))
	}
	if got != strings.ToUpper(got) {
		t.Errorf("fingerprint should be uppercase: %q", got)
	}
}

func TestClearSignRoundTrips(t *testing.T) {
	key := testSigningKey(t)
	release := ReleaseFile(ReleaseOptions{Suite: "stable", Origin: "pkgreg"}, nil)

	signed, err := key.ClearSign(release)
	if err != nil {
		t.Fatalf("ClearSign: %v", err)
	}
	// The content stays readable, which is the point of clear-signing over detaching.
	if !strings.Contains(string(signed), "Suite: stable") {
		t.Error("InRelease should still be readable by a person")
	}
	if !strings.Contains(string(signed), "BEGIN PGP SIGNED MESSAGE") {
		t.Error("missing the clear-signed header")
	}

	public, err := key.ArmoredPublic()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := VerifyClearSigned(public, signed)
	if err != nil {
		t.Fatalf("VerifyClearSigned: %v", err)
	}
	if !strings.Contains(string(payload), "Suite: stable") {
		t.Errorf("the verified payload is not what was signed:\n%s", payload)
	}
}

func TestVerifyClearSignedCatchesTampering(t *testing.T) {
	key := testSigningKey(t)
	public, err := key.ArmoredPublic()
	if err != nil {
		t.Fatal(err)
	}
	signed, err := key.ClearSign([]byte("Suite: stable\nArchitectures: amd64\n"))
	if err != nil {
		t.Fatal(err)
	}
	// The attack this exists to stop: a repository whose Release has been edited to point
	// at a different index, and so at different packages.
	tampered := strings.Replace(string(signed), "amd64", "arm64", 1)
	if _, err := VerifyClearSigned(public, []byte(tampered)); err == nil {
		t.Fatal("an edited signed message must not verify")
	}

	other := testSigningKey(t)
	otherPublic, err := other.ArmoredPublic()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyClearSigned(otherPublic, signed); err == nil {
		t.Fatal("a signature must not verify against an unrelated key")
	}
}

func TestDetachSignIsProducedToo(t *testing.T) {
	// Release.gpg alongside InRelease: a repository that serves only one of them fails on
	// exactly the clients nobody tested.
	key := testSigningKey(t)
	signature, err := key.DetachSign([]byte("Suite: stable\n"))
	if err != nil {
		t.Fatalf("DetachSign: %v", err)
	}
	if !strings.Contains(string(signature), "BEGIN PGP SIGNATURE") {
		t.Errorf("not an armored detached signature:\n%s", signature)
	}
}

func TestEd25519KeysWork(t *testing.T) {
	key, err := GenerateKey(KeyOptions{
		Name: "pkgreg test", Email: "t@example.invalid", Algorithm: KeyEd25519,
	})
	if err != nil {
		t.Fatalf("GenerateKey(ed25519): %v", err)
	}
	signed, err := key.ClearSign([]byte("Suite: stable\n"))
	if err != nil {
		t.Fatalf("ClearSign: %v", err)
	}
	public, err := key.ArmoredPublic()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyClearSigned(public, signed); err != nil {
		t.Errorf("an Ed25519 signature should verify: %v", err)
	}
}

func TestUnknownAlgorithmIsRefused(t *testing.T) {
	_, err := GenerateKey(KeyOptions{Name: "x", Algorithm: KeyAlgorithm("dsa")})
	if err == nil {
		t.Fatal("an unknown algorithm must be refused rather than silently defaulted")
	}
}

// TestGPGAcceptsOurSignature is the check that matters: apt does not use this library, it
// shells out to gpgv. A signature that satisfies the code that produced it and nothing
// else would pass every other test here and fail on every machine.
//
// Skipped where gpg is absent rather than failing, because it is not this project's
// dependency — it is the reference implementation being borrowed as an oracle.
func TestGPGAcceptsOurSignature(t *testing.T) {
	gpg, err := exec.LookPath("gpg")
	if err != nil {
		t.Skip("gpg is not installed; skipping the external check")
	}

	key := testSigningKey(t)
	public, err := key.ArmoredPublic()
	if err != nil {
		t.Fatal(err)
	}
	release := ReleaseFile(ReleaseOptions{
		Suite: "stable", Origin: "pkgreg", Architectures: []string{"amd64"},
		Components: []string{"main"},
	}, []IndexFile{{Path: "main/binary-amd64/Packages", Body: []byte("Package: pkgcache\n")}})
	signed, err := key.ClearSign(release)
	if err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	writeFile := func(name string, body []byte) string {
		full := filepath.Join(home, name)
		if err := os.WriteFile(full, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return full
	}
	publicPath := writeFile("key.asc", public)
	signedPath := writeFile("InRelease", signed)

	run := func(args ...string) (string, error) {
		cmd := exec.Command(gpg, append([]string{"--homedir", home, "--batch"}, args...)...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("--import", publicPath); err != nil {
		t.Fatalf("gpg could not import the public key: %v\n%s", err, out)
	}
	out, err := run("--verify", signedPath)
	if err != nil {
		t.Fatalf("gpg rejected a signature this package produced: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Good signature") {
		t.Errorf("gpg did not report a good signature:\n%s", out)
	}
}
