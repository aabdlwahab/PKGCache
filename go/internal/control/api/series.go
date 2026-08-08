package api

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/brightskies/pkgreg/internal/catalog"
	"github.com/brightskies/pkgreg/internal/control"
	"github.com/brightskies/pkgreg/internal/diskusage"
)

// defaultWindow is what a caller gets with no explicit range: recent enough to be
// about right now, short enough that the fine resolution still exists.
const defaultWindow = 6 * time.Hour

func (a *API) seriesRoutes() {
	a.route("GET /api/v1/coordinates", a.getCoordinates)
	a.route("GET /api/v1/stats/series", a.getTrafficSeries)
	a.route("GET /api/v1/stats/storage", a.getStorageSeries)
	a.route("GET /api/v1/stats/upstreams", a.getUpstreamSeries)
	a.route("GET /api/v1/stats/ages", a.getEntryAges)
}

// getCoordinates reports the addresses this instance actually answers on, plus the
// fingerprint of the CA it presents.
//
// Unauthenticated on purpose. The landing page and the tutorial are public, this
// process serves them, and every value here is something the caller already knows by
// virtue of having reached the port they are asking on. Printing
// "cache.example.com:8443" in a page the server itself serves makes every reader
// translate two values by hand, and the port is the one people get wrong.
//
// ca_sha256 is here rather than only on the project endpoints route because that route
// requires a login the tutorial's reader does not have yet. The effect was that on
// every instance with authentication switched on — the recommended production posture
// — the public getting-started page silently left "PASTE_FINGERPRINT" in the command it
// told people to copy. The value is not a secret in any case: it is a digest of the
// certificate this same server hands to anyone who asks at /api/ca.crt, and pinning it
// is what makes the first download safe. Withholding it protected nothing and broke the
// one step a newcomer cannot work around.
func (a *API) getCoordinates(w http.ResponseWriter, r *http.Request) error {
	host, unifiedPort, proxyPort, err := a.clientCoordinates(r)
	if err != nil {
		return err
	}
	snapshot := a.Config.Current()
	scheme := clientScheme(r, snapshot)
	body := map[string]any{
		"host":   host,
		"scheme": scheme,
		// Pre-joined so a client never has to reason about bracketing IPv6 itself.
		"unified":     net.JoinHostPort(host, strconv.Itoa(unifiedPort)),
		"proxy":       net.JoinHostPort(host, strconv.Itoa(proxyPort)),
		"single_port": snapshot.Server.SinglePort,
		"console":     scheme + "://" + net.JoinHostPort(host, strconv.Itoa(unifiedPort)),
	}
	// An instance with no TLS material has no fingerprint to report, and that is a
	// legitimate configuration rather than a failure — so the field is absent instead
	// of the request being.
	if fingerprint, err := a.caFingerprint(); err == nil {
		body["ca_sha256"] = fingerprint
	}
	writeJSON(w, http.StatusOK, body)
	return nil
}

// getTrafficSeries answers the request-shape-over-time question.
//
// The response carries the span it actually used, which is rarely the one asked for:
// a caller requesting five-minute detail over a month is asking for buckets that were
// folded away days ago, and answering with an empty array would look like an outage.
func (a *API) getTrafficSeries(w http.ResponseWriter, r *http.Request) error {
	query := r.URL.Query()
	project := query.Get("project")
	if project != "" {
		if _, _, err := a.requireView(r, project); err != nil {
			return err
		}
	} else if _, err := a.guard.RequireAuthed(r); err != nil {
		return err
	}

	from, to, err := parseWindow(query.Get("from"), query.Get("to"))
	if err != nil {
		return err
	}
	span, err := resolveSpan(query.Get("span"), from)
	if err != nil {
		return err
	}

	points, err := a.Catalog.TrafficSeries(catalog.SeriesQuery{
		Project: project, Eco: query.Get("eco"), Span: span,
		From: from, To: to, GroupBy: query.Get("by"),
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"span":   span,
		"from":   from,
		"to":     to,
		"points": points,
		// Absent buckets mean "not recorded", not "no traffic" — a flush window can be
		// dropped when the database is unavailable. Say so in the payload so a client
		// has no excuse for interpolating across a gap.
		"gaps_are_unknown": true,
	})
	return nil
}

func (a *API) getStorageSeries(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.guard.RequireAuthed(r); err != nil {
		return err
	}
	from, to, err := parseWindow(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		return err
	}
	samples, err := a.Catalog.StorageSeries(from, to)
	if err != nil {
		return err
	}
	current, err := a.storageNow()
	if err != nil {
		return err
	}
	// History comes from the hourly sampler; "current" is measured on this request.
	// The gauge an operator watches while a disk fills must not be up to an hour old.
	writeJSON(w, http.StatusOK, map[string]any{
		"samples": samples,
		"current": current,
	})
	return nil
}

func (a *API) getUpstreamSeries(w http.ResponseWriter, r *http.Request) error {
	query := r.URL.Query()
	project := query.Get("project")
	if project != "" {
		if _, _, err := a.requireView(r, project); err != nil {
			return err
		}
	} else if _, err := a.guard.RequireAuthed(r); err != nil {
		return err
	}
	from, to, err := parseWindow(query.Get("from"), query.Get("to"))
	if err != nil {
		return err
	}
	points, err := a.Catalog.UpstreamSeries(catalog.SeriesQuery{
		Project: project, From: from, To: to,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"points": points,
		// There are no percentiles here on purpose: latency distribution is
		// Prometheus's job, and duplicating a histogram in SQLite would cost far more
		// than the question this view asks.
		"latency": "mean and max only",
	})
	return nil
}

func (a *API) getEntryAges(w http.ResponseWriter, r *http.Request) error {
	project := r.URL.Query().Get("project")
	if project != "" {
		if _, _, err := a.requireView(r, project); err != nil {
			return err
		}
	} else if _, err := a.guard.RequireAuthed(r); err != nil {
		return err
	}
	buckets, err := a.Catalog.EntryAges(project, time.Now())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": list(buckets)})
	return nil
}

// storageDetail is the current size of the store plus the room left around it.
type storageDetail struct {
	catalog.StorageTotals
	FSFree  int64 `json:"fs_free"`
	FSTotal int64 `json:"fs_total"`
	// MinFreeBytes is the eviction floor, so a gauge can be drawn against the
	// threshold the system will actually act on rather than an arbitrary one.
	MinFreeBytes int64 `json:"min_free_bytes"`
}

func (a *API) storageNow() (storageDetail, error) {
	totals, err := a.Catalog.StorageTotals()
	if err != nil {
		return storageDetail{}, err
	}
	detail := storageDetail{
		StorageTotals: totals,
		MinFreeBytes:  a.Config.Current().Maintenance.EvictMinFreeBytes,
	}
	free, total, err := diskusage.Usage(a.DataDir)
	if err != nil {
		// Report what is known rather than failing the whole view. Zero here is
		// distinguishable from a real reading because fs_total is zero too, which no
		// mounted filesystem reports.
		return detail, nil
	}
	detail.FSFree, detail.FSTotal = free, total
	return detail, nil
}

// parseWindow reads RFC 3339 bounds, defaulting to the recent past.
func parseWindow(fromText, toText string) (from, to time.Time, err error) {
	to = time.Now()
	if toText != "" {
		if to, err = time.Parse(time.RFC3339, toText); err != nil {
			return time.Time{}, time.Time{}, control.NewError(
				http.StatusBadRequest, "invalid_time", "to: expected RFC 3339")
		}
	}
	from = to.Add(-defaultWindow)
	if fromText != "" {
		if from, err = time.Parse(time.RFC3339, fromText); err != nil {
			return time.Time{}, time.Time{}, control.NewError(
				http.StatusBadRequest, "invalid_time", "from: expected RFC 3339")
		}
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, control.NewError(
			http.StatusBadRequest, "invalid_range", "from must precede to")
	}
	return from, to, nil
}

// resolveSpan picks the finest resolution that still exists for the requested window.
//
// Asking for five-minute buckets across a month is not an error; those buckets were
// folded into hours days ago, exactly as designed. Silently answering at the coarser
// resolution — and saying which one in the response — is more useful than either an
// error or an empty array.
func resolveSpan(requested string, from time.Time) (int64, error) {
	age := time.Since(from)
	available := catalog.SpanFine
	switch {
	case age > catalog.RetainHour:
		available = catalog.SpanDay
	case age > catalog.RetainFine:
		available = catalog.SpanHour
	}
	if requested == "" || requested == "auto" {
		return available, nil
	}
	var asked int64
	switch strings.TrimSpace(requested) {
	case "5m", "300":
		asked = catalog.SpanFine
	case "1h", "3600":
		asked = catalog.SpanHour
	case "1d", "86400":
		asked = catalog.SpanDay
	default:
		return 0, control.NewError(http.StatusBadRequest, "invalid_span",
			"span must be 5m, 1h, 1d or auto")
	}
	if asked < available {
		return available, nil
	}
	return asked, nil
}
