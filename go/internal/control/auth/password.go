// Package auth implements control-plane identity, sessions, tokens and policy.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
)

const (
	scryptN    = 1 << 14
	scryptR    = 8
	scryptP    = 1
	scryptSize = 32
	saltSize   = 16
)

// Password hashes and verifies passwords using the Python service's exact scrypt
// parameters, so migrated users.json records remain valid.
type Password struct{}

// Hash returns hex salt and digest.
func (Password) Hash(password string) (saltHex, digestHex string, err error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return "", "", err
	}
	digest, err := deriveScrypt(password, salt, scryptN, scryptR, scryptP, scryptSize)
	if err != nil {
		return "", "", err
	}
	return hex.EncodeToString(salt), hex.EncodeToString(digest), nil
}

// Verify performs a constant-time comparison and rejects malformed records.
func (Password) Verify(password, saltHex, digestHex string) bool {
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return false
	}
	expected, err := hex.DecodeString(digestHex)
	if err != nil || len(expected) != scryptSize {
		return false
	}
	actual, err := deriveScrypt(password, salt, scryptN, scryptR, scryptP, scryptSize)
	return err == nil && subtle.ConstantTimeCompare(actual, expected) == 1
}
