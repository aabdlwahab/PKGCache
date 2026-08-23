// Package frompython imports the retired Python cache layout into the Go catalog.
//
// Source files are never renamed or removed, so the Python stack may continue to
// serve during the bulk pass. Re-running performs a short incremental pass before
// cutover. Progress is committed to a separate SQLite database after every durable
// destination publication, making an interrupted migration resumable.
package frompython

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/control"
	_ "modernc.org/sqlite"
)

const globalProject = "global"

var roleEcosystem = map[string]string{
	"docker": "oci",
	"npm":    "npm",
	"pip":    "pypi",
	"apt":    "apt",
	"git":    "git",
	"files":  "files",
}

// Options controls one source-to-destination pass.
type Options struct {
	SourceDir string
	DataDir   string
	ConfigDir string
	Progress  func(string)
	SkipUsers bool
	SkipGit   bool
	Strict    bool
}

// Report summarizes work done in this pass. Skipped items were already committed
// by an earlier pass with the same source size and modification time.
type Report struct {
	Projects        []string `json:"projects"`
	CASBlobs        int64    `json:"cas_blobs"`
	LinkedBytes     int64    `json:"linked_bytes"`
	HashedFiles     int64    `json:"hashed_files"`
	Entries         int64    `json:"entries"`
	Artifacts       int64    `json:"artifacts"`
	Refs            int64    `json:"refs"`
	ManagedFiles    int64    `json:"managed_files"`
	Skipped         int64    `json:"skipped"`
	Warnings        []string `json:"warnings,omitempty"`
	Elapsed         string   `json:"elapsed"`
	NeedsCheckpoint []string `json:"-"`
}

type importer struct {
	ctx     context.Context
	options Options
	blobs   *blob.Store
	catalog *catalog.DB
	control *control.DB
	state   *progressDB
	report  Report
	started time.Time
	casRoot string
}

type legacyRegistry struct {
	Projects map[string]json.RawMessage `json:"projects"`
	Owners   map[string]string          `json:"owners"`
	Offline  map[string]bool            `json:"offline"`
}

type sourceRoot struct {
	project string
	path    string
}

// Run performs one resumable migration pass.
func Run(ctx context.Context, options Options) (Report, error) {
	if options.SourceDir == "" || options.DataDir == "" {
		return Report{}, errors.New("frompython: source and data directories are required")
	}
	source, err := filepath.Abs(options.SourceDir)
	if err != nil {
		return Report{}, err
	}
	destination, err := filepath.Abs(options.DataDir)
	if err != nil {
		return Report{}, err
	}
	if sameOrNested(source, destination) || sameOrNested(destination, source) {
		return Report{}, errors.New("frompython: source and destination must not contain one another")
	}
	options.SourceDir, options.DataDir = source, destination
	if options.ConfigDir == "" {
		options.ConfigDir = filepath.Join(filepath.Dir(source), "config")
	}

	for _, directory := range []string{
		filepath.Join(destination, "db"),
		filepath.Join(destination, "migration"),
		filepath.Join(destination, "config"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return Report{}, err
		}
	}
	blobStore, err := blob.Open(destination)
	if err != nil {
		return Report{}, err
	}
	cat, err := catalog.Open(catalog.Options{
		Path: filepath.Join(destination, "db", "catalog.db"),
	})
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = cat.Close() }()
	controlDB, err := control.Open(filepath.Join(destination, "db", "control.db"))
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = controlDB.Close() }()
	state, err := openProgress(ctx, filepath.Join(destination, "migration", "from-python.db"))
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = state.Close() }()

	m := &importer{
		ctx: ctx, options: options, blobs: blobStore, catalog: cat,
		control: controlDB, state: state, started: time.Now(),
		casRoot: filepath.Join(source, ".cas", "sha256"),
	}
	if err := m.run(); err != nil {
		m.finish()
		return m.report, err
	}
	m.finish()
	return m.report, nil
}

func (m *importer) run() error {
	m.log("discovering projects and immutable CAS")
	registry, err := m.readRegistry()
	if err != nil {
		return err
	}
	roots, err := m.projectRoots(registry)
	if err != nil {
		return err
	}
	if err := m.importCAS(); err != nil {
		return err
	}
	for _, root := range roots {
		if err := m.ctx.Err(); err != nil {
			return err
		}
		m.log("importing project " + root.project)
		for _, role := range []string{"docker", "npm", "pip", "apt", "git", "files"} {
			roleRoot := filepath.Join(root.path, role)
			if info, err := os.Stat(roleRoot); err != nil || !info.IsDir() {
				continue
			}
			if err := m.importLedger(root.project, role, roleRoot); err != nil {
				return err
			}
			if role == "git" && !m.options.SkipGit {
				if err := m.importManagedGit(root.project, roleRoot); err != nil {
					return err
				}
			}
			if err := m.importTree(root.project, role, roleRoot); err != nil {
				return err
			}
			if role == "docker" {
				if err := m.materializeOCITagEntries(root.project); err != nil {
					return err
				}
			}
		}
	}
	if !m.options.SkipUsers {
		if err := m.copyLegacyUsers(); err != nil {
			return err
		}
	}
	if err := m.catalog.Flush(); err != nil {
		return err
	}
	for _, project := range m.report.Projects {
		head, err := m.catalog.GetHead(project)
		if err != nil {
			return err
		}
		if head == "" {
			m.report.NeedsCheckpoint = append(m.report.NeedsCheckpoint, project)
		}
	}
	return nil
}

func (m *importer) finish() {
	m.report.Elapsed = time.Since(m.started).Round(time.Millisecond).String()
	sort.Strings(m.report.Projects)
}

func (m *importer) readRegistry() (legacyRegistry, error) {
	registry := legacyRegistry{
		Projects: make(map[string]json.RawMessage),
		Owners:   make(map[string]string), Offline: make(map[string]bool),
	}
	body, err := os.ReadFile(filepath.Join(m.options.ConfigDir, "projects.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return registry, nil
	}
	if err != nil {
		return registry, fmt.Errorf("frompython: read projects registry: %w", err)
	}
	if err := json.Unmarshal(body, &registry); err != nil {
		return registry, fmt.Errorf("frompython: parse projects registry: %w", err)
	}
	return registry, nil
}

func (m *importer) projectRoots(registry legacyRegistry) ([]sourceRoot, error) {
	roots := []sourceRoot{{project: globalProject, path: m.options.SourceDir}}
	names := make(map[string]bool)
	for name := range registry.Projects {
		names[name] = true
	}
	projectDir := filepath.Join(m.options.SourceDir, "projects")
	entries, err := os.ReadDir(projectDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			names[entry.Name()] = true
		}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		if safeSegment(name) {
			sorted = append(sorted, name)
		} else {
			m.warn("ignored unsafe project name " + name)
		}
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		project := control.Project{
			Name: name, Owner: registry.Owners[name], Offline: registry.Offline[name],
			DataPlaneAuth: "public",
		}
		if _, err := m.control.Project(name); errors.Is(err, control.ErrNotFound) {
			if err := m.control.CreateProject(project); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
		roots = append(roots, sourceRoot{
			project: name, path: filepath.Join(projectDir, name),
		})
	}
	m.report.Projects = make([]string, 0, len(roots))
	for _, root := range roots {
		m.report.Projects = append(m.report.Projects, root.project)
	}
	return roots, nil
}

// importCAS is the critical fast path: the digest is the filename, so no content is
// read. ImportKnown hardlinks it when the two layouts share a filesystem.
func (m *importer) importCAS() error {
	info, err := os.Stat(m.casRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() {
		return err
	}
	var seen int64
	return filepath.WalkDir(m.casRoot, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := m.ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		// The legacy CAS tree holds its objects under their digest. Anything else in
		// there — a lock file, an editor backup — is not content to import.
		digest, err := blob.ParseDigest(entry.Name())
		if err != nil {
			return nil //nolint:nilerr // a non-digest filename is not a CAS object
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		id := "cas:" + digest.String()
		if m.state.Done(id, info) {
			m.report.Skipped++
			return nil
		}
		stat, err := m.blobs.ImportKnown(digest, name)
		if err != nil {
			return err
		}
		if err := m.state.Mark(id, info, digest.String()); err != nil {
			return err
		}
		m.report.CASBlobs++
		m.report.LinkedBytes += stat.Size
		seen++
		if seen%10000 == 0 {
			m.log(fmt.Sprintf("linked %d CAS blobs", seen))
		}
		return nil
	})
}

type legacyArtifact struct {
	Ecosystem string
	Name      string
	Version   string
	Digest    sql.NullString
	Size      sql.NullInt64
	Origin    sql.NullString
	Path      sql.NullString
	Arch      sql.NullString
	CachedAt  string
	Extra     sql.NullString
}

func (m *importer) importLedger(project, role, root string) error {
	ledgerPath := filepath.Join(root, "ledger.db")
	if _, err := os.Stat(ledgerPath); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	db, err := openLegacyLedger(m.ctx, ledgerPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// The ledger connection pool holds a single connection, so the artifact cursor must
	// be fully closed before refs and stats can query the same database.
	if err := m.importArtifacts(project, role, root, ledgerPath, db); err != nil {
		return err
	}
	if err := m.importRefs(project, role, db); err != nil {
		return err
	}
	return m.importStats(project, ledgerPath, db)
}

func (m *importer) importArtifacts(project, role, root, ledgerPath string, db *sql.DB) error {
	rows, err := db.QueryContext(m.ctx, `SELECT ecosystem, name, version, digest, size,
		origin, path, arch, cached_at, extra FROM artifacts ORDER BY id`)
	if err != nil {
		return fmt.Errorf("frompython: query %s: %w", ledgerPath, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var row legacyArtifact
		if err := rows.Scan(
			&row.Ecosystem, &row.Name, &row.Version, &row.Digest, &row.Size,
			&row.Origin, &row.Path, &row.Arch, &row.CachedAt, &row.Extra,
		); err != nil {
			return fmt.Errorf("frompython: scan %s: %w", ledgerPath, err)
		}
		if err := m.importArtifact(project, role, root, row); err != nil {
			if m.options.Strict {
				return err
			}
			// Warn and carry on. Closing the cursor here — as this loop used to —
			// ended the scan, so one stale row silently dropped every artifact after
			// it, in the default non-strict mode the runbook uses.
			m.warn(err.Error())
		}
	}
	// A driver failure part-way through ends Next() without returning an error of its
	// own, and Close() does not report it either. Unchecked, a truncated scan is
	// indistinguishable from a complete one and the migration claims success.
	if err := rows.Err(); err != nil {
		return fmt.Errorf("frompython: iterate %s artifacts: %w", ledgerPath, err)
	}
	return nil
}

func (m *importer) importArtifact(project, role, root string, row legacyArtifact) error {
	ecoID := translateEcosystem(row.Ecosystem)
	if ecoID == "" {
		ecoID = roleEcosystem[role]
	}
	cached := parseTime(row.CachedAt, time.Now())
	artifact := catalog.Artifact{
		Project: project, Eco: ecoID, Name: row.Name, Version: row.Version,
		Arch: row.Arch.String, Size: row.Size.Int64, Origin: row.Origin.String,
		CachedAt: cached,
	}
	if row.Extra.Valid {
		_ = json.Unmarshal([]byte(row.Extra.String), &artifact.Extra)
	}
	digest, digestOK := parseDigest(row.Digest.String)
	if digestOK {
		artifact.Digest = digest
	}
	if !row.Path.Valid || row.Path.String == "" {
		id := fmt.Sprintf("artifact:%s:%s:%s:%s:%s", project, ecoID,
			row.Name, row.Version, row.Arch.String)
		if m.state.LogicalDone(id) {
			m.report.Skipped++
			return nil
		}
		if err := m.catalog.PutArtifact(artifact); err != nil {
			return err
		}
		if err := m.state.MarkLogical(id, digest.String()); err != nil {
			return err
		}
		m.report.Artifacts++
		return nil
	}
	source, err := secureJoin(root, row.Path.String)
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("frompython: artifact %s/%s source %s: %w",
			ecoID, row.Name, source, err)
	}
	keys := entryKeys(role, row.Path.String, digest)
	if len(keys) == 0 {
		return fmt.Errorf("frompython: cannot derive key for %s", source)
	}
	for index, key := range keys {
		var attached *catalog.Artifact
		if index == 0 {
			attached = &artifact
		}
		if err := m.importEntry(project, ecoID, key, source, info, digest, digestOK, cached, attached); err != nil {
			return err
		}
	}
	return nil
}

func (m *importer) importEntry(
	project, ecoID, key, source string,
	info fs.FileInfo,
	known blob.Digest,
	knownOK bool,
	cached time.Time,
	artifact *catalog.Artifact,
) error {
	id := "entry:" + project + ":" + ecoID + ":" + key
	if m.state.Done(id, info) {
		m.report.Skipped++
		return nil
	}
	digest := known
	size := info.Size()
	if knownOK {
		if _, exists := m.blobs.Stat(digest); !exists {
			stat, err := m.blobs.ImportKnown(digest, source)
			if err != nil {
				return err
			}
			size = stat.Size
			m.report.LinkedBytes += size
		}
	} else {
		var err error
		digest, size, err = m.hashIntoStore(source)
		if err != nil {
			return err
		}
		m.report.HashedFiles++
	}
	if cached.IsZero() {
		cached = info.ModTime()
	}
	entry := catalog.Entry{
		EntryKey: catalog.EntryKey{Project: project, Eco: ecoID, Key: key},
		Digest:   digest, Size: size, CachedAt: cached, LastAccess: cached,
	}
	if err := m.catalog.CommitEntry(entry, artifact, catalog.Quota{}, ecoID == "files"); err != nil {
		return err
	}
	if err := m.state.Mark(id, info, digest.String()); err != nil {
		return err
	}
	m.report.Entries++
	if artifact != nil {
		m.report.Artifacts++
	}
	return nil
}

func (m *importer) hashIntoStore(source string) (blob.Digest, int64, error) {
	in, err := os.Open(source)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = in.Close() }()
	before, err := in.Stat()
	if err != nil {
		return "", 0, err
	}
	writer, err := m.blobs.Create()
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = writer.Abort() }()
	if _, err := io.CopyBuffer(writer, in, make([]byte, 1<<20)); err != nil {
		return "", 0, err
	}
	after, err := in.Stat()
	if err != nil {
		return "", 0, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", 0, fmt.Errorf("frompython: source changed during read: %s", source)
	}
	return writer.CommitImported()
}

func (m *importer) importTree(project, role, root string) error {
	ecoID := roleEcosystem[role]
	return filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := m.ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			slash := filepath.ToSlash(relative)
			if relative == ".git" || relative == ".dvc" ||
				(role == "git" && relative != "." && slash != "blobs" &&
					!strings.HasPrefix(slash, "blobs/")) {
				return filepath.SkipDir
			}
			return nil
		}
		if skipLegacyFile(relative) {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		keys, known := treeKeys(role, filepath.ToSlash(relative))
		for _, key := range keys {
			if err := m.importEntry(
				project, ecoID, key, name, info, known, known.Valid(),
				info.ModTime(), nil,
			); err != nil {
				if m.options.Strict {
					return err
				}
				m.warn(err.Error())
			}
		}
		return nil
	})
}

func (m *importer) importManagedGit(project, root string) error {
	targetRoot, err := m.blobs.ManagedDir("git", project)
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := m.ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(relative)
		if relative == "." {
			return nil
		}
		if slash == "blobs" || strings.HasPrefix(slash, "blobs/") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if skipLegacyFile(relative) {
			return nil
		}
		target := filepath.Join(targetRoot, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		id := "managed:" + project + ":" + slash
		if m.state.Done(id, info) {
			m.report.Skipped++
			return nil
		}
		if err := linkOrCopy(name, target, info.Mode()); err != nil {
			return err
		}
		if err := m.state.Mark(id, info, ""); err != nil {
			return err
		}
		m.report.ManagedFiles++
		return nil
	})
}

// importRefs carries the mutable pointers across: OCI tags and Git branch heads.
//
// Losing one is not cosmetic. An OCI tag that fails to migrate leaves the offline side
// unable to resolve `docker pull repo:tag` at all, which is why each loop below checks
// rows.Err(): a truncated scan must be an error, not a quietly shorter migration.
func (m *importer) importRefs(project, role string, db *sql.DB) error {
	switch role {
	case "docker":
		return m.importOCITagRefs(project, db)
	case "git":
		return m.importGitRefs(project, db)
	}
	return nil
}

func (m *importer) importOCITagRefs(project string, db *sql.DB) error {
	if !tableExists(m.ctx, db, "oci_tags") {
		return nil
	}
	rows, err := db.QueryContext(m.ctx,
		`SELECT upstream, repo, tag, digest, media_type, fetched_at FROM oci_tags`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var upstream, repo, tag, rawDigest string
		var media, fetched sql.NullString
		if err := rows.Scan(&upstream, &repo, &tag, &rawDigest, &media, &fetched); err != nil {
			return err
		}
		digest, ok := parseDigest(rawDigest)
		if !ok {
			continue
		}
		name := "tag/" + upstream + "/" + repo + "/" + tag
		id := "ref:" + project + ":oci:" + name
		if m.state.LogicalDone(id) {
			m.report.Skipped++
			continue
		}
		if err := m.catalog.PutRef(catalog.Ref{
			RefKey: catalog.RefKey{Project: project, Eco: "oci", Name: name},
			Target: "manifest/" + digest.String(), MediaType: media.String,
			FetchedAt: parseTime(fetched.String, time.Now()),
		}); err != nil {
			return err
		}
		if err := m.state.MarkLogical(id, digest.String()); err != nil {
			return err
		}
		m.report.Refs++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("frompython: iterate oci_tags: %w", err)
	}
	return nil
}

func (m *importer) importGitRefs(project string, db *sql.DB) error {
	if !tableExists(m.ctx, db, "git_refs") {
		return nil
	}
	rows, err := db.QueryContext(m.ctx,
		`SELECT repo, ref, commit_sha, fetched_at FROM git_refs`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var repo, ref, commit string
		var fetched sql.NullString
		if err := rows.Scan(&repo, &ref, &commit, &fetched); err != nil {
			return err
		}
		name := repo + "/" + ref
		id := "ref:" + project + ":git:" + name
		if m.state.LogicalDone(id) {
			m.report.Skipped++
			continue
		}
		if err := m.catalog.PutRef(catalog.Ref{
			RefKey: catalog.RefKey{Project: project, Eco: "git", Name: name},
			Target: commit, FetchedAt: parseTime(fetched.String, time.Now()),
		}); err != nil {
			return err
		}
		if err := m.state.MarkLogical(id, commit); err != nil {
			return err
		}
		m.report.Refs++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("frompython: iterate git_refs: %w", err)
	}
	return nil
}

func (m *importer) materializeOCITagEntries(project string) error {
	refs, err := m.catalog.ListRefs(project, "oci", "tag/")
	if err != nil {
		return err
	}
	for _, ref := range refs {
		tagKey := catalog.EntryKey{Project: project, Eco: "oci", Key: ref.Name}
		if _, err := m.catalog.GetEntry(tagKey); err == nil {
			continue
		}
		manifestKey := strings.TrimPrefix(ref.Target, "manifest/")
		digest, ok := parseDigest(manifestKey)
		if !ok {
			continue
		}
		manifest, err := m.catalog.GetEntry(catalog.EntryKey{
			Project: project, Eco: "oci", Key: "manifest/" + digest.String(),
		})
		if err != nil {
			message := fmt.Sprintf("OCI tag %s points at missing manifest %s",
				ref.Name, digest.Prefixed())
			if m.options.Strict {
				return errors.New(message)
			}
			m.warn(message)
			continue
		}
		manifest.EntryKey = tagKey
		if manifest.MediaType == "" {
			manifest.MediaType = ref.MediaType
		}
		if err := m.catalog.PutEntry(manifest); err != nil {
			return err
		}
		m.report.Entries++
	}
	return nil
}

func (m *importer) importStats(project, ledgerPath string, db *sql.DB) error {
	id := "stats:" + ledgerPath
	if m.state.LogicalDone(id) {
		return nil
	}
	access, err := m.legacyPackageStats(project, db)
	if err != nil {
		return err
	}
	traffic, err := m.legacyTrafficStats(project, db)
	if err != nil {
		return err
	}
	if len(access) > 0 || len(traffic) > 0 {
		if err := m.catalog.RecordAccess(access, traffic); err != nil {
			return err
		}
	}
	return m.state.MarkLogical(id, "")
}

func (m *importer) legacyPackageStats(
	project string, db *sql.DB,
) ([]catalog.AccessDelta, error) {
	if !tableExists(m.ctx, db, "package_stats") {
		return nil, nil
	}
	rows, err := db.QueryContext(m.ctx,
		`SELECT ecosystem, name, access_count, last_access FROM package_stats`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var access []catalog.AccessDelta
	for rows.Next() {
		var ecosystem, name string
		var count int64
		var last sql.NullFloat64
		if err := rows.Scan(&ecosystem, &name, &count, &last); err != nil {
			return nil, err
		}
		access = append(access, catalog.AccessDelta{
			Project: project, Eco: translateEcosystem(ecosystem), Name: name,
			Count: count, LastAccess: time.Unix(int64(last.Float64), 0),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("frompython: iterate package_stats: %w", err)
	}
	return access, nil
}

func (m *importer) legacyTrafficStats(
	project string, db *sql.DB,
) ([]catalog.TrafficDelta, error) {
	if !tableExists(m.ctx, db, "traffic_stats") {
		return nil, nil
	}
	rows, err := db.QueryContext(m.ctx,
		`SELECT ecosystem, hit_count, hit_bytes, miss_count, miss_bytes FROM traffic_stats`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var traffic []catalog.TrafficDelta
	for rows.Next() {
		var row catalog.TrafficDelta
		var ecosystem string
		if err := rows.Scan(&ecosystem, &row.HitCount, &row.HitBytes,
			&row.MissCount, &row.MissBytes); err != nil {
			return nil, err
		}
		row.Project, row.Eco = project, translateEcosystem(ecosystem)
		traffic = append(traffic, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("frompython: iterate traffic_stats: %w", err)
	}
	return traffic, nil
}

func (m *importer) copyLegacyUsers() error {
	source := filepath.Join(m.options.ConfigDir, "users.json")
	if _, err := os.Stat(source); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	target := filepath.Join(m.options.DataDir, "config", "users.json")
	if _, err := os.Stat(target); err == nil {
		return nil
	}
	return linkOrCopy(source, target, 0o600)
}

func (m *importer) log(message string) {
	if m.options.Progress != nil {
		m.options.Progress(message)
	}
}

func (m *importer) warn(message string) {
	m.report.Warnings = append(m.report.Warnings, message)
	m.log("warning: " + message)
}

func openLegacyLedger(ctx context.Context, name string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: name}
	query := u.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "query_only(ON)")
	query.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("frompython: open live ledger %s: %w", name, err)
	}
	return db, nil
}

func tableExists(ctx context.Context, db *sql.DB, name string) bool {
	var found string
	return db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).
		Scan(&found) == nil
}

func parseDigest(value string) (blob.Digest, bool) {
	digest, err := blob.ParseDigest(value)
	return digest, err == nil
}

func translateEcosystem(value string) string {
	switch strings.ToLower(value) {
	case "docker", "oci":
		return "oci"
	case "pip", "pypi":
		return "pypi"
	case "apk", "apt":
		return "apt"
	case "npm", "git", "files":
		return strings.ToLower(value)
	default:
		return ""
	}
}

func entryKeys(role, relative string, digest blob.Digest) []string {
	relative = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(relative)), "./")
	switch role {
	case "docker":
		if digest.Valid() {
			return []string{"manifest/" + digest.String(), "blob/" + digest.String()}
		}
	case "apt":
		return []string{"immutable/http://" + relative}
	}
	return []string{relative}
}

func treeKeys(role, relative string) ([]string, blob.Digest) {
	relative = strings.TrimPrefix(relative, "./")
	switch role {
	case "npm":
		if strings.HasSuffix(relative, "/metadata.json") {
			return []string{"packument/" + strings.TrimSuffix(relative, "/metadata.json")}, ""
		}
		return []string{relative}, ""
	case "pip":
		if strings.HasSuffix(relative, "/simple.json") {
			parts := strings.Split(relative, "/")
			if len(parts) >= 3 {
				return []string{"simple/" + strings.Join(parts[:len(parts)-1], "/")}, ""
			}
		}
		return []string{relative}, ""
	case "apt":
		class := "immutable/"
		if volatileAPT(filepath.Base(relative)) {
			class = "volatile/"
		}
		return []string{class + "http://" + relative}, ""
	case "docker":
		if digest, ok := digestFromBlobPath(relative); ok {
			return []string{"manifest/" + digest.String(), "blob/" + digest.String()}, digest
		}
	case "git":
		if digest, ok := digestFromBlobPath(relative); ok {
			return []string{"lfs/" + digest.String()}, digest
		}
		return nil, ""
	case "files":
		return []string{relative}, ""
	}
	return nil, ""
}

func digestFromBlobPath(relative string) (blob.Digest, bool) {
	parts := strings.Split(relative, "/")
	if len(parts) < 4 || parts[0] != "blobs" || parts[1] != "sha256" {
		return "", false
	}
	return parseDigest(parts[len(parts)-1])
}

func volatileAPT(name string) bool {
	switch name {
	case "InRelease", "Release", "Release.gpg", "APKINDEX.tar.gz":
		return true
	}
	return strings.HasPrefix(name, "Packages") ||
		strings.HasPrefix(name, "Sources") ||
		strings.HasPrefix(name, "Contents")
}

func skipLegacyFile(relative string) bool {
	name := filepath.Base(relative)
	return strings.HasPrefix(name, "ledger.db") ||
		strings.HasSuffix(name, ".part") ||
		strings.HasSuffix(name, ".meta") ||
		name == ".DS_Store"
}

func secureJoin(root, relative string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(relative))
	if filepath.IsAbs(clean) || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("frompython: path escapes role root: %q", relative)
	}
	return filepath.Join(root, clean), nil
}

func sameOrNested(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && (relative == "." ||
		(relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

func safeSegment(value string) bool {
	return value != "" && value != "." && value != ".." &&
		!strings.ContainsAny(value, `/\`) && !strings.ContainsRune(value, 0)
}

func parseTime(value string, fallback time.Time) time.Time {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed
	}
	return fallback
}

func linkOrCopy(source, target string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(target); err == nil {
		return nil
	}
	if err := os.Link(source, target); err == nil {
		return nil
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	temp, err := os.CreateTemp(filepath.Dir(target), ".migrate-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if _, err := io.CopyBuffer(temp, in, make([]byte, 1<<20)); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if mode == 0 {
		mode = 0o644
	}
	if err := os.Chmod(tempName, mode.Perm()); err != nil {
		return err
	}
	return os.Rename(tempName, target)
}
