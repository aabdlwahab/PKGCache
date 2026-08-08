// Package catalog is the metadata store: which bytes exist, which cache key resolves
// to which blob, which mutable names point where, and what the inventory looks like.
//
// It replaces the previous design's per-(project, role) SQLite ledgers — 24 database
// files for four projects — with one database. That is what makes cross-project and
// cross-ecosystem questions a GROUP BY instead of a six-way HTTP fan-out and a manual
// merge, and it is what makes garbage collection and eviction expressible at all.
//
// Four concepts carry everything:
//
//	Blob      immutable bytes, addressed by sha256
//	Entry     (project, eco, key) -> blob. The byte cache; what a GET resolves.
//	Ref       (project, eco, name) -> target + freshness. Mutable pointers:
//	          OCI tags, git refs, apt Release files, npm dist-tags.
//	Artifact  the semantic inventory the console shows.
package catalog

import (
	"errors"
	"time"

	"github.com/brightskies/pkgreg/internal/blob"
)

// Errors returned by this package.
var (
	ErrNotFound = errors.New("catalog: not found")
	ErrClosed   = errors.New("catalog: closed")
	ErrQuota    = errors.New("catalog: quota exceeded")
)

// QuotaError reports the current and configured project usage at a rejected
// commit. It is intentionally structured so the data-plane can return a useful 507.
type QuotaError struct {
	Kind    string
	Usage   int64
	Limit   int64
	Attempt int64
}

func (e *QuotaError) Error() string {
	return "catalog: " + e.Kind + " quota exceeded"
}

func (e *QuotaError) Unwrap() error { return ErrQuota }

// Quota is enforced atomically with entry and artifact publication.
type Quota struct {
	Bytes     int64
	Artifacts int64
}

// Blob is a row in the content index. The store on disk is the source of truth for
// the bytes; this row is what makes them findable, accountable and collectable.
type Blob struct {
	Digest     blob.Digest
	Size       int64
	CreatedAt  time.Time
	LastAccess time.Time
}

// EntryKey identifies a cache entry. The key is the ecosystem's own identity for the
// content — a URL-shaped path for pypi and files, "host/path" for apt, the digest for
// an OCI blob.
type EntryKey struct {
	Project string
	Eco     string
	Key     string
}

// Entry maps a cache key to the blob holding its bytes.
type Entry struct {
	EntryKey
	Digest     blob.Digest
	Size       int64
	MediaType  string
	CachedAt   time.Time
	LastAccess time.Time
	Hits       int64
}

// EntrySource streams entries into an atomic catalog operation. The callback must
// be called in (eco, key) byte order when the source is a snapshot manifest.
type EntrySource func(yield func(Entry) error) error

// RefKey identifies a mutable pointer.
type RefKey struct {
	Project string
	Eco     string
	Name    string
}

// Ref is a mutable name pointing at immutable content, with a freshness policy.
//
// This one type replaces four bespoke mechanisms in the previous design: the OCI
// oci_tags table, the git git_refs table, apt's on-disk .meta ETag sidecars, and
// npm's implicit "re-fetch the packument every time". One table, one revalidation
// path, one offline story.
type Ref struct {
	RefKey
	Target       string // a digest, a commit sha, or an entry key
	MediaType    string
	ETag         string
	LastModified string
	FetchedAt    time.Time
	TTL          time.Duration
}

// Fresh reports whether the ref may be used without revalidating upstream.
func (r Ref) Fresh(now time.Time) bool {
	if r.TTL <= 0 {
		return false
	}
	return now.Sub(r.FetchedAt) < r.TTL
}

// Artifact is an inventory row: the semantic view of cached content that the console
// lists and the manifest exports. Not every entry is an artifact (an index page is
// not), and not every artifact is one entry (a container image is many blobs).
type Artifact struct {
	Project  string
	Eco      string
	Name     string
	Version  string
	Arch     string
	Digest   blob.Digest
	Size     int64
	Origin   string
	CachedAt time.Time
	Extra    map[string]any
}

// Snapshot is a checkpoint: an immutable, content-addressed record of a project's
// entry set at a point in time.
//
// The entry set lives in a manifest blob rather than in rows, so taking a snapshot
// costs one blob and one row no matter how large the cache is — and diffing two
// snapshots is a linear merge of two sorted streams.
type Snapshot struct {
	ID         string
	Project    string
	Parent     string
	Manifest   blob.Digest
	EntryCount int64
	TotalBytes int64
	CreatedAt  time.Time
	Subject    string
	Author     string
}

// EntryQuery filters a listing. Prefix drives the files role's autoindex.
type EntryQuery struct {
	Project string
	Eco     string
	Prefix  string
	Limit   int
	Offset  int
}

// ArtifactQuery filters the inventory view. An empty Project or Eco means "all",
// which is the cross-cutting question the previous sharded design could not ask.
type ArtifactQuery struct {
	Project  string
	Eco      string
	Search   string // substring match on name
	Sort     string // name | size | date | version
	Page     int    // 1-based
	PageSize int    // <= 0 means all rows
}

// AccessDelta is one flush window's usage for a package, folded into the persistent
// tallies. Recording a request must never touch SQLite on the hot path.
type AccessDelta struct {
	Project    string
	Eco        string
	Name       string
	Count      int64
	LastAccess time.Time
}

// EntryTouch is one flush window's read activity for a single cache entry.
//
// Cache hits do not rewrite the entry row, so without this the eviction scan would
// rank by insertion time and TTL eviction would measure age-since-cached instead of
// age-since-use — the hottest wheel in the store would be the first one evicted.
// Touches accumulate in memory and fold in on the same cadence as the other usage
// counters, because losing a window costs eviction precision, not correctness.
type EntryTouch struct {
	EntryKey
	Digest     blob.Digest
	Hits       int64
	LastAccess time.Time
}

// TrafficDelta is one flush window's byte traffic for an ecosystem.
type TrafficDelta struct {
	Project   string
	Eco       string
	HitCount  int64
	HitBytes  int64
	MissCount int64
	MissBytes int64
}

// StatsQuery scopes an aggregate. Empty fields mean "all".
type StatsQuery struct {
	Project string
	Eco     string
}

// EcoStats is the per-ecosystem aggregate the console's statistics tab renders.
type EcoStats struct {
	Project   string `json:"project"`
	Eco       string `json:"eco"`
	Count     int64  `json:"count"`
	Size      int64  `json:"size"`
	Requests  int64  `json:"requests"`
	HitCount  int64  `json:"hit_count"`
	HitBytes  int64  `json:"hit_bytes"`
	MissCount int64  `json:"miss_count"`
	MissBytes int64  `json:"miss_bytes"`
}

// PackageCount is one row of the request leaderboard.
type PackageCount struct {
	Eco        string    `json:"eco"`
	Name       string    `json:"name"`
	Count      int64     `json:"count"`
	LastAccess time.Time `json:"last_access"`
}

// StatsResult is the combined aggregate. In the previous design this was assembled
// from six HTTP responses in ~80 lines of Python; here it is a handful of queries
// against one database.
type StatsResult struct {
	ByEco       []EcoStats     `json:"by_eco"`
	Leaderboard []PackageCount `json:"leaderboard"`
	TopLargest  []Artifact     `json:"top_largest"`
	RecentAdded []Artifact     `json:"recent_added"`
	TotalBlobs  int64          `json:"total_blobs"`
	TotalBytes  int64          `json:"total_bytes"`
}

// ---- time series ------------------------------------------------------------

// Bucket widths, in seconds. Traffic is written at SpanFine and folded upward by
// CompactSeries, so age costs resolution rather than the history itself.
const (
	SpanFine = int64(300)   // 5 minutes
	SpanHour = int64(3600)  // 1 hour
	SpanDay  = int64(86400) // 1 day
)

// How long each resolution survives before it is folded into the next one. These are
// the numbers behind "the console can show you five-minute detail for two days, hourly
// for a month, and daily for two years". Anyone who needs finer or longer wants
// Prometheus, which is scraping the same counters at whatever resolution it likes.
const (
	RetainFine = 48 * time.Hour
	RetainHour = 30 * 24 * time.Hour
	RetainDay  = 2 * 365 * 24 * time.Hour
)

// SeriesDelta is one flush window's traffic for a single bucket, project, ecosystem
// and outcome. A window that straddles a bucket boundary produces two of these, which
// is the reason the engine must stop discarding its timestamp.
type SeriesDelta struct {
	Bucket  time.Time
	Project string
	Eco     string
	Outcome string
	Count   int64
	Bytes   int64
}

// UpstreamDelta is one flush window's origin traffic for a single upstream.
type UpstreamDelta struct {
	Bucket    time.Time
	Project   string
	Upstream  string
	Requests  int64
	Errors    int64
	Bytes     int64
	MillisSum int64
	MillisMax int64
}

// SeriesQuery scopes a time-series read. Span selects a resolution; From and To bound
// it. GroupBy is "outcome", "eco", "" for a single total line, or "eco,outcome".
type SeriesQuery struct {
	Project string
	Eco     string
	Span    int64
	From    time.Time
	To      time.Time
	GroupBy string
}

// TrafficPoint is one bucket of the traffic series. Eco and Outcome are empty when
// the query grouped them away.
type TrafficPoint struct {
	Bucket  time.Time `json:"bucket"`
	Eco     string    `json:"eco,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
	Count   int64     `json:"count"`
	Bytes   int64     `json:"bytes"`
}

// UpstreamPoint is one bucket of one upstream's health. Mean is derived from the
// stored sum; there are no percentiles here on purpose.
type UpstreamPoint struct {
	Bucket     time.Time `json:"bucket"`
	Upstream   string    `json:"upstream"`
	Requests   int64     `json:"requests"`
	Errors     int64     `json:"errors"`
	Bytes      int64     `json:"bytes"`
	MeanMillis int64     `json:"mean_ms"`
	MaxMillis  int64     `json:"max_ms"`
}

// StorageSample is one periodic observation of how much is stored and how much room
// is left.
type StorageSample struct {
	Bucket     time.Time `json:"bucket"`
	BlobCount  int64     `json:"blob_count"`
	BlobBytes  int64     `json:"blob_bytes"`
	EntryCount int64     `json:"entry_count"`
	EntryBytes int64     `json:"entry_bytes"`
	FSFree     int64     `json:"fs_free"`
	FSTotal    int64     `json:"fs_total"`
}

// StorageTotals is the store's current size on both sides of deduplication:
// EntryBytes is what callers asked for, BlobBytes is what the disk actually holds.
// The gap between them is what content addressing bought.
type StorageTotals struct {
	BlobCount  int64 `json:"blob_count"`
	BlobBytes  int64 `json:"blob_bytes"`
	EntryCount int64 `json:"entry_count"`
	EntryBytes int64 `json:"entry_bytes"`
}

// AgeBucket is one bar of the cache-age histogram — how much of the cache has not
// been read in a given span. This is the shape of what eviction would take next.
type AgeBucket struct {
	Label      string `json:"label"`
	MaxAgeDays int    `json:"max_age_days"` // 0 in the final, open-ended bucket
	Entries    int64  `json:"entries"`
	Bytes      int64  `json:"bytes"`
}

// Store is the metadata store contract.
//
// This is the one deliberately wide interface in the codebase: it fronts a single
// database, and splitting it into narrow consumer-side interfaces would scatter the
// schema's shape across the packages that use it. It exists so a Postgres backend
// remains possible for multi-instance deployments without touching call sites — that
// backend is explicitly not being built now.
type Store interface {
	// blobs
	UpsertBlob(b Blob) error
	GetBlob(d blob.Digest) (Blob, error)
	TouchBlobs(ds []blob.Digest, at time.Time) error
	WalkBlobs(fn func(Blob) error) error
	UnreferencedBlobs(olderThan time.Time) ([]blob.Digest, error)

	// entries
	GetEntry(k EntryKey) (Entry, error)
	PutEntry(e Entry) error
	TouchEntries(touches []EntryTouch) error
	CommitEntry(e Entry, artifact *Artifact, quota Quota, replaceArtifactName bool) error
	DeleteEntry(k EntryKey) error
	EvictEntry(k EntryKey) (digest blob.Digest, size int64, stillReferenced bool, err error)
	DeleteProject(project string) (int64, error)
	ListEntries(q EntryQuery) ([]Entry, error)
	WalkEntries(project string, fn func(Entry) error) error
	WalkEntriesEco(project, eco string, fn func(Entry) error) error
	WalkEvictionCandidates(project string, fn func(Entry) error) error
	ApplySnapshot(project, snapshotID string, source EntrySource) error
	ApplySnapshotFrom(project, expectedHead, snapshotID string, source EntrySource) error
	CountEntries(project string) (count int64, bytes int64, err error)

	// refs
	GetRef(k RefKey) (Ref, error)
	PutRef(r Ref) error
	DeleteRef(k RefKey) error
	ListRefs(project, eco, prefix string) ([]Ref, error)

	// artifacts
	PutArtifact(a Artifact) error
	DeleteArtifacts(project, eco, name string) error
	DeleteArtifactVersion(project, eco, name, version string) error
	QueryArtifacts(q ArtifactQuery) ([]Artifact, int, error)

	// snapshots
	PutSnapshot(s Snapshot) error
	CommitSnapshot(s Snapshot) error
	GetSnapshot(id string) (Snapshot, error)
	ListSnapshots(project string, limit int) ([]Snapshot, error)
	WalkSnapshots(fn func(Snapshot) error) error
	SetHead(project, snapshotID string) error
	GetHead(project string) (string, error)

	// stats
	RecordAccess(access []AccessDelta, traffic []TrafficDelta) error
	Stats(q StatsQuery) (StatsResult, error)

	// time series
	RecordSeries(traffic []SeriesDelta, upstream []UpstreamDelta) error
	TrafficSeries(q SeriesQuery) ([]TrafficPoint, error)
	UpstreamSeries(q SeriesQuery) ([]UpstreamPoint, error)
	SampleStorage(s StorageSample) error
	StorageSeries(from, to time.Time) ([]StorageSample, error)
	StorageTotals() (StorageTotals, error)
	EntryAges(project string, now time.Time) ([]AgeBucket, error)
	CompactSeries(now time.Time) error

	// maintenance
	IsBlobReferenced(d blob.Digest) (bool, error)
	DeleteBlobRecord(d blob.Digest) error

	// lifecycle
	Ping() error
	Flush() error
	Close() error
}
