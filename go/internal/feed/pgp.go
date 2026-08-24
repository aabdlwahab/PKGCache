package feed

import (
	"bytes"
	"crypto"
	"fmt"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// The repository signing key.
//
// apt's trust model is one signature over one file. Release names every index and its
// hash, each index names every package and its hash, so a signature over Release is a
// signature over the whole repository — and the key that makes it is the most valuable
// thing this system holds. Anybody who has it can hand every machine in the organisation
// a package of their choosing.
//
// This is done with a library rather than by hand, which is a deliberate exception to how
// the rest of this project treats dependencies. OpenPGP is a packet format with key IDs,
// fingerprints, subpacket tables and MPI encodings, and a bug anywhere in it produces
// either a repository nobody can install from or, far worse, one whose signatures do not
// mean what they appear to. It is not a place to save three modules.

// KeyAlgorithm chooses what kind of signing key to make.
type KeyAlgorithm string

const (
	// KeyRSA is the conservative choice, and the default. Every apt and every gpgv that
	// has ever shipped reads it.
	KeyRSA KeyAlgorithm = "rsa"
	// KeyEd25519 is smaller and instant to generate, and needs GnuPG 2.1 or newer —
	// 2014, so every supported Ubuntu. Offered for an operator who wants it, not assumed.
	KeyEd25519 KeyAlgorithm = "ed25519"
)

// KeyOptions describes a key to generate.
type KeyOptions struct {
	// Name and Email identify the key in a listing. They are what somebody sees when
	// they run `apt-key list` or inspect what they have trusted, so they should say which
	// server this is rather than who ran the command.
	Name  string
	Email string
	// Algorithm defaults to KeyRSA.
	Algorithm KeyAlgorithm
	// RSABits defaults to 4096. Ignored for Ed25519.
	RSABits int
	// Created fixes the key's creation time. Tests set it; leaving it zero means now.
	Created time.Time
}

// PGPKey is a repository signing key.
type PGPKey struct{ entity *openpgp.Entity }

// GenerateKey makes a new repository signing key.
//
// Generated once per instance and then kept, so a slow RSA keygen is paid for on the day
// somebody sets the server up and never again.
func GenerateKey(options KeyOptions) (*PGPKey, error) {
	if options.Name == "" {
		return nil, fmt.Errorf("feed: a signing key needs a name somebody will recognise")
	}
	config := &packet.Config{
		DefaultHash:   crypto.SHA256,
		RSABits:       options.RSABits,
		DefaultCipher: packet.CipherAES256,
	}
	switch options.Algorithm {
	case KeyEd25519:
		config.Algorithm = packet.PubKeyAlgoEdDSA
	case KeyRSA, "":
		config.Algorithm = packet.PubKeyAlgoRSA
		if config.RSABits == 0 {
			config.RSABits = 4096
		}
	default:
		return nil, fmt.Errorf("feed: unknown key algorithm %q", options.Algorithm)
	}
	if !options.Created.IsZero() {
		config.Time = func() time.Time { return options.Created }
	}

	entity, err := openpgp.NewEntity(options.Name, "pkgreg repository signing key",
		options.Email, config)
	if err != nil {
		return nil, fmt.Errorf("feed: generate signing key: %w", err)
	}
	return &PGPKey{entity: entity}, nil
}

// LoadKey reads a key back from its armored private form.
func LoadKey(armored []byte) (*PGPKey, error) {
	entities, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(armored))
	if err != nil {
		return nil, fmt.Errorf("feed: read signing key: %w", err)
	}
	for _, entity := range entities {
		if entity.PrivateKey != nil {
			return &PGPKey{entity: entity}, nil
		}
	}
	return nil, fmt.Errorf(
		"feed: that is a public key, not a signing key — the private half is needed to sign")
}

// Fingerprint is the key's full fingerprint, uppercase and unspaced.
//
// This is what an operator reads out to somebody configuring a machine by hand, and what
// `pkgreg doctor` compares against, so it is the whole fingerprint rather than the short
// key ID that has been forgeable since 2016.
func (k *PGPKey) Fingerprint() string {
	return strings.ToUpper(fmt.Sprintf("%x", k.entity.PrimaryKey.Fingerprint))
}

// ArmoredPublic is the key a client installs to trust this repository.
func (k *PGPKey) ArmoredPublic() ([]byte, error) {
	var out bytes.Buffer
	writer, err := armor.Encode(&out, openpgp.PublicKeyType, nil)
	if err != nil {
		return nil, err
	}
	if err := k.entity.Serialize(writer); err != nil {
		return nil, fmt.Errorf("feed: serialize public key: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// ArmoredPrivate is the key the operator keeps, and the only copy of it.
func (k *PGPKey) ArmoredPrivate() ([]byte, error) {
	var out bytes.Buffer
	writer, err := armor.Encode(&out, openpgp.PrivateKeyType, nil)
	if err != nil {
		return nil, err
	}
	if err := k.entity.SerializePrivate(writer, nil); err != nil {
		return nil, fmt.Errorf("feed: serialize signing key: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// ClearSign wraps a payload in a clear-signed OpenPGP message: InRelease.
//
// The content stays readable, which matters more than it sounds. An operator debugging a
// repository opens InRelease and reads it; a detached signature over an opaque file would
// mean opening two.
func (k *PGPKey) ClearSign(payload []byte) ([]byte, error) {
	var out bytes.Buffer
	writer, err := clearsign.Encode(&out, k.entity.PrivateKey, &packet.Config{
		DefaultHash: crypto.SHA256,
	})
	if err != nil {
		return nil, fmt.Errorf("feed: clear-sign: %w", err)
	}
	if _, err := writer.Write(payload); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("feed: finish clear-signing: %w", err)
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// DetachSign produces the separate signature file: Release.gpg.
//
// Written alongside InRelease rather than instead of it. Modern apt prefers InRelease and
// this is what an older client, or one configured the old way, looks for — and a
// repository that serves only one of them fails on exactly the machines nobody tested.
func (k *PGPKey) DetachSign(payload []byte) ([]byte, error) {
	var out bytes.Buffer
	err := openpgp.ArmoredDetachSign(&out, k.entity, bytes.NewReader(payload),
		&packet.Config{DefaultHash: crypto.SHA256})
	if err != nil {
		return nil, fmt.Errorf("feed: detach-sign: %w", err)
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// VerifyClearSigned checks a clear-signed message against a public key and returns the
// payload it vouches for.
//
// Here so `pkgreg doctor` can ask the question apt will ask, on the server, before a
// machine somewhere fails an update.
func VerifyClearSigned(publicKey, signed []byte) ([]byte, error) {
	block, rest := clearsign.Decode(signed)
	if block == nil {
		return nil, fmt.Errorf("feed: not a clear-signed message")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("feed: trailing data after the signed message")
	}
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(publicKey))
	if err != nil {
		return nil, fmt.Errorf("feed: read public key: %w", err)
	}
	if _, err := openpgp.CheckDetachedSignature(
		keyring, bytes.NewReader(block.Bytes), block.ArmoredSignature.Body, nil,
	); err != nil {
		return nil, fmt.Errorf("feed: signature does not verify: %w", err)
	}
	return block.Plaintext, nil
}
