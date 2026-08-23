package frompython

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
)

func TestRunLinksCASAndResumes(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "legacy")
	destination := filepath.Join(base, "pkgreg")
	configDir := filepath.Join(base, "config")
	for _, directory := range []string{
		filepath.Join(source, ".cas", "sha256"),
		filepath.Join(source, "npm", "demo", "-"),
		configDir,
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(configDir, "projects.json"),
		[]byte(`{"projects":{},"owners":{},"offline":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	content := []byte("legacy npm tarball")
	sum := sha256.Sum256(content)
	hexDigest := hex.EncodeToString(sum[:])
	casPath := filepath.Join(source, ".cas", "sha256", hexDigest[:2], hexDigest)
	if err := os.MkdirAll(filepath.Dir(casPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(casPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(source, "npm", "demo", "-", "demo-1.0.0.tgz")
	if err := os.Link(casPath, artifactPath); err != nil {
		t.Fatal(err)
	}
	createLegacyLedger(t, filepath.Join(source, "npm", "ledger.db"), hexDigest, int64(len(content)))

	report, err := Run(context.Background(), Options{
		SourceDir: source, DataDir: destination, ConfigDir: configDir, Strict: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.CASBlobs != 1 || report.Entries == 0 || report.Artifacts != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	store, err := blob.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := blob.ParseDigest(hexDigest)
	target, err := store.Path(digest)
	if err != nil {
		t.Fatal(err)
	}
	sourceInfo, _ := os.Stat(casPath)
	targetInfo, _ := os.Stat(target)
	if !os.SameFile(sourceInfo, targetInfo) {
		t.Fatal("CAS entry was not hardline")
	}
	cat, err := catalog.Open(catalog.Options{Path: filepath.Join(destination, "db", "catalog.db")})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := cat.GetEntry(catalog.EntryKey{
		Project: "global", Eco: "npm", Key: "demo/-/demo-1.0.0.tgz",
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Digest != digest {
		t.Fatalf("entry digest = %s", entry.Digest)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	resumed, err := Run(context.Background(), Options{
		SourceDir: source, DataDir: destination, ConfigDir: configDir, Strict: true,
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.CASBlobs != 0 || resumed.Skipped == 0 {
		t.Fatalf("resume did not use durable progress: %+v", resumed)
	}
}

func TestRunMakesImportedOCITagImmediatelyServeable(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "legacy")
	destination := filepath.Join(base, "pkgreg")
	configDir := filepath.Join(base, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "projects.json"),
		[]byte(`{"projects":{},"owners":{},"offline":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestBody := []byte(`{"schemaVersion":2}`)
	sum := sha256.Sum256(manifestBody)
	hexDigest := hex.EncodeToString(sum[:])
	manifestPath := filepath.Join(source, "docker", "blobs", "sha256",
		hexDigest[:2], hexDigest)
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifestBody, 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(source, "docker", "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE artifacts (
		id INTEGER PRIMARY KEY,
		ecosystem TEXT NOT NULL, name TEXT NOT NULL, version TEXT NOT NULL,
		digest TEXT, size INTEGER, origin TEXT, path TEXT, arch TEXT,
		cached_at TEXT NOT NULL, extra TEXT
	);
	CREATE TABLE package_stats (
		ecosystem TEXT, name TEXT, access_count INTEGER, last_access REAL
	);
	CREATE TABLE traffic_stats (
		ecosystem TEXT, hit_count INTEGER, hit_bytes INTEGER,
		miss_count INTEGER, miss_bytes INTEGER
	);
	CREATE TABLE oci_tags (
		upstream TEXT NOT NULL, repo TEXT NOT NULL, tag TEXT NOT NULL,
		digest TEXT NOT NULL, media_type TEXT, fetched_at TEXT,
		PRIMARY KEY (upstream, repo, tag)
	);
	INSERT INTO oci_tags VALUES(
		'dockerhub','library/demo','latest','sha256:' || ?,
		'application/vnd.oci.image.manifest.v1+json','2026-01-01T00:00:00Z'
	);`, hexDigest); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(context.Background(), Options{
		SourceDir: source, DataDir: destination, ConfigDir: configDir, Strict: true,
	}); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(catalog.Options{
		Path: filepath.Join(destination, "db", "catalog.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	tag, err := cat.GetEntry(catalog.EntryKey{
		Project: "global", Eco: "oci", Key: "tag/dockerhub/library/demo/latest",
	})
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := blob.ParseDigest(hexDigest)
	if tag.Digest != digest {
		t.Fatalf("tag digest = %s, want %s", tag.Digest, digest)
	}
}

// In the default non-strict mode a stale ledger row is a warning, not a stop. It must
// not end the scan: the loop used to close its cursor on the first warning, so one
// unreadable row silently dropped every artifact recorded after it.
func TestNonStrictStaleRowDoesNotTruncateTheImport(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "legacy")
	destination := filepath.Join(base, "pkgreg")
	configDir := filepath.Join(base, "config")
	for _, directory := range []string{
		filepath.Join(source, ".cas", "sha256"),
		filepath.Join(source, "npm", "good", "-"),
		configDir,
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(configDir, "projects.json"),
		[]byte(`{"projects":{},"owners":{},"offline":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	content := []byte("a good npm tarball")
	sum := sha256.Sum256(content)
	hexDigest := hex.EncodeToString(sum[:])
	casPath := filepath.Join(source, ".cas", "sha256", hexDigest[:2], hexDigest)
	if err := os.MkdirAll(filepath.Dir(casPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(casPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(casPath, filepath.Join(source, "npm", "good", "-", "good-1.0.0.tgz")); err != nil {
		t.Fatal(err)
	}

	// Row id 1 points at a file that is no longer on disk; row id 2 is importable.
	// Ordered by id, so the stale row is scanned first.
	ledger := filepath.Join(source, "npm", "ledger.db")
	db, err := sql.Open("sqlite", ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE artifacts (
		id INTEGER PRIMARY KEY,
		ecosystem TEXT NOT NULL, name TEXT NOT NULL, version TEXT NOT NULL,
		digest TEXT, size INTEGER, origin TEXT, path TEXT, arch TEXT,
		cached_at TEXT NOT NULL, extra TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO artifacts(id,ecosystem,name,version,digest,size,path,cached_at)
		VALUES(?,'npm',?,?,?,?,?,'2026-01-01T00:00:00Z')`
	if _, err := db.Exec(insert, 1, "vanished", "9.9.9", "sha256:"+hexDigest, 10,
		"vanished/-/vanished-9.9.9.tgz"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert, 2, "good", "1.0.0", "sha256:"+hexDigest, len(content),
		"good/-/good-1.0.0.tgz"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), Options{
		SourceDir: source, DataDir: destination, ConfigDir: configDir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Warnings) == 0 {
		t.Fatal("the stale row produced no warning; the fixture no longer covers the case")
	}

	cat, err := catalog.Open(catalog.Options{
		Path: filepath.Join(destination, "db", "catalog.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	artifacts, _, err := cat.QueryArtifacts(catalog.ArtifactQuery{Project: "global", Eco: "npm"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, artifact := range artifacts {
		names = append(names, artifact.Name)
	}
	if !slices.Contains(names, "good") {
		t.Fatalf("the ledger row after the stale one was dropped; imported inventory = %v", names)
	}
}

func createLegacyLedger(t *testing.T, path, digest string, size int64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE artifacts (
		id INTEGER PRIMARY KEY,
		ecosystem TEXT NOT NULL, name TEXT NOT NULL, version TEXT NOT NULL,
		digest TEXT, size INTEGER, origin TEXT, path TEXT, arch TEXT,
		cached_at TEXT NOT NULL, extra TEXT
	);
	CREATE TABLE package_stats (
		ecosystem TEXT, name TEXT, access_count INTEGER, last_access REAL
	);
	CREATE TABLE traffic_stats (
		ecosystem TEXT, hit_count INTEGER, hit_bytes INTEGER,
		miss_count INTEGER, miss_bytes INTEGER
	);
	INSERT INTO artifacts(ecosystem,name,version,digest,size,path,cached_at)
	VALUES('npm','demo','1.0.0','sha256:' || ?,?,'demo/-/demo-1.0.0.tgz',
		'2026-01-01T00:00:00Z');
	INSERT INTO package_stats VALUES('npm','demo',3,1700000000);
	INSERT INTO traffic_stats VALUES('npm',2,20,1,10);`, digest, size)
	if err != nil {
		t.Fatal(err)
	}
}
