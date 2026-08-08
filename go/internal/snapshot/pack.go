package snapshot

import (
	"archive/tar"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/brightskies/pkgreg/internal/blob"
	"github.com/brightskies/pkgreg/internal/catalog"
)

// PackVersion is the only transfer format version this build reads and writes.
const PackVersion = 1

// Pack is the first member of every transfer tar.
type Pack struct {
	Version   int    `json:"version"`
	Project   string `json:"project"`
	Base      string `json:"base,omitempty"`
	Target    string `json:"target"`
	Snapshots []Meta `json:"snapshots"`
	Blobs     int64  `json:"blobs"`
	Bytes     int64  `json:"bytes"`
}

// Meta is the portable form of a catalog snapshot row.
type Meta struct {
	ID         string      `json:"id"`
	Project    string      `json:"project"`
	Parent     string      `json:"parent,omitempty"`
	Manifest   blob.Digest `json:"manifest_sha256"`
	EntryCount int64       `json:"entry_count"`
	TotalBytes int64       `json:"total_bytes"`
	CreatedAt  time.Time   `json:"created_at"`
	Subject    string      `json:"subject"`
	Author     string      `json:"author,omitempty"`
}

func metaFromCatalog(s catalog.Snapshot) Meta {
	return Meta{
		ID: s.ID, Project: s.Project, Parent: s.Parent, Manifest: s.Manifest,
		EntryCount: s.EntryCount, TotalBytes: s.TotalBytes, CreatedAt: s.CreatedAt,
		Subject: s.Subject, Author: s.Author,
	}
}

func (s Meta) catalog() catalog.Snapshot {
	return catalog.Snapshot{
		ID: s.ID, Project: s.Project, Parent: s.Parent, Manifest: s.Manifest,
		EntryCount: s.EntryCount, TotalBytes: s.TotalBytes, CreatedAt: s.CreatedAt,
		Subject: s.Subject, Author: s.Author,
	}
}

// ExportOptions selects one full or incremental transfer.
type ExportOptions struct {
	Project string
	Base    string
	Target  string
	CertDir string
	Logf    func(string)
}

// WritePack streams a transfer tar. Digest membership is held in a temporary SQLite
// set so memory usage does not grow with the cache.
func WritePack(
	ctx context.Context, dst io.Writer, cat *catalog.DB, store *blob.Store, opts ExportOptions,
) (Pack, error) {
	if opts.Project == "" {
		return Pack{}, errors.New("snapshot: export project is required")
	}
	targetID := opts.Target
	if targetID == "" {
		var err error
		targetID, err = cat.GetHead(opts.Project)
		if err != nil {
			return Pack{}, err
		}
	}
	if targetID == "" {
		return Pack{}, errors.New("snapshot: project has no checkpoint to export")
	}
	lineage, err := exportLineage(cat, opts.Project, opts.Base, targetID)
	if err != nil {
		return Pack{}, err
	}

	scratch, err := openScratch()
	if err != nil {
		return Pack{}, err
	}
	defer scratch.Close()
	if _, err := scratch.db.ExecContext(ctx,
		`CREATE TABLE base(d TEXT PRIMARY KEY) WITHOUT ROWID;
		 CREATE TABLE selected(d TEXT PRIMARY KEY, size INTEGER NOT NULL) WITHOUT ROWID;`); err != nil {
		return Pack{}, fmt.Errorf("snapshot: prepare export set: %w", err)
	}
	if opts.Base != "" {
		base, err := cat.GetSnapshot(opts.Base)
		if err != nil {
			return Pack{}, err
		}
		if err := indexManifest(ctx, cat, store, base, scratch.db,
			`INSERT OR IGNORE INTO base(d) VALUES (?)`); err != nil {
			return Pack{}, err
		}
	}
	target := lineage[len(lineage)-1]
	if err := indexManifest(ctx, cat, store, target.catalog(), scratch.db,
		`INSERT OR IGNORE INTO selected(d, size)
		 SELECT ?, ? WHERE NOT EXISTS (SELECT 1 FROM base WHERE d = ?)`); err != nil {
		return Pack{}, err
	}
	var pack Pack
	pack.Version, pack.Project, pack.Base, pack.Target =
		PackVersion, opts.Project, opts.Base, targetID
	pack.Snapshots = lineage
	if err := scratch.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM selected`).Scan(&pack.Blobs, &pack.Bytes); err != nil {
		return Pack{}, fmt.Errorf("snapshot: count export set: %w", err)
	}

	tw := tar.NewWriter(dst)
	failed := true
	defer func() {
		if failed {
			_ = tw.Close()
		}
	}()
	payload, err := json.MarshalIndent(pack, "", "  ")
	if err != nil {
		return Pack{}, err
	}
	payload = append(payload, '\n')
	if err := writeTarBytes(tw, "pack.json", payload, 0o644, target.CreatedAt); err != nil {
		return Pack{}, err
	}
	for _, meta := range lineage {
		if err := contextErr(ctx); err != nil {
			return Pack{}, err
		}
		if err := writeBlobMember(tw, store, meta.Manifest,
			"snapshots/"+meta.ID+".manifest.gz", meta.CreatedAt); err != nil {
			return Pack{}, err
		}
	}
	rows, err := scratch.db.QueryContext(ctx, `SELECT d, size FROM selected ORDER BY d`)
	if err != nil {
		return Pack{}, fmt.Errorf("snapshot: list export blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var written int64
	for rows.Next() {
		if err := contextErr(ctx); err != nil {
			return Pack{}, err
		}
		var digestText string
		var size int64
		if err := rows.Scan(&digestText, &size); err != nil {
			return Pack{}, err
		}
		digest := blob.Digest(digestText)
		stat, ok := store.Stat(digest)
		if !ok {
			return Pack{}, fmt.Errorf("snapshot: export blob %s is missing", digest)
		}
		if stat.Size != size {
			return Pack{}, fmt.Errorf("snapshot: export blob %s has size %d, manifest says %d",
				digest, stat.Size, size)
		}
		if err := writeBlobMember(tw, store, digest,
			"blobs/"+digestText[:2]+"/"+digestText, target.CreatedAt); err != nil {
			return Pack{}, err
		}
		written++
		if opts.Logf != nil && written%1000 == 0 {
			opts.Logf(fmt.Sprintf("exported %d of %d blobs", written, pack.Blobs))
		}
	}
	if err := rows.Err(); err != nil {
		return Pack{}, err
	}
	if opts.Project == "global" && opts.CertDir != "" {
		for _, name := range []string{"ca.crt", "server.crt", "server.key"} {
			path := filepath.Join(opts.CertDir, name)
			if err := writeFileMember(tw, path, "certs/"+name, target.CreatedAt); err != nil &&
				!errors.Is(err, os.ErrNotExist) {
				return Pack{}, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return Pack{}, fmt.Errorf("snapshot: finish pack: %w", err)
	}
	failed = false
	return pack, nil
}

func exportLineage(
	cat *catalog.DB, project, base, target string,
) ([]Meta, error) {
	var reverse []Meta
	next := target
	for next != base {
		if next == "" {
			return nil, fmt.Errorf("snapshot: base %s is not an ancestor of target %s", base, target)
		}
		s, err := cat.GetSnapshot(next)
		if err != nil {
			return nil, err
		}
		if s.Project != project {
			return nil, fmt.Errorf("snapshot: checkpoint %s belongs to project %s", s.ID, s.Project)
		}
		reverse = append(reverse, metaFromCatalog(s))
		next = s.Parent
	}
	if len(reverse) == 0 {
		return nil, errors.New("snapshot: base and target are identical")
	}
	out := make([]Meta, len(reverse))
	for i := range reverse {
		out[len(reverse)-1-i] = reverse[i]
	}
	return out, nil
}

func indexManifest(
	ctx context.Context,
	cat *catalog.DB,
	store *blob.Store,
	s catalog.Snapshot,
	db *sql.DB,
	query string,
) error {
	file, _, err := store.Open(s.Manifest)
	if err != nil {
		return fmt.Errorf("snapshot: open manifest %s: %w", s.ID, err)
	}
	defer func() { _ = file.Close() }()
	it, err := NewIterator(file)
	if err != nil {
		return err
	}
	defer func() { _ = it.Close() }()
	if it.Header.Project != s.Project {
		return fmt.Errorf("snapshot: manifest %s belongs to project %s", s.ID, it.Header.Project)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer func() { _ = stmt.Close() }()
	count, bytes, walkErr := it.Walk(func(entry Entry) error {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if strings.Contains(query, "selected") {
			_, err = stmt.ExecContext(ctx, string(entry.Digest), entry.Size, string(entry.Digest))
		} else {
			_, err = stmt.ExecContext(ctx, string(entry.Digest))
		}
		return err
	})
	if walkErr != nil {
		_ = tx.Rollback()
		return walkErr
	}
	if count != s.EntryCount || bytes != s.TotalBytes {
		_ = tx.Rollback()
		return fmt.Errorf("snapshot: manifest %s totals do not match catalog", s.ID)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_ = cat
	return nil
}

// ImportOptions controls verified application of one pack.
type ImportOptions struct {
	Project string
	CertDir string
	Logf    func(string)
	// Apply lets the composition root materialize descriptor-owned managed trees
	// before atomically applying the catalog mappings. Nil applies catalog entries
	// directly, which is useful for stores without managed ecosystems.
	Apply func(context.Context, Pack, Meta) error
}

// InspectPack reads and validates only pack.json. Callers reopen the file for the
// verified import after using this metadata to register a new named project.
func InspectPack(src io.Reader, expectedProject string) (Pack, error) {
	tr := tar.NewReader(src)
	header, err := tr.Next()
	if err != nil {
		return Pack{}, fmt.Errorf("snapshot: read pack header: %w", err)
	}
	if header.Name != "pack.json" || header.Size > 4<<20 {
		return Pack{}, errors.New("snapshot: pack.json must be the first member")
	}
	var pack Pack
	if err := json.NewDecoder(tr).Decode(&pack); err != nil {
		return Pack{}, fmt.Errorf("snapshot: decode pack.json: %w", err)
	}
	if err := validatePack(pack, expectedProject); err != nil {
		return Pack{}, err
	}
	return pack, nil
}

// ReadPack verifies every member before appending snapshots and atomically applying
// the target manifest.
func ReadPack(
	ctx context.Context, src io.Reader, cat *catalog.DB, store *blob.Store, opts ImportOptions,
) (Pack, error) {
	tr := tar.NewReader(src)
	header, err := tr.Next()
	if err != nil {
		return Pack{}, fmt.Errorf("snapshot: read pack header: %w", err)
	}
	if header.Name != "pack.json" || header.Size > 4<<20 {
		return Pack{}, errors.New("snapshot: pack.json must be the first member")
	}
	var pack Pack
	if err := json.NewDecoder(tr).Decode(&pack); err != nil {
		return Pack{}, fmt.Errorf("snapshot: decode pack.json: %w", err)
	}
	if err := validatePack(pack, opts.Project); err != nil {
		return Pack{}, err
	}
	head, err := cat.GetHead(pack.Project)
	if err != nil {
		return Pack{}, err
	}
	if head != pack.Base {
		return Pack{}, fmt.Errorf(
			"snapshot: non-fast-forward import: local HEAD is %q, pack requires base %q",
			head, pack.Base)
	}

	scratch, err := openScratch()
	if err != nil {
		return Pack{}, err
	}
	defer scratch.Close()
	if _, err := scratch.db.ExecContext(ctx,
		`CREATE TABLE seen_manifest(id TEXT PRIMARY KEY) WITHOUT ROWID;
		 CREATE TABLE seen_blob(d TEXT PRIMARY KEY, size INTEGER NOT NULL) WITHOUT ROWID;`); err != nil {
		return Pack{}, err
	}
	metaByID := make(map[string]Meta, len(pack.Snapshots))
	for _, meta := range pack.Snapshots {
		metaByID[meta.ID] = meta
	}
	stagedCerts := make(map[string]string)
	var importedBlobs int64
	defer func() {
		for _, path := range stagedCerts {
			_ = os.Remove(path)
		}
	}()
	for {
		if err := contextErr(ctx); err != nil {
			return Pack{}, err
		}
		header, err = tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Pack{}, fmt.Errorf("snapshot: read pack member: %w", err)
		}
		switch {
		case strings.HasPrefix(header.Name, "snapshots/"):
			id, ok := snapshotMemberID(header.Name)
			meta, expected := metaByID[id]
			if !ok || !expected {
				return Pack{}, fmt.Errorf("snapshot: unexpected manifest member %q", header.Name)
			}
			digest, size, err := importBlob(ctx, tr, store, meta.Manifest)
			if err != nil {
				return Pack{}, err
			}
			if err := cat.UpsertBlob(catalog.Blob{
				Digest: digest, Size: size, CreatedAt: meta.CreatedAt, LastAccess: meta.CreatedAt,
			}); err != nil {
				return Pack{}, err
			}
			if _, err := scratch.db.ExecContext(ctx, `INSERT INTO seen_manifest(id) VALUES (?)`, id); err != nil {
				return Pack{}, fmt.Errorf("snapshot: duplicate manifest %s", id)
			}
		case strings.HasPrefix(header.Name, "blobs/"):
			expected, ok := blobMemberDigest(header.Name)
			if !ok {
				return Pack{}, fmt.Errorf("snapshot: invalid blob member %q", header.Name)
			}
			digest, size, err := importBlob(ctx, tr, store, expected)
			if err != nil {
				return Pack{}, err
			}
			if _, err := scratch.db.ExecContext(ctx,
				`INSERT INTO seen_blob(d, size) VALUES (?, ?)`, string(digest), size); err != nil {
				return Pack{}, fmt.Errorf("snapshot: duplicate blob %s", digest)
			}
			now := time.Now()
			if err := cat.UpsertBlob(catalog.Blob{
				Digest: digest, Size: size, CreatedAt: now, LastAccess: now,
			}); err != nil {
				return Pack{}, err
			}
			importedBlobs++
			if opts.Logf != nil && importedBlobs%1000 == 0 {
				opts.Logf(fmt.Sprintf("verified %d of %d blobs", importedBlobs, pack.Blobs))
			}
		case strings.HasPrefix(header.Name, "certs/"):
			if pack.Project != "global" || opts.CertDir == "" {
				return Pack{}, fmt.Errorf("snapshot: unexpected certificate member %q", header.Name)
			}
			name := strings.TrimPrefix(header.Name, "certs/")
			if name != "ca.crt" && name != "server.crt" && name != "server.key" {
				return Pack{}, fmt.Errorf("snapshot: forbidden certificate member %q", header.Name)
			}
			path, err := stageFile(ctx, tr, opts.CertDir, name)
			if err != nil {
				return Pack{}, err
			}
			stagedCerts[name] = path
		default:
			return Pack{}, fmt.Errorf("snapshot: unexpected pack member %q", header.Name)
		}
	}
	var seenManifests, seenBlobs, seenBytes int64
	if err := scratch.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM seen_manifest`).Scan(&seenManifests); err != nil {
		return Pack{}, err
	}
	if err := scratch.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM seen_blob`).Scan(&seenBlobs, &seenBytes); err != nil {
		return Pack{}, err
	}
	if seenManifests != int64(len(pack.Snapshots)) {
		return Pack{}, errors.New("snapshot: pack is missing one or more manifests")
	}
	if seenBlobs != pack.Blobs || seenBytes != pack.Bytes {
		return Pack{}, fmt.Errorf(
			"snapshot: pack blob totals mismatch: got %d/%d bytes, want %d/%d",
			seenBlobs, seenBytes, pack.Blobs, pack.Bytes)
	}
	for index, meta := range pack.Snapshots {
		if err := verifyManifest(ctx, store, meta, index == len(pack.Snapshots)-1); err != nil {
			return Pack{}, err
		}
		if existing, err := cat.GetSnapshot(meta.ID); err == nil {
			if metaFromCatalog(existing) != meta {
				return Pack{}, fmt.Errorf("snapshot: checkpoint %s conflicts with local metadata", meta.ID)
			}
		} else if !errors.Is(err, catalog.ErrNotFound) {
			return Pack{}, err
		} else if err := cat.PutSnapshot(meta.catalog()); err != nil {
			return Pack{}, err
		}
	}
	target := pack.Snapshots[len(pack.Snapshots)-1]
	if opts.Apply != nil {
		if err := opts.Apply(ctx, pack, target); err != nil {
			return Pack{}, err
		}
	} else {
		file, _, err := store.Open(target.Manifest)
		if err != nil {
			return Pack{}, err
		}
		defer func() { _ = file.Close() }()
		it, err := NewIterator(file)
		if err != nil {
			return Pack{}, err
		}
		defer func() { _ = it.Close() }()
		if err := cat.ApplySnapshotFrom(
			pack.Project, pack.Base, pack.Target,
			func(yield func(catalog.Entry) error) error {
				_, _, err := it.Walk(func(entry Entry) error {
					return yield(catalog.Entry{
						EntryKey: catalog.EntryKey{
							Project: pack.Project, Eco: entry.Eco, Key: entry.Key,
						},
						Digest: entry.Digest, Size: entry.Size,
					})
				})
				return err
			}); err != nil {
			return Pack{}, err
		}
	}
	for name, staged := range stagedCerts {
		mode := os.FileMode(0o644)
		if name == "server.key" {
			mode = 0o600
		}
		if err := os.Chmod(staged, mode); err != nil {
			return Pack{}, err
		}
		if err := os.Rename(staged, filepath.Join(opts.CertDir, name)); err != nil {
			return Pack{}, fmt.Errorf("snapshot: install %s: %w", name, err)
		}
		delete(stagedCerts, name)
	}
	return pack, nil
}

func validatePack(pack Pack, expectedProject string) error {
	if pack.Version != PackVersion {
		return fmt.Errorf("snapshot: unsupported pack version %d", pack.Version)
	}
	if pack.Project == "" || pack.Target == "" || len(pack.Snapshots) == 0 {
		return errors.New("snapshot: incomplete pack metadata")
	}
	if expectedProject != "" && pack.Project != expectedProject {
		return fmt.Errorf("snapshot: pack belongs to project %s, not %s",
			pack.Project, expectedProject)
	}
	parent := pack.Base
	for _, meta := range pack.Snapshots {
		if meta.Project != pack.Project || meta.ID != string(meta.Manifest) || !meta.Manifest.Valid() ||
			meta.Parent != parent || meta.EntryCount < 0 || meta.TotalBytes < 0 ||
			meta.CreatedAt.IsZero() {
			return fmt.Errorf("snapshot: invalid lineage at checkpoint %s", meta.ID)
		}
		parent = meta.ID
	}
	if parent != pack.Target || pack.Blobs < 0 || pack.Bytes < 0 {
		return errors.New("snapshot: invalid pack target or totals")
	}
	return nil
}

func verifyManifest(
	ctx context.Context, store *blob.Store, meta Meta, requireBlobs bool,
) error {
	file, _, err := store.Open(meta.Manifest)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	it, err := NewIterator(file)
	if err != nil {
		return err
	}
	defer func() { _ = it.Close() }()
	if it.Header.Project != meta.Project {
		return fmt.Errorf("snapshot: manifest %s project mismatch", meta.ID)
	}
	count, bytes, err := it.Walk(func(entry Entry) error {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if requireBlobs {
			stat, ok := store.Stat(entry.Digest)
			if !ok {
				return fmt.Errorf("snapshot: manifest %s references missing blob %s",
					meta.ID, entry.Digest)
			}
			if stat.Size != entry.Size {
				return fmt.Errorf("snapshot: blob %s size mismatch", entry.Digest)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if count != meta.EntryCount || bytes != meta.TotalBytes {
		return fmt.Errorf("snapshot: manifest %s totals mismatch", meta.ID)
	}
	return nil
}

func importBlob(
	ctx context.Context, src io.Reader, store *blob.Store, expected blob.Digest,
) (blob.Digest, int64, error) {
	writer, err := store.Create()
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = writer.Abort() }()
	if _, err := io.Copy(writer, contextReader{ctx: ctx, reader: src}); err != nil {
		return "", 0, fmt.Errorf("snapshot: import blob: %w", err)
	}
	if writer.Digest() != expected {
		return "", 0, fmt.Errorf(
			"snapshot: corrupt blob %s: content digest is %s", expected, writer.Digest())
	}
	return writer.Commit()
}

func writeBlobMember(
	tw *tar.Writer, store *blob.Store, digest blob.Digest, name string, modified time.Time,
) error {
	file, stat, err := store.Open(digest)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o644, Size: stat.Size, ModTime: modified.UTC(),
		Format: tar.FormatPAX,
	}); err != nil {
		return fmt.Errorf("snapshot: write tar header: %w", err)
	}
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("snapshot: write %s: %w", name, err)
	}
	return nil
}

func writeFileMember(tw *tar.Writer, path, name string, modified time.Time) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	mode := int64(0o644)
	if filepath.Base(path) == "server.key" {
		mode = 0o600
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: mode, Size: stat.Size(), ModTime: modified.UTC(),
		Format: tar.FormatPAX,
	}); err != nil {
		return err
	}
	_, err = io.Copy(tw, file)
	return err
}

func writeTarBytes(tw *tar.Writer, name string, payload []byte, mode int64, modified time.Time) error {
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: mode, Size: int64(len(payload)), ModTime: modified.UTC(),
		Format: tar.FormatPAX,
	}); err != nil {
		return err
	}
	_, err := tw.Write(payload)
	return err
}

func snapshotMemberID(name string) (string, bool) {
	if !strings.HasPrefix(name, "snapshots/") || !strings.HasSuffix(name, ".manifest.gz") {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, "snapshots/"), ".manifest.gz")
	return id, id != "" && !strings.Contains(id, "/")
}

func blobMemberDigest(name string) (blob.Digest, bool) {
	parts := strings.Split(name, "/")
	if len(parts) != 3 || parts[0] != "blobs" || len(parts[1]) != 2 ||
		!strings.HasPrefix(parts[2], parts[1]) {
		return "", false
	}
	digest, err := blob.ParseDigest(parts[2])
	return digest, err == nil
}

func stageFile(ctx context.Context, src io.Reader, dir, name string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, "."+name+".import-*")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if _, err := io.Copy(file, contextReader{ctx: ctx, reader: src}); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

type scratchDB struct {
	db   *sql.DB
	path string
}

func openScratch() (*scratchDB, error) {
	file, err := os.CreateTemp("", "pkgreg-snapshot-*.db")
	if err != nil {
		return nil, fmt.Errorf("snapshot: create scratch database: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)")
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &scratchDB{db: db, path: path}, nil
}

// Close releases the temporary membership database and removes its files.
func (s *scratchDB) Close() {
	_ = s.db.Close()
	_ = os.Remove(s.path)
	_ = os.Remove(s.path + "-journal")
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := contextErr(r.ctx); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func contextErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
