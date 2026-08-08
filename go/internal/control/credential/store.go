// Package credential seals upstream credentials under a host-local key.
package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/brightskies/pkgreg/internal/control"
)

const (
	keySize      = 32
	sealedFormat = 1
)

// Plain is the in-memory credential value. It must never be logged.
type Plain struct {
	Label    string `json:"-"`
	Kind     string `json:"kind"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"`
}

// Store seals and unseals records. AES-256-GCM is used instead of the architecture
// draft's NaCl secretbox so the implementation stays within Go's audited standard
// library while retaining authenticated encryption and random nonces.
type Store struct {
	db   *control.DB
	aead cipher.AEAD
}

// Open loads or creates the 0600 host key.
func Open(db *control.DB, keyPath string) (*Store, error) {
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("credential: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credential: GCM: %w", err)
	}
	return &Store{db: db, aead: aead}, nil
}

// Create seals and persists a credential.
func (s *Store) Create(plain Plain) (int64, error) {
	switch plain.Kind {
	case "basic":
		if plain.Username == "" || plain.Password == "" {
			return 0, errors.New("credential: basic requires username and password")
		}
	case "bearer":
		if plain.Token == "" {
			return 0, errors.New("credential: bearer requires token")
		}
	default:
		return 0, errors.New("credential: kind must be basic or bearer")
	}
	body, err := json.Marshal(plain)
	if err != nil {
		return 0, err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return 0, err
	}
	sealed := make([]byte, 1, 1+len(nonce)+len(body)+s.aead.Overhead())
	sealed[0] = sealedFormat
	sealed = append(sealed, nonce...)
	sealed = s.aead.Seal(sealed, nonce, body, nil)
	return s.db.CreateCredential(control.CredentialRecord{
		Label: plain.Label, Kind: plain.Kind, Sealed: sealed,
	})
}

// Get decrypts one credential.
func (s *Store) Get(id int64) (Plain, error) {
	record, err := s.db.Credential(id)
	if err != nil {
		return Plain{}, err
	}
	if len(record.Sealed) < 1+s.aead.NonceSize() || record.Sealed[0] != sealedFormat {
		return Plain{}, errors.New("credential: unsupported or truncated sealed value")
	}
	nonce := record.Sealed[1 : 1+s.aead.NonceSize()]
	body, err := s.aead.Open(nil, nonce, record.Sealed[1+s.aead.NonceSize():], nil)
	if err != nil {
		return Plain{}, fmt.Errorf("credential: authenticate sealed value: %w", err)
	}
	var plain Plain
	if err := json.Unmarshal(body, &plain); err != nil {
		return Plain{}, fmt.Errorf("credential: decode: %w", err)
	}
	plain.Label = record.Label
	return plain, nil
}

// Delete removes a credential.
func (s *Store) Delete(id int64) error { return s.db.DeleteCredential(id) }

func loadOrCreateKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != keySize {
			return nil, fmt.Errorf("credential: host key must be %d bytes", keySize)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("credential: read host key: %w", err)
	}
	key = make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return loadOrCreateKey(path)
	}
	if err != nil {
		return nil, fmt.Errorf("credential: create host key: %w", err)
	}
	if _, err := file.Write(key); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("credential: write host key: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("credential: sync host key: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return key, nil
}
