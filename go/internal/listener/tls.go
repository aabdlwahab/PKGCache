// Package listener owns network-listener mechanics: first-byte protocol
// multiplexing and atomically reloadable TLS certificates.
package listener

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"sync/atomic"
)

// Certificates serves one certificate pair and can replace it without disturbing
// established TLS connections. Every new ClientHello loads the current pointer.
type Certificates struct {
	certFile string
	keyFile  string
	current  atomic.Pointer[tls.Certificate]
}

// LoadCertificates loads a serving pair.
func LoadCertificates(certFile, keyFile string) (*Certificates, error) {
	c := &Certificates{certFile: certFile, keyFile: keyFile}
	if err := c.Reload(); err != nil {
		return nil, err
	}
	return c, nil
}

// Reload parses both files completely before publishing the new pair. A malformed
// replacement leaves the last known-good certificate active.
func (c *Certificates) Reload() error {
	pair, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return fmt.Errorf("listener: load TLS certificate: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return fmt.Errorf("listener: TLS certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("listener: parse TLS leaf: %w", err)
	}
	pair.Leaf = leaf
	c.current.Store(&pair)
	return nil
}

// TLSConfig returns a server configuration that selects the current certificate
// for every SNI name covered by its SANs.
func (c *Certificates) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The listener feeds net/http through Server.Serve rather than ServeTLS so
		// certificate reload remains under our GetCertificate hook. Advertise only
		// the protocol that server is guaranteed to speak.
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			pair := c.current.Load()
			if pair == nil {
				return nil, fmt.Errorf("listener: no TLS certificate loaded")
			}
			return pair, nil
		},
	}
}
