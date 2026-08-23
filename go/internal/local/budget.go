package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/diskusage"
)

// The disk policy, which is where pkgcache diverges from pkgreg on purpose rather
// than by omission.
//
// A server evicts: it holds a size an operator chose, and discards the least recently
// used content to stay there. On a laptop that is the wrong default. The wheels in
// this cache are the ones somebody's current work depends on, and a background process
// deleting them to hold a number nobody chose is a worse outcome than running out of
// room. So:
//
//   - the limit is set by the user, explicitly, before the cache serves anything;
//   - nothing is ever deleted unless `pkgcache prune` or `pkgcache gc` is run;
//   - a full cache stops caching, loudly. It does not stop the build, and it does not
//     start deleting.

// DefaultMinFree is the free-space floor kept underneath whatever limit is set.
//
// pkgcache must never be the reason a disk fills, and a limit alone cannot promise
// that: 25 GiB is a fine limit on a disk with 200 GiB free and a catastrophe on one
// with 6 GiB left.
const DefaultMinFree int64 = 5 << 30

// ErrNoLimit reports that this cache has no budget yet.
//
// The message is the whole interaction: pkgcache asks for a size once, rather than
// picking one for somebody else's disk, so the error a person meets has to contain the
// two commands that answer it.
var ErrNoLimit = errors.New(
	"no cache limit has been set, and pkgcache will not guess one for your disk\n" +
		"  pkgcache limit 25G     cap the cache at 25 GiB\n" +
		"  pkgcache limit none    no cap; a free-space floor still applies\n" +
		"  PKGCACHE_LIMIT=25G     the same thing, for CI")

// Budget is the disk policy for one cache directory.
type Budget struct {
	// LimitBytes is the cache's size budget. Zero means no limit was chosen, which is
	// a refusal to serve rather than a default; NoLimit means one was chosen and it is
	// "as much as the disk allows".
	LimitBytes int64 `json:"limit_bytes"`
	// MinFreeBytes is the free-space floor. Zero uses DefaultMinFree.
	MinFreeBytes int64 `json:"min_free_bytes"`
}

// NoLimit is the stored value for `pkgcache limit none`: a deliberate choice of no
// cap, which is a different thing from never having chosen.
const NoLimit int64 = -1

// budgetPath is where the choice is kept. Its own file rather than a line in the
// daemon state, because state describes a running process and this outlives every one.
func budgetPath(dataDir string) string { return filepath.Join(dataDir, "budget.json") }

// ReadBudget returns this cache's budget, or ErrNoLimit if none has been set.
//
// PKGCACHE_LIMIT overrides the stored value and is what makes the requirement one
// setup line in CI rather than an interactive prompt nobody can answer there.
func ReadBudget(dataDir string) (Budget, error) {
	if raw := strings.TrimSpace(os.Getenv("PKGCACHE_LIMIT")); raw != "" {
		limit, err := ParseSize(raw)
		if err != nil {
			return Budget{}, fmt.Errorf("PKGCACHE_LIMIT: %w", err)
		}
		return Budget{LimitBytes: limit}, nil
	}
	data, err := os.ReadFile(budgetPath(dataDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Budget{}, ErrNoLimit
		}
		return Budget{}, fmt.Errorf("local: read cache budget: %w", err)
	}
	var budget Budget
	if err := json.Unmarshal(data, &budget); err != nil {
		return Budget{}, ErrNoLimit
	}
	if budget.LimitBytes == 0 {
		return Budget{}, ErrNoLimit
	}
	return budget, nil
}

// WriteBudget records the user's choice.
func WriteBudget(dataDir string, budget Budget) error {
	data, err := json.MarshalIndent(budget, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(budgetPath(dataDir), append(data, '\n'), 0o600)
}

// ParseSize reads the sizes people write: 25G, 500M, 2TiB, 1024, none.
func ParseSize(value string) (int64, error) {
	text := strings.TrimSpace(strings.ToLower(value))
	if text == "none" || text == "unlimited" {
		return NoLimit, nil
	}
	text = strings.TrimSuffix(text, "b")
	multiplier := int64(1)
	for suffix, factor := range map[string]int64{
		"k": 1 << 10, "ki": 1 << 10,
		"m": 1 << 20, "mi": 1 << 20,
		"g": 1 << 30, "gi": 1 << 30,
		"t": 1 << 40, "ti": 1 << 40,
	} {
		if strings.HasSuffix(text, suffix) {
			// Longest match wins, so "gi" is not read as "i" after a "g" is trimmed.
			if factor >= multiplier {
				trimmed := strings.TrimSuffix(text, suffix)
				if _, err := strconv.ParseFloat(trimmed, 64); err == nil {
					multiplier = factor
				}
			}
		}
	}
	if multiplier > 1 {
		for _, suffix := range []string{"ki", "mi", "gi", "ti", "k", "m", "g", "t"} {
			if strings.HasSuffix(text, suffix) {
				text = strings.TrimSuffix(text, suffix)
				break
			}
		}
	}
	amount, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size; write 25G, 500M or none", value)
	}
	if amount <= 0 {
		return 0, fmt.Errorf("a cache limit must be positive; write `none` for no limit")
	}
	return int64(amount * float64(multiplier)), nil
}

// FormatSize renders bytes the way the size flags are written.
func FormatSize(n int64) string {
	if n == NoLimit {
		return "none"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for value := n / unit; value >= unit; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Usage is what a caller needs in order to say whether the cache is full and by how
// much.
type Usage struct {
	Bytes     int64
	Objects   int64
	FreeBytes int64
	Budget    Budget
	Full      bool
	Reason    string
}

// Guard enforces a Budget on the fill path. It satisfies engine.StoreGuard.
//
// The store size is sampled rather than measured per request: walking a large blob
// tree on every miss would cost more than the download. A sample that is up to
// sampleInterval stale can overshoot the limit by whatever arrives in that window,
// which is the deliberate trade — the floor underneath means the overshoot can never
// reach the disk.
type Guard struct {
	blobs *blob.Store
	dir   string

	budget Budget

	mu      sync.Mutex
	sampled time.Time
	bytes   int64
	free    int64

	// full is read by anything reporting status and written by the fill path, so it is
	// atomic rather than guarded: a status command must never block behind a fetch.
	full   atomic.Bool
	reason atomic.Pointer[string]

	// notified throttles the desktop notification. A cache that is full stays full
	// until somebody acts, and a toast every few seconds would train them not to look.
	notifyMu   sync.Mutex
	lastNotify time.Time
	notify     func(string)
}

const sampleInterval = 10 * time.Second

// NewGuard builds the guard for a cache directory.
//
// The blob store may be nil at construction: the engine needs a guard before the
// store exists, because both are built by the same composition root. attach supplies
// it, and until then the guard measures nothing and stores everything, which is the
// right answer for the handful of milliseconds involved.
func NewGuard(blobs *blob.Store, dataDir string, budget Budget, notify func(string)) *Guard {
	if budget.MinFreeBytes <= 0 {
		budget.MinFreeBytes = DefaultMinFree
	}
	return &Guard{blobs: blobs, dir: dataDir, budget: budget, notify: notify}
}

// attach supplies the blob store once the composition root has built it.
func (g *Guard) attach(blobs *blob.Store) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.blobs = blobs
}

// MayStore reports whether an incoming artifact may be kept.
func (g *Guard) MayStore(size int64) (bool, string) {
	bytes, free, remeasured := g.sample()
	if size < 0 {
		size = 0
	}
	wasFull, _ := g.Full()
	if g.budget.LimitBytes > 0 && bytes+size > g.budget.LimitBytes {
		return g.refuse(bytes, free, wasFull, fmt.Sprintf(
			"the cache is full: %s of a %s limit used. Run `pkgcache prune` to reclaim "+
				"space, or raise the limit with `pkgcache limit`",
			FormatSize(bytes), FormatSize(g.budget.LimitBytes)))
	}
	if free > 0 && free-size < g.budget.MinFreeBytes {
		return g.refuse(bytes, free, wasFull, fmt.Sprintf(
			"the disk is nearly full: %s free, and pkgcache keeps %s in reserve. Run "+
				"`pkgcache prune` to reclaim space",
			FormatSize(free), FormatSize(g.budget.MinFreeBytes)))
	}
	g.full.Store(false)
	g.reason.Store(nil)
	// Published on a transition or a fresh measurement, never on every artifact: this
	// is a file write on the fill path, and a `uv sync` fires thousands of requests.
	if wasFull || remeasured {
		g.publish(bytes, free)
	}
	return true, ""
}

// refuse records the decision and publishes it.
//
// The publish is here rather than inside sample on purpose. It used to happen while
// measuring, which meant the record was written before the verdict it was supposed to
// carry: a cache that had just refused an artifact published `full: false`, and every
// client reading it — `pkgcache run`, `pkgcache status` — reported a healthy cache
// that was in fact storing nothing.
func (g *Guard) refuse(bytes, free int64, wasFull bool, reason string) (bool, string) {
	g.full.Store(true)
	g.reason.Store(&reason)
	if !wasFull {
		// The first refusal is the one worth interrupting somebody for.
		g.maybeNotify(reason)
	}
	g.publish(bytes, free)
	return false, reason
}

func (g *Guard) publish(bytes, free int64) {
	full, reason := g.Full()
	PublishUsage(g.dir, Usage{
		Bytes: bytes, FreeBytes: free, Budget: g.budget, Full: full, Reason: reason,
	})
}

// Full reports whether the cache has stopped storing, and why.
func (g *Guard) Full() (bool, string) {
	if g == nil || !g.full.Load() {
		return false, ""
	}
	if reason := g.reason.Load(); reason != nil {
		return true, *reason
	}
	return true, "the cache is full"
}

// Usage measures the cache now, ignoring the sampling interval. This is what `status`
// reports, and a person who asked deserves the real number.
func (g *Guard) Usage() Usage {
	bytes, free := g.measure()
	g.mu.Lock()
	g.bytes, g.free, g.sampled = bytes, free, time.Now()
	g.mu.Unlock()
	full, reason := g.Full()
	var objects int64
	if g.blobs != nil {
		objects, _, _ = g.blobs.Usage()
	}
	return Usage{
		Bytes: bytes, Objects: objects, FreeBytes: free,
		Budget: g.budget, Full: full, Reason: reason,
	}
}

// sample returns the store size and free disk, re-measuring at most every
// sampleInterval, and reports whether this call did the measuring.
func (g *Guard) sample() (bytes, free int64, remeasured bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.sampled) < sampleInterval {
		return g.bytes, g.free, false
	}
	g.bytes, g.free = g.measure()
	g.sampled = time.Now()
	return g.bytes, g.free, true
}

func (g *Guard) measure() (bytes, free int64) {
	if g.blobs != nil {
		if _, used, err := g.blobs.Usage(); err == nil {
			bytes = used
		}
	}
	if available, _, err := diskusage.Usage(g.dir); err == nil {
		free = available
	}
	return bytes, free
}

// maybeNotify raises a desktop notification at most once an hour.
func (g *Guard) maybeNotify(reason string) {
	if g.notify == nil {
		return
	}
	g.notifyMu.Lock()
	defer g.notifyMu.Unlock()
	if time.Since(g.lastNotify) < time.Hour {
		return
	}
	g.lastNotify = time.Now()
	go g.notify(reason)
}
