package control

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound reports a missing control-plane object.
var ErrNotFound = errors.New("control: not found")

// DB is the SQLite-backed control store.
type DB struct {
	sql *sql.DB
	now func() time.Time
}

// Open opens and migrates a control database.
func Open(path string) (*DB, error) {
	if path == "" {
		return nil, errors.New("control: database path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("control: resolve path: %w", err)
	}
	dsn := "file:" + url.PathEscape(absolute) +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)" +
		"&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("control: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: ping: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &DB{sql: db, now: time.Now}, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// Ping checks database availability.
func (d *DB) Ping() error { return d.sql.Ping() }

// SchemaVersion returns the installed migration version.
func (d *DB) SchemaVersion() (int, error) {
	var version int
	err := d.sql.QueryRow(`SELECT COALESCE(MAX(version), 0)
		FROM control_schema_version`).Scan(&version)
	return version, err
}

// Flag returns one instance flag.
func (d *DB) Flag(key string) (value string, found bool, err error) {
	err = d.sql.QueryRow(`SELECT value FROM instance_flags WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("control: get flag: %w", err)
	}
	return value, true, nil
}

// SetFlag upserts one instance flag.
func (d *DB) SetFlag(key, value string) error {
	_, err := d.sql.Exec(`INSERT INTO instance_flags(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("control: set flag: %w", err)
	}
	return nil
}

// ListProjects returns persisted named projects.
func (d *DB) ListProjects() ([]Project, error) {
	rows, err := d.sql.Query(`SELECT name, COALESCE(owner, ''), created_at, offline,
		quota_bytes, quota_artifacts, data_plane_auth, rate_limit, rate_burst
		FROM projects ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("control: list projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var projects []Project
	for rows.Next() {
		var project Project
		var created int64
		if err := rows.Scan(&project.Name, &project.Owner, &created, &project.Offline,
			&project.QuotaBytes, &project.QuotaArtifacts, &project.DataPlaneAuth,
			&project.RateLimit, &project.RateBurst); err != nil {
			return nil, fmt.Errorf("control: scan project: %w", err)
		}
		project.CreatedAt = fromUnix(created)
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

// Project returns a named project.
func (d *DB) Project(name string) (Project, error) {
	var project Project
	var created int64
	err := d.sql.QueryRow(`SELECT name, COALESCE(owner, ''), created_at, offline,
		quota_bytes, quota_artifacts, data_plane_auth, rate_limit, rate_burst
		FROM projects WHERE name = ?`,
		name).Scan(&project.Name, &project.Owner, &created, &project.Offline,
		&project.QuotaBytes, &project.QuotaArtifacts, &project.DataPlaneAuth,
		&project.RateLimit, &project.RateBurst)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, fmt.Errorf("%w: project %s", ErrNotFound, name)
	}
	if err != nil {
		return Project{}, fmt.Errorf("control: get project: %w", err)
	}
	project.CreatedAt = fromUnix(created)
	return project, nil
}

// CreateProject inserts a project.
func (d *DB) CreateProject(project Project) error {
	if project.CreatedAt.IsZero() {
		project.CreatedAt = d.now()
	}
	if project.DataPlaneAuth == "" {
		project.DataPlaneAuth = "public"
	}
	_, err := d.sql.Exec(`INSERT INTO projects(name, owner, created_at, offline,
		quota_bytes, quota_artifacts, data_plane_auth, rate_limit, rate_burst)
		VALUES (?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?)`,
		project.Name, project.Owner, unix(project.CreatedAt), project.Offline,
		project.QuotaBytes, project.QuotaArtifacts, project.DataPlaneAuth,
		project.RateLimit, project.RateBurst)
	if err != nil {
		return fmt.Errorf("control: create project: %w", err)
	}
	return nil
}

// UpdateProject replaces mutable project fields.
func (d *DB) UpdateProject(project Project) error {
	result, err := d.sql.Exec(`UPDATE projects SET owner = NULLIF(?, ''), offline = ?,
		quota_bytes = ?, quota_artifacts = ?, data_plane_auth = ?,
		rate_limit = ?, rate_burst = ? WHERE name = ?`,
		project.Owner, project.Offline, project.QuotaBytes, project.QuotaArtifacts,
		project.DataPlaneAuth, project.RateLimit, project.RateBurst, project.Name)
	if err != nil {
		return fmt.Errorf("control: update project: %w", err)
	}
	return expectChanged(result, "project "+project.Name)
}

// DeleteProject removes control records but deliberately leaves cached bytes.
func (d *DB) DeleteProject(name string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("control: delete project: %w", err)
	}
	for _, statement := range []string{
		`DELETE FROM tokens WHERE project = ?`,
		`DELETE FROM upstreams WHERE project = ?`,
		`DELETE FROM project_grants WHERE project = ?`,
		`DELETE FROM projects WHERE name = ?`,
	} {
		result, execErr := tx.Exec(statement, name)
		if execErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("control: delete project: %w", execErr)
		}
		if statement == `DELETE FROM projects WHERE name = ?` {
			if changed, _ := result.RowsAffected(); changed == 0 {
				_ = tx.Rollback()
				return fmt.Errorf("%w: project %s", ErrNotFound, name)
			}
		}
	}
	return tx.Commit()
}

// ListGrants returns every grant on one project, oldest first.
func (d *DB) ListGrants(project string) ([]Grant, error) {
	rows, err := d.sql.Query(`SELECT project, username, level, COALESCE(granted_by, ''),
		created_at FROM project_grants WHERE project = ? ORDER BY username`, project)
	if err != nil {
		return nil, fmt.Errorf("control: list grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanGrants(rows)
}

// GrantsFor returns every grant held by one account.
func (d *DB) GrantsFor(username string) ([]Grant, error) {
	rows, err := d.sql.Query(`SELECT project, username, level, COALESCE(granted_by, ''),
		created_at FROM project_grants WHERE username = ? ORDER BY project`, username)
	if err != nil {
		return nil, fmt.Errorf("control: list grants for %s: %w", username, err)
	}
	defer func() { _ = rows.Close() }()
	return scanGrants(rows)
}

// GrantLevel returns the level username holds on project, or "" for none.
//
// It answers with a level rather than a bool because this sits on the authorization
// path for every request against a shared project, and a caller that has to ask twice —
// once for view, once for operate — is a caller that will one day ask only once.
func (d *DB) GrantLevel(project, username string) (string, error) {
	var level string
	err := d.sql.QueryRow(`SELECT level FROM project_grants
		WHERE project = ? AND username = ?`, project, username).Scan(&level)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("control: read grant: %w", err)
	}
	return level, nil
}

// PutGrant inserts or replaces one grant.
func (d *DB) PutGrant(grant Grant) error {
	if grant.CreatedAt.IsZero() {
		grant.CreatedAt = d.now()
	}
	// ON CONFLICT rather than INSERT OR REPLACE: re-granting at a different level is
	// the same relationship changing, so created_at should record when access began,
	// not when it was last adjusted.
	_, err := d.sql.Exec(`INSERT INTO project_grants(project, username, level, granted_by, created_at)
		VALUES (?, ?, ?, NULLIF(?, ''), ?)
		ON CONFLICT(project, username) DO UPDATE SET level = excluded.level,
		  granted_by = excluded.granted_by`,
		grant.Project, grant.Username, grant.Level, grant.GrantedBy, unix(grant.CreatedAt))
	if err != nil {
		return fmt.Errorf("control: put grant: %w", err)
	}
	return nil
}

// DeleteGrant revokes one grant.
func (d *DB) DeleteGrant(project, username string) error {
	result, err := d.sql.Exec(`DELETE FROM project_grants
		WHERE project = ? AND username = ?`, project, username)
	if err != nil {
		return fmt.Errorf("control: delete grant: %w", err)
	}
	return expectChanged(result, "grant on "+project+" for "+username)
}

func scanGrants(rows *sql.Rows) ([]Grant, error) {
	var grants []Grant
	for rows.Next() {
		var grant Grant
		var created int64
		if err := rows.Scan(&grant.Project, &grant.Username, &grant.Level,
			&grant.GrantedBy, &created); err != nil {
			return nil, fmt.Errorf("control: scan grant: %w", err)
		}
		grant.CreatedAt = fromUnix(created)
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// ListUsers returns stored users.
func (d *DB) ListUsers() ([]User, error) {
	rows, err := d.sql.Query(`SELECT username, role, salt, hash,
		COALESCE(reports_to, ''), created_at FROM users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("control: list users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var users []User
	for rows.Next() {
		var user User
		var created int64
		if err := rows.Scan(&user.Username, &user.Role, &user.Salt, &user.Hash,
			&user.ReportsTo, &created); err != nil {
			return nil, fmt.Errorf("control: scan user: %w", err)
		}
		user.CreatedAt = fromUnix(created)
		users = append(users, user)
	}
	return users, rows.Err()
}

// User returns one stored user.
func (d *DB) User(username string) (User, error) {
	var user User
	var created int64
	err := d.sql.QueryRow(`SELECT username, role, salt, hash,
		COALESCE(reports_to, ''), created_at FROM users WHERE username = ?`, username).
		Scan(&user.Username, &user.Role, &user.Salt, &user.Hash, &user.ReportsTo, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("%w: user %s", ErrNotFound, username)
	}
	if err != nil {
		return User{}, fmt.Errorf("control: get user: %w", err)
	}
	user.CreatedAt = fromUnix(created)
	return user, nil
}

// CreateUser inserts an account.
func (d *DB) CreateUser(user User) error {
	if user.CreatedAt.IsZero() {
		user.CreatedAt = d.now()
	}
	_, err := d.sql.Exec(`INSERT INTO users(username, role, salt, hash, reports_to, created_at)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), ?)`, user.Username, user.Role, user.Salt,
		user.Hash, user.ReportsTo, unix(user.CreatedAt))
	if err != nil {
		return fmt.Errorf("control: create user: %w", err)
	}
	return nil
}

// UpdateUser replaces a stored account.
func (d *DB) UpdateUser(user User) error {
	result, err := d.sql.Exec(`UPDATE users SET role = ?, salt = ?, hash = ?,
		reports_to = NULLIF(?, '') WHERE username = ?`, user.Role, user.Salt,
		user.Hash, user.ReportsTo, user.Username)
	if err != nil {
		return fmt.Errorf("control: update user: %w", err)
	}
	return expectChanged(result, "user "+user.Username)
}

// DeleteUser deletes an account and everything that only exists to point at it.
//
// The grants go with it in the same transaction. A row left behind would be re-attached
// silently the day someone recreated an account under the same name, which is the one
// way an access list can grow a member nobody granted.
func (d *DB) DeleteUser(username string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("control: delete user: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM project_grants WHERE username = ?`, username); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("control: delete user grants: %w", err)
	}
	result, err := tx.Exec(`DELETE FROM users WHERE username = ?`, username)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("control: delete user: %w", err)
	}
	if err := expectChanged(result, "user "+username); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// InsertToken persists a token hash.
func (d *DB) InsertToken(token Token) error {
	var expires any
	if token.ExpiresAt != nil {
		expires = unix(*token.ExpiresAt)
	}
	_, err := d.sql.Exec(`INSERT INTO tokens(id, secret_sha256, project, eco, scope,
		label, created_by, created_at, expires_at, rate_limit, rate_burst)
		VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, NULLIF(?, ''),
		NULLIF(?, ''), ?, ?, ?, ?)`,
		token.ID, token.SecretSHA256, token.Project, token.Eco, token.Scope,
		token.Label, token.CreatedBy, unix(token.CreatedAt), expires,
		token.RateLimit, token.RateBurst)
	if err != nil {
		return fmt.Errorf("control: insert token: %w", err)
	}
	return nil
}

// Token returns a token by public id.
func (d *DB) Token(id string) (Token, error) {
	row := d.sql.QueryRow(`SELECT id, secret_sha256, COALESCE(project, ''),
		COALESCE(eco, ''), scope, COALESCE(label, ''), COALESCE(created_by, ''),
		created_at, expires_at, last_used, rate_limit, rate_burst
		FROM tokens WHERE id = ?`, id)
	return scanToken(row)
}

// ListTokens returns token metadata, never hashes.
func (d *DB) ListTokens(project string) ([]Token, error) {
	query := `SELECT id, secret_sha256, COALESCE(project, ''), COALESCE(eco, ''),
		scope, COALESCE(label, ''), COALESCE(created_by, ''), created_at,
		expires_at, last_used, rate_limit, rate_burst FROM tokens`
	var args []any
	if project != "" {
		query += ` WHERE project = ?`
		args = append(args, project)
	}
	query += ` ORDER BY created_at DESC, id`
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("control: list tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var tokens []Token
	for rows.Next() {
		token, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

// TouchToken records successful use.
func (d *DB) TouchToken(id string, at time.Time) {
	_, _ = d.sql.Exec(`UPDATE tokens SET last_used = ? WHERE id = ?`, unix(at), id)
}

// DeleteToken revokes a token.
func (d *DB) DeleteToken(id string) error {
	result, err := d.sql.Exec(`DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("control: delete token: %w", err)
	}
	return expectChanged(result, "token "+id)
}

// CreateCredential persists an opaque sealed credential.
func (d *DB) CreateCredential(record CredentialRecord) (int64, error) {
	result, err := d.sql.Exec(`INSERT INTO credentials(label, kind, sealed)
		VALUES (?, ?, ?)`, record.Label, record.Kind, record.Sealed)
	if err != nil {
		return 0, fmt.Errorf("control: create credential: %w", err)
	}
	return result.LastInsertId()
}

// Credential returns one sealed credential.
func (d *DB) Credential(id int64) (CredentialRecord, error) {
	var record CredentialRecord
	err := d.sql.QueryRow(`SELECT id, label, kind, sealed FROM credentials WHERE id = ?`,
		id).Scan(&record.ID, &record.Label, &record.Kind, &record.Sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialRecord{}, fmt.Errorf("%w: credential %d", ErrNotFound, id)
	}
	if err != nil {
		return CredentialRecord{}, fmt.Errorf("control: get credential: %w", err)
	}
	return record, nil
}

// DeleteCredential removes a sealed credential.
func (d *DB) DeleteCredential(id int64) error {
	result, err := d.sql.Exec(`DELETE FROM credentials WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("control: delete credential: %w", err)
	}
	return expectChanged(result, fmt.Sprintf("credential %d", id))
}

// ListUpstreams returns overrides for a project. An empty project means global.
func (d *DB) ListUpstreams(project string) ([]Upstream, error) {
	rows, err := d.sql.Query(`SELECT id, COALESCE(project, ''), eco, name, url, kind,
		priority, enabled, credential_id FROM upstreams
		WHERE COALESCE(project, '') = ? ORDER BY eco, priority, name`, project)
	if err != nil {
		return nil, fmt.Errorf("control: list upstreams: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var upstreams []Upstream
	for rows.Next() {
		upstream, err := scanUpstream(rows)
		if err != nil {
			return nil, err
		}
		upstreams = append(upstreams, upstream)
	}
	return upstreams, rows.Err()
}

// Upstream returns one project-scoped override.
func (d *DB) Upstream(project string, id int64) (Upstream, error) {
	row := d.sql.QueryRow(`SELECT id, COALESCE(project, ''), eco, name, url, kind,
		priority, enabled, credential_id FROM upstreams WHERE id = ? AND
		COALESCE(project, '') = ?`, id, project)
	upstream, err := scanUpstream(row)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
		return Upstream{}, fmt.Errorf("%w: upstream %d", ErrNotFound, id)
	}
	return upstream, err
}

// AllUpstreams returns every override for snapshot publication.
func (d *DB) AllUpstreams() ([]Upstream, error) {
	rows, err := d.sql.Query(`SELECT id, COALESCE(project, ''), eco, name, url, kind,
		priority, enabled, credential_id FROM upstreams ORDER BY project, eco, priority, name`)
	if err != nil {
		return nil, fmt.Errorf("control: list all upstreams: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var upstreams []Upstream
	for rows.Next() {
		upstream, err := scanUpstream(rows)
		if err != nil {
			return nil, err
		}
		upstreams = append(upstreams, upstream)
	}
	return upstreams, rows.Err()
}

// CreateUpstream inserts an override and returns its id.
func (d *DB) CreateUpstream(upstream Upstream) (int64, error) {
	result, err := d.sql.Exec(`INSERT INTO upstreams(project, eco, name, url, kind,
		priority, enabled, credential_id) VALUES (NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?)`,
		upstream.Project, upstream.Eco, upstream.Name, upstream.URL, upstream.Kind,
		upstream.Priority, upstream.Enabled, upstream.CredentialID)
	if err != nil {
		return 0, fmt.Errorf("control: create upstream: %w", err)
	}
	return result.LastInsertId()
}

// UpdateUpstream replaces an override.
func (d *DB) UpdateUpstream(upstream Upstream) error {
	result, err := d.sql.Exec(`UPDATE upstreams SET name = ?, url = ?, kind = ?,
		priority = ?, enabled = ?, credential_id = ? WHERE id = ? AND
		COALESCE(project, '') = ?`, upstream.Name, upstream.URL, upstream.Kind,
		upstream.Priority, upstream.Enabled, upstream.CredentialID, upstream.ID,
		upstream.Project)
	if err != nil {
		return fmt.Errorf("control: update upstream: %w", err)
	}
	return expectChanged(result, fmt.Sprintf("upstream %d", upstream.ID))
}

// DeleteUpstream deletes an override.
func (d *DB) DeleteUpstream(project string, id int64) error {
	result, err := d.sql.Exec(`DELETE FROM upstreams WHERE id = ? AND
		COALESCE(project, '') = ?`, id, project)
	if err != nil {
		return fmt.Errorf("control: delete upstream: %w", err)
	}
	return expectChanged(result, fmt.Sprintf("upstream %d", id))
}

// CreateJob persists a queued job.
func (d *DB) CreateJob(job Job) (int64, error) {
	params, err := json.Marshal(job.Params)
	if err != nil {
		return 0, fmt.Errorf("control: encode job params: %w", err)
	}
	result, err := d.sql.Exec(`INSERT INTO jobs(project, action, status, params, actor)
		VALUES (NULLIF(?, ''), ?, ?, ?, NULLIF(?, ''))`, job.Project, job.Action,
		job.Status, string(params), job.Actor)
	if err != nil {
		return 0, fmt.Errorf("control: create job: %w", err)
	}
	return result.LastInsertId()
}

// Job returns one job and its persisted log.
func (d *DB) Job(id int64) (Job, error) {
	row := d.sql.QueryRow(`SELECT id, COALESCE(project, ''), action, status,
		COALESCE(params, '{}'), started_at, finished_at, COALESCE(error, ''),
		COALESCE(actor, '') FROM jobs WHERE id = ?`, id)
	job, err := scanJob(row)
	if err != nil {
		return Job{}, err
	}
	job.Log, err = d.JobLog(id)
	return job, err
}

// ListJobs lists newest jobs first.
func (d *DB) ListJobs(limit int) ([]Job, error) {
	query := `SELECT id, COALESCE(project, ''), action, status, COALESCE(params, '{}'),
		started_at, finished_at, COALESCE(error, ''), COALESCE(actor, '')
		FROM jobs ORDER BY id DESC`
	var args []any
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("control: list jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var jobs []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// SetJobStatus updates lifecycle fields.
func (d *DB) SetJobStatus(id int64, status, message string, started, finished *time.Time) error {
	var startValue, finishValue any
	if started != nil {
		startValue = unix(*started)
	}
	if finished != nil {
		finishValue = unix(*finished)
	}
	result, err := d.sql.Exec(`UPDATE jobs SET status = ?, error = NULLIF(?, ''),
		started_at = COALESCE(?, started_at), finished_at = ? WHERE id = ?`,
		status, message, startValue, finishValue, id)
	if err != nil {
		return fmt.Errorf("control: update job: %w", err)
	}
	return expectChanged(result, fmt.Sprintf("job %d", id))
}

// AppendJobLog persists one log line.
func (d *DB) AppendJobLog(id int64, line string) error {
	_, err := d.sql.Exec(`INSERT INTO job_logs(job_id, seq, line)
		VALUES (?, COALESCE((SELECT MAX(seq) + 1 FROM job_logs WHERE job_id = ?), 0), ?)`,
		id, id, line)
	if err != nil {
		return fmt.Errorf("control: append job log: %w", err)
	}
	return nil
}

// JobLog returns the concatenated persisted log.
func (d *DB) JobLog(id int64) (string, error) {
	rows, err := d.sql.Query(`SELECT line FROM job_logs WHERE job_id = ? ORDER BY seq`, id)
	if err != nil {
		return "", fmt.Errorf("control: job log: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var log string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		log += line
	}
	return log, rows.Err()
}

// InterruptJobs marks work from a previous process as failed.
func (d *DB) InterruptJobs() error {
	now := unix(d.now())
	_, err := d.sql.Exec(`UPDATE jobs SET status = 'failed',
		error = 'interrupted by process restart', finished_at = ?
		WHERE status IN ('queued', 'running')`, now)
	return err
}

// AppendAudit records a mutation.
func (d *DB) AppendAudit(record AuditRecord) (int64, error) {
	if record.Time.IsZero() {
		record.Time = d.now()
	}
	detail, err := json.Marshal(record.Detail)
	if err != nil {
		return 0, fmt.Errorf("control: encode audit detail: %w", err)
	}
	result, err := d.sql.Exec(`INSERT INTO audit(ts, actor, action, target, detail, client_ip)
		VALUES (?, NULLIF(?, ''), ?, NULLIF(?, ''), ?, NULLIF(?, ''))`,
		unix(record.Time), record.Actor, record.Action, record.Target, string(detail),
		record.ClientIP)
	if err != nil {
		return 0, fmt.Errorf("control: append audit: %w", err)
	}
	return result.LastInsertId()
}

// ListAudit returns newest records first.
func (d *DB) ListAudit(limit int) ([]AuditRecord, error) {
	query := `SELECT id, ts, COALESCE(actor, ''), action, COALESCE(target, ''),
		COALESCE(detail, '{}'), COALESCE(client_ip, '') FROM audit ORDER BY ts DESC, id DESC`
	var args []any
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("control: list audit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var records []AuditRecord
	for rows.Next() {
		var record AuditRecord
		var timestamp int64
		var detail string
		if err := rows.Scan(&record.ID, &timestamp, &record.Actor, &record.Action,
			&record.Target, &detail, &record.ClientIP); err != nil {
			return nil, err
		}
		record.Time = fromUnix(timestamp)
		_ = json.Unmarshal([]byte(detail), &record.Detail)
		records = append(records, record)
	}
	return records, rows.Err()
}

type rowScanner interface{ Scan(...any) error }

func scanToken(row rowScanner) (Token, error) {
	var token Token
	var created int64
	var expires, used sql.NullInt64
	err := row.Scan(&token.ID, &token.SecretSHA256, &token.Project, &token.Eco,
		&token.Scope, &token.Label, &token.CreatedBy, &created, &expires, &used,
		&token.RateLimit, &token.RateBurst)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, fmt.Errorf("%w: token", ErrNotFound)
	}
	if err != nil {
		return Token{}, fmt.Errorf("control: scan token: %w", err)
	}
	token.CreatedAt = fromUnix(created)
	if expires.Valid {
		value := fromUnix(expires.Int64)
		token.ExpiresAt = &value
	}
	if used.Valid {
		value := fromUnix(used.Int64)
		token.LastUsed = &value
	}
	return token, nil
}

func scanUpstream(row rowScanner) (Upstream, error) {
	var upstream Upstream
	var credential sql.NullInt64
	if err := row.Scan(&upstream.ID, &upstream.Project, &upstream.Eco, &upstream.Name,
		&upstream.URL, &upstream.Kind, &upstream.Priority, &upstream.Enabled,
		&credential); err != nil {
		return Upstream{}, fmt.Errorf("control: scan upstream: %w", err)
	}
	if credential.Valid {
		value := credential.Int64
		upstream.CredentialID = &value
	}
	return upstream, nil
}

func scanJob(row rowScanner) (Job, error) {
	var job Job
	var params string
	var started, finished sql.NullInt64
	err := row.Scan(&job.ID, &job.Project, &job.Action, &job.Status, &params,
		&started, &finished, &job.Error, &job.Actor)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("%w: job", ErrNotFound)
	}
	if err != nil {
		return Job{}, fmt.Errorf("control: scan job: %w", err)
	}
	_ = json.Unmarshal([]byte(params), &job.Params)
	if started.Valid {
		value := fromUnix(started.Int64)
		job.StartedAt = &value
	}
	if finished.Valid {
		value := fromUnix(finished.Int64)
		job.FinishedAt = &value
	}
	return job, nil
}

func expectChanged(result sql.Result, object string) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, object)
	}
	return nil
}

func unix(value time.Time) int64 { return value.UTC().UnixNano() }
func fromUnix(value int64) time.Time {
	return time.Unix(0, value).UTC()
}
