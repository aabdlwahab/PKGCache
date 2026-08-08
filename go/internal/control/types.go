// Package control owns the durable control-plane state.
//
// Cache metadata remains in catalog.db. Identity, tenants, credentials, jobs and
// audit records live in control.db so an operator can back up or inspect either
// concern independently.
package control

import "time"

// Project is one persisted tenant. The global tenant is implicit and is not stored.
type Project struct {
	Name           string    `json:"name"`
	Owner          string    `json:"owner,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	Offline        bool      `json:"offline"`
	QuotaBytes     int64     `json:"quota_bytes"`
	QuotaArtifacts int64     `json:"quota_artifacts"`
	DataPlaneAuth  string    `json:"data_plane_auth"`
	RateLimit      int       `json:"rate_limit"`
	RateBurst      int       `json:"rate_burst"`
}

// User is a stored account. Salt and Hash are never serialized to API callers.
type User struct {
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	Salt      string    `json:"-"`
	Hash      string    `json:"-"`
	ReportsTo string    `json:"reports_to,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Builtin   bool      `json:"builtin,omitempty"`
}

// Grant is one account's access to one project it does not own.
//
// The owner is deliberately not represented here. Ownership is a column on the project
// and answers "who is responsible"; a grant answers "who else may work on it", and
// keeping the two apart means revoking every grant can never leave a project ownerless.
type Grant struct {
	Project   string    `json:"project"`
	Username  string    `json:"username"`
	Level     string    `json:"level"`
	GrantedBy string    `json:"granted_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Token is a persisted API/data-plane credential. SecretSHA256 is never returned.
type Token struct {
	ID           string     `json:"id"`
	SecretSHA256 string     `json:"-"`
	Project      string     `json:"project,omitempty"`
	Eco          string     `json:"eco,omitempty"`
	Scope        string     `json:"scope"`
	Label        string     `json:"label,omitempty"`
	CreatedBy    string     `json:"created_by,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	LastUsed     *time.Time `json:"last_used,omitempty"`
	RateLimit    int        `json:"rate_limit"`
	RateBurst    int        `json:"rate_burst"`
}

// Upstream is one live origin or peer override.
type Upstream struct {
	ID           int64  `json:"id"`
	Project      string `json:"project,omitempty"`
	Eco          string `json:"eco"`
	Name         string `json:"name"`
	URL          string `json:"url"`
	Kind         string `json:"kind"`
	Priority     int    `json:"priority"`
	Enabled      bool   `json:"enabled"`
	CredentialID *int64 `json:"credential_id,omitempty"`
}

// CredentialRecord is the sealed database representation of an upstream secret.
type CredentialRecord struct {
	ID     int64
	Label  string
	Kind   string
	Sealed []byte
}

// Job is one persisted background operation.
type Job struct {
	ID         int64          `json:"id"`
	Project    string         `json:"project,omitempty"`
	Action     string         `json:"action"`
	Status     string         `json:"status"`
	Params     map[string]any `json:"params,omitempty"`
	StartedAt  *time.Time     `json:"started_at,omitempty"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`
	Error      string         `json:"error,omitempty"`
	Actor      string         `json:"actor,omitempty"`
	Log        string         `json:"log,omitempty"`
}

// AuditRecord is one immutable mutation record.
type AuditRecord struct {
	ID       int64          `json:"id"`
	Time     time.Time      `json:"time"`
	Actor    string         `json:"actor,omitempty"`
	Action   string         `json:"action"`
	Target   string         `json:"target,omitempty"`
	Detail   map[string]any `json:"detail,omitempty"`
	ClientIP string         `json:"client_ip,omitempty"`
}
