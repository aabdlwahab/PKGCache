package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/brightskies/pkgreg/internal/blob"
	"github.com/brightskies/pkgreg/internal/catalog"
	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/obs"
	"github.com/brightskies/pkgreg/internal/upstream"
)

// ErrNotCached is returned when the cache is offline and does not hold the content.
// The air-gap miss.
var ErrNotCached = errors.New("engine: not cached")

// Expect carries integrity constraints known before a fetch begins.
//
// pypi indexes and OCI descriptors both declare a digest up front. That is worth two
// distinct things: content already in the store can be linked without any upstream
// request at all, and a fetch that returns the wrong bytes is rejected rather than
// cached.
type Expect struct {
	Digest blob.Digest
	Size   int64
	// Room is how many bytes the project's quota still allows, or 0 for unlimited.
	// The fetch abandons a transfer that cannot fit rather than streaming gigabytes
	// it will be forced to discard at commit time.
	Room int64
}

// Resolution is what an ecosystem adapter asks the engine to serve.
//
// The adapter describes; the engine decides. That split is what keeps single-flight,
// hashing, deduplication, integrity checking and catalog bookkeeping out of six
// separate protocol handlers.
type Resolution struct {
	// Project and Eco scope the cache key.
	Project string
	Eco     string
	// Key identifies the content within (Project, Eco).
	Key string

	// Upstream is where to fetch on a miss. Zero value means cache-only: a miss is
	// simply a miss, with no origin request.
	Upstream upstream.Request

	// Expect declares integrity constraints, when the adapter knows them.
	Expect Expect

	// MediaType overrides what upstream reported.
	MediaType string
	// Headers are added to the response (Docker-Content-Digest and friends).
	Headers http.Header

	// Artifact records an inventory row on a successful commit. Nil for content that
	// is cached but not semantically an artifact, such as an index page.
	Artifact *catalog.Artifact
	// AccessName attributes the request in the per-package leaderboard. Empty means
	// the request is not counted, which is right for sub-resources like OCI blobs
	// that a cached pull may skip entirely.
	AccessName string
}

// Outcome reports how a request was satisfied.
type Outcome string

// The vocabulary of cache outcomes.
const (
	OutcomeHit   Outcome = obs.OutcomeHit
	OutcomeDedup Outcome = obs.OutcomeDedup
	OutcomePeer  Outcome = obs.OutcomePeer
	OutcomeMiss  Outcome = obs.OutcomeMiss
	OutcomeFail  Outcome = obs.OutcomeFail
)

// Engine executes resolutions.
type Engine struct {
	blobs    *blob.Store
	cat      catalog.Store
	pool     *upstream.Pool
	cfg      *config.Store
	metrics  *obs.Metrics
	events   *obs.Bus
	inflight *Registry
	stats    *statsCollector
	docs     docGroup
	peer     PeerFetcher

	// usage caches per-project entry totals for the quota pre-flight, so a burst of
	// requests costs one aggregate query rather than one per request.
	usageMu sync.Mutex
	usage   map[string]usageSample

	// baseCtx is the process lifetime. Fetch goroutines derive from it with
	// context.WithoutCancel so a client disconnect cannot abort them.
	baseCtx context.Context
}

type usageSample struct {
	count int64
	bytes int64
	at    time.Time
}

// PeerFetcher is the digest-addressed sibling cache boundary.
type PeerFetcher interface {
	Fetch(context.Context, string, string, blob.Digest) (found bool, size int64, err error)
}

// Options are Engine's dependencies.
type Options struct {
	Blobs   *blob.Store
	Catalog catalog.Store
	Pool    *upstream.Pool
	Config  *config.Store
	Metrics *obs.Metrics
	Events  *obs.Bus
	Context context.Context
	Peer    PeerFetcher
}

// New wires an engine.
func New(o Options) *Engine {
	ctx := o.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return &Engine{
		blobs:    o.Blobs,
		cat:      o.Catalog,
		pool:     o.Pool,
		cfg:      o.Config,
		metrics:  o.Metrics,
		events:   o.Events,
		inflight: NewRegistry(),
		stats:    newStatsCollector(),
		usage:    make(map[string]usageSample),
		baseCtx:  ctx,
		peer:     o.Peer,
	}
}

// Stats exposes the accumulator so the flush loop can drain it.
func (e *Engine) Stats() *statsCollector { return e.stats }

// Inflight exposes the registry for tests and metrics.
func (e *Engine) Inflight() *Registry { return e.inflight }

// Drain waits for detached upstream fetches to finish. HTTP server shutdown only
// knows about request handlers; a client that disconnected deliberately leaves its
// shared cache fill running, so process shutdown needs this second barrier.
func (e *Engine) Drain(ctx context.Context) error { return e.inflight.Wait(ctx) }

// Serve satisfies a resolution over HTTP.
//
// The decision order is fixed:
//
//  1. entry hit             the catalog already maps this key to a blob
//  2. dedup                 the expected digest is already in the store, from
//     another project or ecosystem — link it, no fetch
//  3. offline               serve nothing rather than reaching upstream
//  4. single-flight miss    one fetch, N progressive readers
//
// Step 2 is new relative to the previous design, which could only deduplicate when
// the digest was known in advance AND the CAS happened to hold it. Here every write
// is hashed as it streams, so any blob the host has ever seen is reusable.
func (e *Engine) Serve(w http.ResponseWriter, r *http.Request, res Resolution) (Outcome, error) {
	key := catalog.EntryKey{Project: res.Project, Eco: res.Eco, Key: res.Key}
	now := time.Now()

	if res.AccessName != "" {
		e.stats.recordAccess(res.Project, res.Eco, res.AccessName, now)
	}

	// ---- 1. hit -----------------------------------------------------------
	if entry, err := e.cat.GetEntry(key); err == nil {
		if e.blobs.Exists(entry.Digest) {
			e.stats.recordTouch(key, entry.Digest, now)
			e.record(res, OutcomeHit, entry.Size, now)
			e.applyHeaders(w, res)
			return OutcomeHit, blob.Serve(w, r, e.blobs, entry.Digest, mediaType(res, entry.MediaType))
		}
		// The catalog says we have it and the store does not. That means the blob was
		// collected or removed underneath us; drop the stale row and fall through to
		// re-fetch rather than serving a 404 for content we can get.
		_ = e.cat.DeleteEntry(key)
	} else if !errors.Is(err, catalog.ErrNotFound) {
		return OutcomeFail, err
	}

	// ---- 2. dedup ---------------------------------------------------------
	if res.Expect.Digest != "" && e.blobs.Exists(res.Expect.Digest) {
		st, _ := e.blobs.Stat(res.Expect.Digest)
		if err := e.link(key, res, res.Expect.Digest, st.Size, now); err != nil {
			return OutcomeFail, err
		}
		e.stats.recordTouch(key, res.Expect.Digest, now)
		e.record(res, OutcomeDedup, st.Size, now)
		e.applyHeaders(w, res)
		return OutcomeDedup, blob.Serve(w, r, e.blobs, res.Expect.Digest, mediaType(res, ""))
	}

	// ---- 3. peer ----------------------------------------------------------
	if res.Expect.Digest != "" && e.peer != nil {
		if found, size, _ := e.peer.Fetch(
			r.Context(), res.Project, res.Eco, res.Expect.Digest,
		); found {
			if err := e.link(key, res, res.Expect.Digest, size, now); err != nil {
				return OutcomeFail, err
			}
			e.stats.recordTouch(key, res.Expect.Digest, now)
			e.record(res, OutcomePeer, size, now)
			e.applyHeaders(w, res)
			return OutcomePeer, blob.Serve(
				w, r, e.blobs, res.Expect.Digest, mediaType(res, ""),
			)
		}
	}

	// ---- 4. offline -------------------------------------------------------
	if e.offline(res.Project) || res.Upstream.URL == "" {
		e.record(res, OutcomeFail, 0, now)
		return OutcomeFail, fmt.Errorf("%w: %s/%s/%s", ErrNotCached, res.Project, res.Eco, res.Key)
	}

	// ---- 5. quota -----------------------------------------------------------
	// Refuse before the transfer, not after. Enforcing only at entry-commit time
	// meant an over-quota project re-downloaded the same artifact on every request:
	// the bytes landed in the store, the entry insert was rejected, and the next
	// request found no entry and started again. Checked here so the client gets one
	// clear 507 instead of an unbounded upstream bill.
	if err := e.checkQuota(res.Project, res.Expect.Size); err != nil {
		e.record(res, OutcomeFail, 0, now)
		return OutcomeFail, err
	}

	// ---- 6. single-flight miss --------------------------------------------
	return e.serveMiss(w, r, res, key, now)
}

func (e *Engine) serveMiss(
	w http.ResponseWriter, r *http.Request, res Resolution, key catalog.EntryKey, now time.Time,
) (Outcome, error) {
	// One fetch per (project, eco, key): a hundred simultaneous requests for the same
	// wheel produce exactly one upstream transfer.
	fetchKey := res.Project + "\x00" + res.Eco + "\x00" + res.Key
	f, created := e.inflight.Start(fetchKey, res.Eco)
	if created {
		// Close the lookup→registry TOCTOU window. Another fetch may have published
		// this entry after Serve's first catalog lookup but before this request won
		// Registry.Start. Without the second lookup, a late concurrent request can
		// become the owner of a needless second upstream transfer.
		if entry, err := e.cat.GetEntry(key); err == nil && e.blobs.Exists(entry.Digest) {
			f.finish(entry.Digest, entry.Size, nil)
			e.stats.recordTouch(key, entry.Digest, now)
			e.record(res, OutcomeHit, entry.Size, now)
			e.applyHeaders(w, res)
			return OutcomeHit, blob.Serve(
				w, r, e.blobs, entry.Digest, mediaType(res, entry.MediaType),
			)
		}
		req := res.Upstream
		req.Eco = res.Eco
		// Publish the entry before Fetch marks itself done and removes itself from
		// the registry. If publication were left to a reader after Wait returns,
		// a late request could land in the tiny done→entry window, see neither an
		// entry nor an in-flight fetch, and start a duplicate upstream transfer.
		//
		// This is also the ONLY place the entry is written for a miss. Readers used to
		// repeat it, which double-counted byte traffic once per reader and re-ran the
		// quota check for work already done.
		want := res.Expect
		want.Room = e.quotaRoom(res.Project)
		// The detached context is the deliberate exception recorded in the ground rules:
		// a client pressing Ctrl-C must not abort a transfer other readers are consuming
		// and the cache is about to keep.
		//nolint:contextcheck // documented detachment; see runFetch
		go e.runFetch(f, req, want, func(digest blob.Digest, size int64) {
			e.publishEntry(key, res, digest, size, now)
		}, res.Project)
	}

	// HEAD and Range cannot ride a progressive stream: a range needs random access
	// into content that does not fully exist yet, and a HEAD has no body to stream.
	// Both wait for the commit and then hand off to http.ServeContent, which does
	// them properly.
	if r.Method == http.MethodHead || r.Header.Get("Range") != "" {
		if err := f.Wait(r.Context()); err != nil {
			return OutcomeFail, err
		}
		digest, size, ferr := f.Digest()
		if ferr != nil {
			e.record(res, OutcomeFail, 0, now)
			return OutcomeFail, ferr
		}
		// A HEAD sends no body, so it must not be billed for one.
		served := size
		if r.Method == http.MethodHead {
			served = 0
		}
		e.record(res, OutcomeMiss, served, now)
		e.applyHeaders(w, res)
		return OutcomeMiss, blob.Serve(w, r, e.blobs, digest, mediaType(res, f.MediaType))
	}

	body, err := f.Reader(r.Context())
	switch {
	case errors.Is(err, errCommitted):
		// The fetch finished between our joining it and attaching a reader. Serving
		// the committed blob is strictly better anyway.
		digest, size, ferr := f.Digest()
		if ferr != nil {
			e.record(res, OutcomeFail, 0, now)
			return OutcomeFail, ferr
		}
		e.record(res, OutcomeMiss, size, now)
		e.applyHeaders(w, res)
		return OutcomeMiss, blob.Serve(w, r, e.blobs, digest, mediaType(res, f.MediaType))
	case err != nil:
		e.record(res, OutcomeFail, 0, now)
		return OutcomeFail, err
	}
	defer func() { _ = body.Close() }()

	e.applyHeaders(w, res)
	if ct := mediaType(res, f.MediaType); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// Only claim a length upstream actually declared. Setting one we guessed would
	// desynchronise the response framing if the transfer came up short.
	if f.Total >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(f.Total, 10))
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusOK)

	written, copyErr := io.Copy(w, body)

	// The download continues regardless of what happened to this client: the fetch
	// goroutine is detached, and other readers plus the cache itself still want it.
	// The entry is published by that goroutine, so nothing is needed here beyond
	// letting it finish before the request is considered served.
	_ = f.Wait(context.WithoutCancel(r.Context()))

	if copyErr != nil {
		e.record(res, OutcomeFail, written, now)
		return OutcomeFail, copyErr
	}
	e.record(res, OutcomeMiss, written, now)
	return OutcomeMiss, nil
}

// publishEntry records a completed fetch in the catalog, exactly once per fetch.
//
// A failure here is not fatal to the request — the bytes are already in the store and
// already going out to the client — but it must never be silent. An entry that cannot
// be written means the next request for this key misses again, so a persistent failure
// is a re-fetch loop, and that is worth an event and a log line rather than a
// discarded error.
func (e *Engine) publishEntry(
	key catalog.EntryKey, res Resolution, digest blob.Digest, size int64, now time.Time,
) {
	if err := e.link(key, res, digest, size, now); err != nil {
		kind := obs.EventFetchError
		detail := "cache entry not recorded: " + err.Error()
		var quota *catalog.QuotaError
		if errors.As(err, &quota) {
			detail = "cache entry rejected by project quota: " + quota.Error()
		}
		obs.LoggerFrom(e.baseCtx).Warn("entry not published",
			"project", res.Project, "eco", res.Eco, "key", res.Key, "error", err)
		e.events.Publish(obs.Event{
			Kind: kind, Project: res.Project, Eco: res.Eco, ID: res.Key, Detail: detail,
		})
		return
	}
	e.invalidateUsage(res.Project)
}

// link writes the entry row and, when the adapter asked for one, the inventory row.
func (e *Engine) link(
	key catalog.EntryKey, res Resolution, digest blob.Digest, size int64, now time.Time,
) error {
	return e.blobs.WithBlob(digest, func(st blob.Stat) error {
		if size <= 0 {
			size = st.Size
		}
		var artifact *catalog.Artifact
		if res.Artifact != nil {
			a := *res.Artifact
			a.Project, a.Eco = res.Project, res.Eco
			a.Digest, a.Size = digest, size
			if a.CachedAt.IsZero() {
				a.CachedAt = now
			}
			artifact = &a
		}
		return e.cat.CommitEntry(catalog.Entry{
			EntryKey: key, Digest: digest, Size: size,
			MediaType: res.MediaType, CachedAt: now, LastAccess: now,
		}, artifact, e.quota(res.Project), false)
	})
}

func (e *Engine) quota(project string) catalog.Quota {
	if e.cfg == nil {
		return catalog.Quota{}
	}
	p := e.cfg.Current().Projects[project]
	return catalog.Quota{Bytes: p.QuotaBytes, Artifacts: p.QuotaArtifacts}
}

// checkQuota rejects a fill that the project has no room for.
//
// The usage figure is cached for quotaUsageTTL because the underlying query is an
// aggregate over one project's entries — affordable periodically, not per request in
// a `uv sync` burst. A project with no quota configured, which is the normal case,
// costs one map lookup and never queries at all. Commit-time enforcement in
// CommitEntry remains the authority; this only stops the pointless transfer.
func (e *Engine) checkQuota(project string, incoming int64) error {
	quota := e.quota(project)
	if quota.Bytes <= 0 && quota.Artifacts <= 0 {
		return nil
	}
	count, usage, err := e.projectUsage(project)
	if err != nil {
		// Never fail a download because the usage query failed; CommitEntry still
		// enforces the limit authoritatively.
		return nil //nolint:nilerr // an unreadable usage figure must not break serving
	}
	if quota.Bytes > 0 {
		attempt := usage + incoming
		if attempt <= usage {
			attempt = usage + 1
		}
		if usage >= quota.Bytes || attempt > quota.Bytes {
			return &catalog.QuotaError{
				Kind: "bytes", Usage: usage, Limit: quota.Bytes, Attempt: attempt,
			}
		}
	}
	if quota.Artifacts > 0 && count >= quota.Artifacts {
		return &catalog.QuotaError{
			Kind: "artifacts", Usage: count, Limit: quota.Artifacts, Attempt: count + 1,
		}
	}
	return nil
}

// quotaRoom reports how many bytes the project's byte quota still allows, or 0 when
// it is unlimited. A project already at or over its limit reports 1 rather than 0 or a
// negative number, so "unlimited" stays unambiguous and any transfer is refused.
func (e *Engine) quotaRoom(project string) int64 {
	limit := e.quota(project).Bytes
	if limit <= 0 {
		return 0
	}
	_, usage, err := e.projectUsage(project)
	if err != nil {
		return 0
	}
	if room := limit - usage; room > 0 {
		return room
	}
	return 1
}

// quotaExceeded builds the error an adapter renders as a 507 with current usage.
func (e *Engine) quotaExceeded(project string, attempted int64) error {
	limit := e.quota(project).Bytes
	_, usage, err := e.projectUsage(project)
	if err != nil {
		usage = 0
	}
	return &catalog.QuotaError{
		Kind: "bytes", Usage: usage, Limit: limit, Attempt: usage + attempted,
	}
}

// quotaUsageTTL bounds how stale a cached usage figure may be. Short enough that a
// project cannot overshoot its quota by much, long enough that a burst of thousands
// of requests costs one query.
const quotaUsageTTL = 2 * time.Second

func (e *Engine) projectUsage(project string) (count, bytes int64, err error) {
	now := time.Now()
	e.usageMu.Lock()
	if cached, ok := e.usage[project]; ok && now.Sub(cached.at) < quotaUsageTTL {
		e.usageMu.Unlock()
		return cached.count, cached.bytes, nil
	}
	e.usageMu.Unlock()

	count, bytes, err = e.cat.CountEntries(project)
	if err != nil {
		return 0, 0, err
	}
	e.usageMu.Lock()
	e.usage[project] = usageSample{count: count, bytes: bytes, at: now}
	e.usageMu.Unlock()
	return count, bytes, nil
}

// invalidateUsage drops a cached usage figure after a write, so the next check sees
// the new total rather than waiting out the TTL.
func (e *Engine) invalidateUsage(project string) {
	e.usageMu.Lock()
	delete(e.usage, project)
	e.usageMu.Unlock()
}

// record accounts for one served request.
//
// size is the bytes this response actually put on the wire: zero for a HEAD, the
// copied count for a progressive stream. It is exact for every full-body response and
// approximate only for a Range request, where counting precisely would mean wrapping
// the ResponseWriter and giving up the sendfile path that makes large reads cheap.
func (e *Engine) record(res Resolution, outcome Outcome, size int64, now time.Time) {
	e.metrics.Requests.WithLabelValues(res.Eco, res.Project, string(outcome)).Inc()
	if size > 0 {
		e.metrics.BytesServed.WithLabelValues(res.Eco, res.Project).Add(float64(size))
	}
	// Every outcome lands in the series, failures included. The five steps are the
	// cache's own vocabulary, and a chart that folded them into hit-or-not would hide
	// the difference between a local entry, a coalesced fetch and a peer — which is
	// most of what an operator is trying to find out.
	e.stats.recordSeries(res.Project, res.Eco, string(outcome), size, now)

	switch outcome {
	case OutcomeHit, OutcomeDedup, OutcomePeer:
		e.stats.recordTraffic(res.Project, res.Eco, true, size)
		e.events.Publish(obs.Event{
			Kind: obs.EventCacheHit, Project: res.Project, Eco: res.Eco,
			ID: res.Key, Size: size,
		})
	case OutcomeMiss:
		e.stats.recordTraffic(res.Project, res.Eco, false, size)
	case OutcomeFail:
	}
}

func (e *Engine) applyHeaders(w http.ResponseWriter, res Resolution) {
	for k, vs := range res.Headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}

// offline reports whether this project must serve from cache only, combining the
// instance-wide hard mode with the project's own soft flag. Read per request from the
// live snapshot, so flipping it takes effect immediately rather than on a poll.
func (e *Engine) offline(project string) bool {
	if e.cfg == nil {
		return false
	}
	return e.cfg.Current().OfflineFor(project)
}

func mediaType(res Resolution, fallback string) string {
	if res.MediaType != "" {
		return res.MediaType
	}
	return fallback
}
