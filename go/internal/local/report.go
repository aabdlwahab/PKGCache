package local

import (
	"context"
	"net/http"
	"sort"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

// What a cache is holding, per project.
//
// A project on a laptop is an upstream chain and an accounting boundary; this is the
// second half made visible. It is one request rather than one per project, because the
// catalog is single and answers the whole question at once — which is the argument the
// architecture makes for a single catalog, so it would be strange for the client to
// take the fan-out anyway.

// ProjectReport is what `status` says about one project.
type ProjectReport struct {
	Project string
	// Objects and Bytes are what this project's entries resolve to. Bytes is content
	// attributed to the project, not disk consumed by it: two projects sharing a wheel
	// are each shown its size, and the disk holds one copy.
	Objects int64
	Bytes   int64
	Hits    int64
	Misses  int64
}

// Served is the share of requests this project answered without going upstream.
//
// hit, dedup and peer are all "did not leave this machine for it"; the catalog folds them
// into HitCount already. The second return is false when nothing has been asked for yet,
// which is a different thing from a rate of zero.
func (r ProjectReport) Served() (float64, bool) {
	total := r.Hits + r.Misses
	if total == 0 {
		return 0, false
	}
	return float64(r.Hits) / float64(total), true
}

// ProjectReports returns every project this cache holds something for, global first and
// the rest by name.
func ProjectReports(ctx context.Context, state State) ([]ProjectReport, error) {
	// No project parameter: the response then carries a row per (project, eco), which is
	// the whole table in one round trip.
	var body struct {
		ByEco []struct {
			Project   string `json:"project"`
			Count     int64  `json:"count"`
			Size      int64  `json:"size"`
			HitCount  int64  `json:"hit_count"`
			MissCount int64  `json:"miss_count"`
		} `json:"by_eco"`
	}
	if err := newProjectAPI(state).do(
		ctx, http.MethodGet, "/api/v1/stats", nil, &body); err != nil {
		return nil, err
	}
	byProject := map[string]*ProjectReport{}
	for _, row := range body.ByEco {
		report, found := byProject[row.Project]
		if !found {
			report = &ProjectReport{Project: row.Project}
			byProject[row.Project] = report
		}
		report.Objects += row.Count
		report.Bytes += row.Size
		report.Hits += row.HitCount
		report.Misses += row.MissCount
	}
	reports := make([]ProjectReport, 0, len(byProject))
	for _, report := range byProject {
		reports = append(reports, *report)
	}
	sort.Slice(reports, func(i, j int) bool {
		if (reports[i].Project == config.GlobalProject) != (reports[j].Project == config.GlobalProject) {
			return reports[i].Project == config.GlobalProject
		}
		return reports[i].Project < reports[j].Project
	})
	return reports, nil
}
