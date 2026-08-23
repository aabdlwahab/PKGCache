package obs

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/aabdlwahab/PKGCache/internal/buildinfo"
)

// Outcome labels for a served request. These are the vocabulary of the cache: every
// data-plane request ends as exactly one of them.
const (
	OutcomeHit   = "hit"   // served from this project's catalog
	OutcomeDedup = "dedup" // blob already present from another project or ecosystem
	OutcomePeer  = "peer"  // fetched from a sibling instance
	OutcomeMiss  = "miss"  // fetched from the origin upstream
	OutcomeFail  = "fail"
)

// Metrics is the process metric set. It is passed explicitly rather than kept in a
// package global so tests can build a throwaway registry.
//
// A note on *Vec metrics: a Vec is a factory, not a metric. Until some label
// combination is observed it has no children, so it contributes nothing to a scrape
// — not even HELP and TYPE lines. That matters for alerting, because
// `rate(pkgreg_requests_total{outcome="fail"}[5m])` returns *no data* rather than
// zero when nothing has failed, which renders as an outage on a dashboard.
//
// The fix is to pre-create the series that are knowable. Here `eco` and `outcome`
// are bounded and `project` is not, so the right moment is project registration:
// InitProjectSeries below is called when the control plane learns of a project.
type Metrics struct {
	reg *prometheus.Registry

	Requests        *prometheus.CounterVec   // eco, project, outcome
	BytesServed     *prometheus.CounterVec   // eco, project
	UpstreamBytes   *prometheus.CounterVec   // eco, upstream
	UpstreamErrors  *prometheus.CounterVec   // upstream, code
	FetchDuration   *prometheus.HistogramVec // eco
	InflightFetches *prometheus.GaugeVec     // eco
	CatalogQuery    *prometheus.HistogramVec // query
	BlobStoreBytes  prometheus.Gauge
	BlobCount       prometheus.Gauge
	GCReclaimed     prometheus.Counter
	EvictedEntries  *prometheus.CounterVec // project, reason
	JobDuration     *prometheus.HistogramVec
	EventsDropped   prometheus.Counter
}

// NewMetrics registers the metric set on a fresh registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pkgreg_requests_total",
			Help: "Data-plane requests by ecosystem, project and cache outcome.",
		}, []string{"eco", "project", "outcome"}),
		BytesServed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pkgreg_bytes_served_total",
			Help: "Bytes written to clients.",
		}, []string{"eco", "project"}),
		UpstreamBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pkgreg_upstream_bytes_total",
			Help: "Bytes fetched from upstreams (what the cache saved on a later hit).",
		}, []string{"eco", "upstream"}),
		UpstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pkgreg_upstream_errors_total",
			Help: "Failed upstream requests by status code (or transport error).",
		}, []string{"upstream", "code"}),
		FetchDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "pkgreg_fetch_duration_seconds",
			Help: "Upstream fetch wall time. Buckets span a small index to a multi-GB wheel.",
			// 50ms → ~7min: a packument is at the bottom, a 2.5GB CUDA wheel at the top.
			Buckets: []float64{0.05, 0.25, 1, 5, 15, 60, 180, 420},
		}, []string{"eco"}),
		InflightFetches: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pkgreg_inflight_fetches",
			Help: "Upstream fetches currently running.",
		}, []string{"eco"}),
		CatalogQuery: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "pkgreg_catalog_query_seconds",
			Help:    "Catalog query latency. Watched to justify the pure-Go SQLite driver.",
			Buckets: []float64{50e-6, 200e-6, 1e-3, 5e-3, 50e-3, 500e-3},
		}, []string{"query"}),
		BlobStoreBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "pkgreg_blob_store_bytes", Help: "Total bytes held in the blob store.",
		}),
		BlobCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "pkgreg_blob_count", Help: "Distinct blobs held.",
		}),
		GCReclaimed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "pkgreg_gc_reclaimed_bytes_total", Help: "Bytes reclaimed by garbage collection.",
		}),
		EvictedEntries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pkgreg_evicted_entries_total", Help: "Cache entries evicted.",
		}, []string{"project", "reason"}),
		JobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "pkgreg_job_duration_seconds",
			Help:    "Control-plane job wall time.",
			Buckets: prometheus.ExponentialBuckets(1, 4, 8), // 1s → ~4.5h
		}, []string{"action", "status"}),
		EventsDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "pkgreg_events_dropped_total", Help: "Bus events dropped by slow subscribers.",
		}),
	}

	b := buildinfo.Get()
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pkgreg_build_info", Help: "Build identity; always 1.",
	}, []string{"version", "commit", "go_version", "platform"})
	info.WithLabelValues(b.Version, b.Commit, b.GoVersion, b.Platform).Set(1)

	reg.MustRegister(
		m.Requests, m.BytesServed, m.UpstreamBytes, m.UpstreamErrors,
		m.FetchDuration, m.InflightFetches, m.CatalogQuery,
		m.BlobStoreBytes, m.BlobCount, m.GCReclaimed, m.EvictedEntries,
		m.JobDuration, m.EventsDropped, info,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Ecosystems is the label vocabulary for pre-creating series. Kept here so
// InitProjectSeries needs no import of the ecosystem registry, which would be a
// dependency cycle: eco depends on obs, not the other way round.
var Ecosystems = []string{"oci", "npm", "pypi", "apt", "apk", "git", "files"}

// Outcomes enumerates every way a data-plane request can end.
var Outcomes = []string{OutcomeHit, OutcomeDedup, OutcomePeer, OutcomeMiss, OutcomeFail}

// InitProjectSeries creates the zero-valued series for a project, so a dashboard
// shows 0 instead of "no data" before the first request and an alert rule has
// something to evaluate. Idempotent — WithLabelValues returns the existing child.
func (m *Metrics) InitProjectSeries(project string) {
	for _, eco := range Ecosystems {
		m.BytesServed.WithLabelValues(eco, project)
		for _, outcome := range Outcomes {
			m.Requests.WithLabelValues(eco, project, outcome)
		}
	}
}

// Handler serves the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Registry exposes the underlying registry so other subsystems can register their
// own collectors without reaching for a global.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }
