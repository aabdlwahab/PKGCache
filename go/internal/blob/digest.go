// Package blob is the content-addressed store: one copy of every distinct
// byte-string on the host, addressed by sha256.
//
// A blob is immutable once linked. Nothing ever rewrites one in place. That single
// invariant is what makes cross-project hardlinking, concurrent garbage collection
// and live snapshots safe without locking anything.
package blob

import (
	"errors"
	"fmt"
	"strings"
)

// Errors returned by this package.
var (
	ErrNotFound      = errors.New("blob: not found")
	ErrInvalidDigest = errors.New("blob: invalid digest")
)

// DigestLen is the length of a sha256 digest in lowercase hex.
const DigestLen = 64

// Digest is a validated lowercase sha256 hex string, with no algorithm prefix.
//
// It is a distinct type because digests are used to build filesystem paths from
// data that ultimately came off the network (an OCI blob URL, a PyPI index hash).
// Only ParseDigest constructs one, and it rejects anything that is not exactly 64
// lowercase hex characters — so a digest can never contain a path separator, a dot
// segment, or a NUL, and path traversal is impossible by construction rather than
// by careful escaping downstream.
type Digest string

// ParseDigest validates s and returns it as a Digest. It accepts both the bare hex
// form and the "sha256:<hex>" form used by OCI and by our own ledger rows. Uppercase
// hex is normalised; any other algorithm is rejected.
func ParseDigest(s string) (Digest, error) {
	if algo, hex, ok := strings.Cut(s, ":"); ok {
		if algo != "sha256" {
			return "", fmt.Errorf("%w: unsupported algorithm %q", ErrInvalidDigest, algo)
		}
		s = hex
	}
	if len(s) != DigestLen {
		return "", fmt.Errorf("%w: want %d hex chars, got %d", ErrInvalidDigest, DigestLen, len(s))
	}
	var b strings.Builder
	b.Grow(DigestLen)
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
			b.WriteByte(c)
		case c >= 'A' && c <= 'F':
			b.WriteByte(c + ('a' - 'A'))
		default:
			return "", fmt.Errorf("%w: non-hex byte %q at %d", ErrInvalidDigest, c, i)
		}
	}
	return Digest(b.String()), nil
}

// parseDigestName is ParseDigest as a predicate, for walking a directory where a name
// that is not a digest is an ordinary thing to skip rather than a failure to report.
func parseDigestName(name string) (Digest, bool) {
	d, err := ParseDigest(name)
	return d, err == nil
}

// MustParseDigest is ParseDigest for compile-time-known values (tests, constants).
func MustParseDigest(s string) Digest {
	d, err := ParseDigest(s)
	if err != nil {
		panic(err)
	}
	return d
}

// Valid reports whether d was properly constructed. Defence in depth: a Digest read
// back out of the database has not been through ParseDigest.
func (d Digest) Valid() bool {
	if len(d) != DigestLen {
		return false
	}
	for i := range len(d) {
		c := d[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// String returns the bare hex form.
func (d Digest) String() string { return string(d) }

// Prefixed returns the "sha256:<hex>" form that OCI and the ledger schema use.
func (d Digest) Prefixed() string { return "sha256:" + string(d) }
