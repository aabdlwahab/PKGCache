// Package pki mints the TLS material the cache serves with.
//
// It replaces scripts/gen-certs.sh and, with it, the openssl dependency. Everything
// here is crypto/x509, so a host needs nothing installed to bring up HTTPS — which is
// the point of shipping one binary.
package pki

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// File names inside the certs directory. These match what the shell script produced,
// so an existing deployment's already-distributed trust keeps working.
const (
	CAKeyFile      = "ca.key"
	CACertFile     = "ca.crt"
	ServerKeyFile  = "server.key"
	ServerCertFile = "server.crt"
)

const (
	// Long-lived by default: an air-gapped side has no renewal path, and re-issuing
	// means physically carrying new trust material across the gap.
	defaultValidity = 10 * 365 * 24 * time.Hour
	caKeyBits       = 4096
	serverKeyBits   = 2048

	keyMode  = 0o600
	certMode = 0o644
)

// CA is a local certificate authority.
type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
	Dir  string
}

// LoadOrCreateCA returns the CA in dir, creating one only if it is absent.
//
// Reuse is the important half: the CA certificate is distributed to every build host,
// and minting a fresh one would silently invalidate that trust everywhere. An
// existing key is used as-is whatever its algorithm.
func LoadOrCreateCA(dir string) (*CA, bool, error) {
	certPath := filepath.Join(dir, CACertFile)
	keyPath := filepath.Join(dir, CAKeyFile)

	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)
	switch {
	case certExists && keyExists:
		ca, err := loadCA(dir)
		return ca, false, err
	case certExists != keyExists:
		// Half a CA is worse than none: silently minting a replacement would break
		// every client that already trusts the surviving certificate.
		return nil, false, fmt.Errorf(
			"pki: %s exists without its counterpart in %s — refusing to guess; "+
				"restore the missing file or remove both to mint a new CA",
			map[bool]string{true: CACertFile, false: CAKeyFile}[certExists], dir)
	}

	ca, err := createCA(dir)
	return ca, true, err
}

func loadCA(dir string) (*CA, error) {
	certDER, err := readPEM(filepath.Join(dir, CACertFile), "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("pki: parse CA certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("pki: %s is not a CA certificate", CACertFile)
	}
	key, err := loadPrivateKey(filepath.Join(dir, CAKeyFile))
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, Dir: dir}, nil
}

func createCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("pki: create certs dir: %w", err)
	}
	key, err := rsa.GenerateKey(rand.Reader, caKeyBits)
	if err != nil {
		return nil, fmt.Errorf("pki: generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "pkgreg local CA",
			Organization: []string{"pkgreg"},
		},
		NotBefore:             now.Add(-time.Hour), // tolerate modest clock skew
		NotAfter:              now.Add(defaultValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("pki: self-sign CA: %w", err)
	}
	if err := writePEM(filepath.Join(dir, CACertFile), "CERTIFICATE", der, certMode); err != nil {
		return nil, err
	}
	if err := writeKey(filepath.Join(dir, CAKeyFile), key); err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("pki: parse freshly minted CA: %w", err)
	}
	return &CA{Cert: cert, Key: key, Dir: dir}, nil
}

// IssueServer mints the serving certificate for the given names and writes it beside
// the CA. Existing server material is replaced; only the CA is precious.
//
// The CA private key never leaves this host — it is deliberately not part of an
// air-gap export, because it could mint certificates trusted by every build host.
func (ca *CA) IssueServer(names []string) ([]string, error) {
	dns, ips := splitSANs(names)
	if len(dns) == 0 && len(ips) == 0 {
		return nil, errors.New("pki: at least one subject alternative name is required")
	}
	key, err := rsa.GenerateKey(rand.Reader, serverKeyBits)
	if err != nil {
		return nil, fmt.Errorf("pki: generate server key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	cn := "pkgreg"
	if len(dns) > 0 {
		cn = dns[0]
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"pkgreg"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(defaultValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, key.Public(), ca.Key)
	if err != nil {
		return nil, fmt.Errorf("pki: sign server certificate: %w", err)
	}
	if err := writePEM(filepath.Join(ca.Dir, ServerCertFile), "CERTIFICATE", der, certMode); err != nil {
		return nil, err
	}
	if err := writeKey(filepath.Join(ca.Dir, ServerKeyFile), key); err != nil {
		return nil, err
	}
	return describeSANs(dns, ips), nil
}

// DiscoverSANs returns the names clients plausibly reach this host by: loopback, the
// hostname and its FQDN, every non-loopback interface address, plus `extra`.
//
// A certificate is trusted for a name only if that name is in its SANs, and the most
// common bring-up failure is minting for the hostname while everyone connects by IP.
func DiscoverSANs(ctx context.Context, extra ...string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}

	add("localhost")
	add("127.0.0.1")
	add("::1")
	if h, err := os.Hostname(); err == nil {
		add(h)
		// Bounded deliberately: minting a certificate must not stall on a slow or
		// unreachable resolver, and a missing reverse record only costs one SAN.
		lookupCtx, cancelLookup := context.WithTimeout(ctx, 2*time.Second)
		names, err := net.DefaultResolver.LookupAddr(lookupCtx, h)
		cancelLookup()
		if err == nil {
			for _, n := range names {
				add(strings.TrimSuffix(n, "."))
			}
		}
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				add(ipn.IP.String())
			}
		}
	}
	for _, e := range extra {
		add(e)
	}
	return out
}

// splitSANs partitions names into DNS names and IP addresses, deduplicated and
// ordered so a re-issue for the same inputs produces the same SAN list.
func splitSANs(names []string) ([]string, []net.IP) {
	var dns []string
	var ips []net.IP
	seenDNS := map[string]bool{}
	seenIP := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if ip := net.ParseIP(n); ip != nil {
			if !seenIP[ip.String()] {
				seenIP[ip.String()] = true
				ips = append(ips, ip)
			}
			continue
		}
		n = strings.ToLower(strings.TrimSuffix(n, "."))
		if !seenDNS[n] {
			seenDNS[n] = true
			dns = append(dns, n)
		}
	}
	sort.Strings(dns)
	// Put localhost first so it becomes the CommonName: stable, and always correct.
	if i := slices.Index(dns, "localhost"); i > 0 {
		dns[0], dns[i] = dns[i], dns[0]
		sort.Strings(dns[1:])
	}
	sort.Slice(ips, func(a, b int) bool { return ips[a].String() < ips[b].String() })
	return dns, ips
}

func describeSANs(dns []string, ips []net.IP) []string {
	out := make([]string, 0, len(dns)+len(ips))
	for _, d := range dns {
		out = append(out, "DNS:"+d)
	}
	for _, ip := range ips {
		out = append(out, "IP:"+ip.String())
	}
	return out
}

// ---- file helpers --------------------------------------------------------

func randomSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("pki: generate serial: %w", err)
	}
	return n, nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	buf := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if buf == nil {
		return fmt.Errorf("pki: encode %s", blockType)
	}
	// Atomic: a half-written certificate would break every client at once.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, mode); err != nil {
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pki: install %s: %w", path, err)
	}
	return nil
}

func writeKey(path string, key crypto.Signer) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("pki: marshal private key: %w", err)
	}
	return writePEM(path, "PRIVATE KEY", der, keyMode)
}

func readPEM(path, wantType string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pki: read %s: %w", path, err)
	}
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("pki: no %s block in %s", wantType, path)
		}
		if block.Type == wantType {
			return block.Bytes, nil
		}
	}
}

// loadPrivateKey accepts PKCS#8, PKCS#1 and SEC1, because an existing deployment's
// ca.key was produced by `openssl genrsa` and must keep working unchanged.
func loadPrivateKey(path string) (crypto.Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pki: read %s: %w", path, err)
	}
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("pki: no private key found in %s", path)
		}
		if !strings.Contains(block.Type, "PRIVATE KEY") {
			continue
		}
		if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
			signer, ok := k.(crypto.Signer)
			if !ok {
				return nil, fmt.Errorf("pki: %s holds an unusable key type %T", path, k)
			}
			return signer, nil
		}
		if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
			return k, nil
		}
		if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
			return k, nil
		}
		return nil, fmt.Errorf("pki: %s is not a supported private key format", path)
	}
}
