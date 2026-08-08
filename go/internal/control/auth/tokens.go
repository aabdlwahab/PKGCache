package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/brightskies/pkgreg/internal/control"
)

// Tokens issues and verifies hashed bearer tokens.
type Tokens struct {
	db  *control.DB
	now func() time.Time
}

// NewTokens builds a token service.
func NewTokens(db *control.DB) *Tokens { return &Tokens{db: db, now: time.Now} }

// Issue creates a token and returns its plaintext exactly once.
func (t *Tokens) Issue(project, eco, scope, label, actor string, ttl time.Duration) (control.Token, string, error) {
	return t.IssueWithLimit(project, eco, scope, label, actor, ttl, 0, 0)
}

// IssueWithLimit creates a token with an optional requests/second bucket.
func (t *Tokens) IssueWithLimit(
	project, eco, scope, label, actor string, ttl time.Duration, rate, burst int,
) (control.Token, string, error) {
	switch scope {
	case "read", "write", "admin", "peer":
	default:
		return control.Token{}, "", bad("scope must be read, write, admin, or peer")
	}
	if rate < 0 || burst < 0 {
		return control.Token{}, "", bad("rate_limit and rate_burst cannot be negative")
	}
	id, err := randomToken(12)
	if err != nil {
		return control.Token{}, "", err
	}
	secret, err := randomToken(32)
	if err != nil {
		return control.Token{}, "", err
	}
	now := t.now()
	sum := sha256.Sum256([]byte(secret))
	token := control.Token{
		ID: id, SecretSHA256: hex.EncodeToString(sum[:]), Project: project, Eco: eco,
		Scope: scope, Label: label, CreatedBy: actor, CreatedAt: now,
		RateLimit: rate, RateBurst: burst,
	}
	if ttl > 0 {
		expires := now.Add(ttl)
		token.ExpiresAt = &expires
	}
	if err := t.db.InsertToken(token); err != nil {
		return control.Token{}, "", err
	}
	return token, id + "." + secret, nil
}

// VerifyToken implements the files/data-plane token verifier contract.
func (t *Tokens) VerifyToken(project, eco, scope, presented string) bool {
	_, ok := t.Authenticate(project, eco, scope, presented)
	return ok
}

// scopeRank orders the read/write/admin ladder. A higher rank satisfies every lower
// one, which is what callers expect: a CI credential that can publish to a project can
// obviously also download from it, and requiring two separate tokens for one pipeline
// only encourages issuing admin instead.
//
// peer is deliberately outside the ladder. It authorises digest-addressed sibling
// fetches between instances and is not a degree of user access, so it neither grants
// nor is granted by read, write or admin.
func scopeRank(scope string) (int, bool) {
	switch scope {
	case "read":
		return 1, true
	case "write":
		return 2, true
	case "admin":
		return 3, true
	}
	return 0, false
}

// scopeSatisfies reports whether a token's scope covers the scope being demanded.
func scopeSatisfies(held, required string) bool {
	if held == required {
		return true
	}
	heldRank, heldRanked := scopeRank(held)
	requiredRank, requiredRanked := scopeRank(required)
	return heldRanked && requiredRanked && heldRank >= requiredRank
}

// Authenticate verifies a presentation and returns rate-limit metadata.
func (t *Tokens) Authenticate(
	project, eco, scope, presented string,
) (control.Token, bool) {
	id, secret, found := strings.Cut(strings.TrimSpace(presented), ".")
	if !found || id == "" || secret == "" {
		return control.Token{}, false
	}
	token, err := t.db.Token(id)
	if err != nil {
		return control.Token{}, false
	}
	now := t.now()
	if token.ExpiresAt != nil && !now.Before(*token.ExpiresAt) {
		return control.Token{}, false
	}
	if token.Project != "" && token.Project != project {
		return control.Token{}, false
	}
	if token.Eco != "" && token.Eco != eco {
		return control.Token{}, false
	}
	if !scopeSatisfies(token.Scope, scope) {
		return control.Token{}, false
	}
	sum := sha256.Sum256([]byte(secret))
	expected, err := hex.DecodeString(token.SecretSHA256)
	if err != nil || subtle.ConstantTimeCompare(sum[:], expected) != 1 {
		return control.Token{}, false
	}
	t.db.TouchToken(id, now)
	return token, true
}

// HasToken reports whether any unexpired token can satisfy a scope.
func (t *Tokens) HasToken(project, eco, scope string) bool {
	rows, err := t.db.ListTokens(project)
	if err != nil {
		return false
	}
	now := t.now()
	for _, token := range rows {
		if token.Project != "" && token.Project != project {
			continue
		}
		if token.Eco != "" && token.Eco != eco {
			continue
		}
		if !scopeSatisfies(token.Scope, scope) {
			continue
		}
		if token.ExpiresAt == nil || now.Before(*token.ExpiresAt) {
			return true
		}
	}
	return false
}

// List returns visible token metadata.
func (t *Tokens) List(project string) ([]control.Token, error) {
	return t.db.ListTokens(project)
}

// Get returns token metadata by id.
func (t *Tokens) Get(id string) (control.Token, error) { return t.db.Token(id) }

// Revoke deletes a token.
func (t *Tokens) Revoke(id string) error {
	err := t.db.DeleteToken(id)
	if errors.Is(err, control.ErrNotFound) {
		return control.NewError(http.StatusNotFound, "not_found", "no such token: %s", id)
	}
	return err
}
