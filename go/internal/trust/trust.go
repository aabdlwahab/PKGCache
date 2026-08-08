// Package trust establishes that a cache is the one you were told about.
//
// The exchange is deliberately small. A client fetches `/api/ca.crt` over an
// unverified connection, refuses to go further unless it matches a SHA-256 fingerprint
// supplied out of band, and from then on verifies every request against it normally —
// signature, dates and host name included. The unverified leg carries no credential and
// a certificate is public, so an attacker in the middle can only serve a CA whose
// fingerprint will not match.
//
// It lives here rather than inside pkgreg-client because two programs now need it: the
// client, to open a session against a team cache, and pkgcache, whose middle tier *is*
// a team cache and which has to trust it before it can ask it for anything.
package trust

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/brightskies/pkgreg/internal/onboarding"
)

// MaxCABytes bounds the one unverified download. A CA is a few kilobytes; anything
// approaching this is not one.
const MaxCABytes = 1 << 20

// ErrUnauthorized marks a refusal the caller can answer by presenting a session.
var ErrUnauthorized = errors.New("server requires authentication")

// Options describes the cache to establish trust in.
type Options struct {
	// Server is the origin, for example https://cache.internal:8443.
	Server string
	// ExpectedSHA256 is the fingerprint you were given through a separate channel.
	// Colons optional.
	ExpectedSHA256 string
	// CAFile supplies the certificate directly, and its fingerprint with it. When both
	// this and ExpectedSHA256 are given they must agree.
	CAFile string
	// Cookie is a raw Cookie header for a control plane that requires a session.
	Cookie string
	// Client overrides the HTTP client used for both legs. Tests set it; a real caller
	// leaves it nil so the unverified leg really is the only unverified one.
	Client *http.Client
}

// Verified is a cache whose identity has been established.
type Verified struct {
	// Base is the parsed, normalised origin.
	Base *url.URL
	// CAPEM is the certificate, verified against the fingerprint.
	CAPEM []byte
	// Fingerprint is its SHA-256 in display form, colon-separated.
	Fingerprint string
	// Client verifies against CAPEM in the ordinary way.
	Client *http.Client
}

// Fetch downloads the server's CA and refuses to return it unless it matches.
func Fetch(ctx context.Context, o Options) (Verified, error) {
	base, err := ParseServer(o.Server)
	if err != nil {
		return Verified{}, err
	}
	if base.Scheme != "https" && o.Client == nil {
		return Verified{}, errors.New(
			"server must use https; the unverified connection exists only to fetch " +
				"fingerprint-pinned CA material")
	}
	expected, caForTLS, err := ExpectedFingerprint(o.ExpectedSHA256, o.CAFile)
	if err != nil {
		return Verified{}, err
	}

	caClient := o.Client
	if caClient == nil {
		switch {
		case len(caForTLS) > 0:
			// A CA was supplied on disk, so even this leg can verify.
			caClient, err = ClientFor(caForTLS)
		default:
			caClient = bootstrapClient()
		}
		if err != nil {
			return Verified{}, err
		}
	}

	caURL := *base
	caURL.Path = path.Join(base.Path, "/api/ca.crt")
	caPEM, err := download(ctx, caClient, caURL.String(), o.Cookie, MaxCABytes)
	if err != nil {
		return Verified{}, fmt.Errorf("download CA: %w", err)
	}
	display, err := onboarding.FingerprintSHA256(caPEM)
	if err != nil {
		return Verified{}, fmt.Errorf("verify downloaded CA: %w", err)
	}
	if Normalize(display) != expected {
		return Verified{}, fmt.Errorf("CA fingerprint mismatch: got %s, want %s",
			display, Display(expected))
	}

	verified := o.Client
	if verified == nil {
		verified, err = ClientFor(caPEM)
		if err != nil {
			return Verified{}, fmt.Errorf("trust verified CA: %w", err)
		}
	}
	return Verified{Base: base, CAPEM: caPEM, Fingerprint: display, Client: verified}, nil
}

// ParseServer normalises an origin and rejects anything carrying more than one.
func ParseServer(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.EscapedPath() != "" && parsed.EscapedPath() != "/") {
		return nil, errors.New(
			"server must be an http(s) origin without credentials, query, or fragment")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return parsed, nil
}

// ExpectedFingerprint resolves what the CA must hash to, from a fingerprint, a file, or
// both — and refuses if the two disagree.
func ExpectedFingerprint(fingerprint, caFile string) (expected string, caPEM []byte, err error) {
	expected = Normalize(fingerprint)
	if caFile != "" {
		caPEM, err = os.ReadFile(caFile)
		if err != nil {
			return "", nil, fmt.Errorf("read CA file: %w", err)
		}
		display, err := onboarding.FingerprintSHA256(caPEM)
		if err != nil {
			return "", nil, fmt.Errorf("fingerprint CA file: %w", err)
		}
		fromFile := Normalize(display)
		if expected == "" {
			expected = fromFile
		} else if expected != fromFile {
			return "", nil, errors.New("CA file does not match the expected fingerprint")
		}
	}
	if len(expected) != 64 {
		return "", nil, errors.New(
			"ca-sha256 must be the 64-hex SHA-256 fingerprint (colons are optional)")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return "", nil, errors.New("ca-sha256 contains non-hexadecimal characters")
	}
	return expected, caPEM, nil
}

// Normalize reduces a fingerprint to bare uppercase hex, so the forms people paste all
// compare equal.
func Normalize(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, ":", "")
	return strings.ReplaceAll(value, " ", "")
}

// Display renders a normalised fingerprint the way a console shows it.
func Display(raw string) string {
	var out strings.Builder
	for i := 0; i+2 <= len(raw); i += 2 {
		if i > 0 {
			out.WriteByte(':')
		}
		out.WriteString(raw[i : i+2])
	}
	return out.String()
}

// ClientFor returns a client that verifies against this CA in addition to the system
// roots.
func ClientFor(caPEM []byte) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if len(caPEM) > 0 {
		roots, err := Pool(caPEM)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = &tls.Config{ // #nosec G402 -- defaults verify certificates.
			MinVersion: tls.VersionTLS12, RootCAs: roots,
		}
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}, nil
}

// Pool returns the system roots plus this CA.
//
// Added to the system pool rather than replacing it, because a cache configured with a
// publicly-trusted certificate and one with a private CA have to work through the same
// client — and a chain whose second endpoint is a public registry has to keep verifying
// normally.
func Pool(caPEM []byte) (*x509.CertPool, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("CA material contains no usable certificate")
	}
	return roots, nil
}

// bootstrapClient is limited to fetching the public CA. Fetch verifies that response
// against the out-of-band fingerprint and builds a normally-verifying client from it.
func bootstrapClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{ // #nosec G402 -- the fingerprint authenticates the only bytes downloaded here.
		MinVersion: tls.VersionTLS12, InsecureSkipVerify: true,
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}

func download(
	ctx context.Context, client *http.Client, target, cookie string, limit int64,
) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return nil, err
	}
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		err := fmt.Errorf("server returned %s: %s",
			response.Status, strings.TrimSpace(string(message)))
		if response.StatusCode == http.StatusUnauthorized {
			err = fmt.Errorf("%w: %w", ErrUnauthorized, err)
		}
		return nil, err
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return payload, nil
}
