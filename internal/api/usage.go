package api

import (
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// Organization usage. Studio's Usage page (Organization > Usage, and Project Settings > Billing > Usage)
// is open to every owner, and every project page asks for the same numbers in the background (the
// header's "Exceeding usage limits" badge, the Plan usage card of the project list). A node has no
// plan with quotas and nobody bills for it, so the page can only say what the node measures, as
// unlimited, and leave out what it does not.
//
// The node measures one thing the page lists: the size of each project's databases (the collector's
// pg_database_size, daily means). The answer has an entry for DATABASE_SIZE and none for egress,
// Storage size, active users, Edge Function invocations, Realtime or logs: the page shows a section
// for such a metric without a figure, and a figure of zero would say that the node counted nothing
// when it did not count. The request counters of internal/reqstats start again at every restart, so
// they cannot stand for a billing period either.
//
// The period is the calendar month in UTC (billingPeriod), which is what the subscription answer
// reports, so the page reads "01 Oct 2026 - 01 Nov 2026" and not the epoch.

const (
	// usageGiB is the unit of the figures Studio prints as GB. The daily charts format bytes in powers
	// of 1024, and the summary uses the same unit, so the two agree.
	usageGiB = float64(1 << 30)
	// usageMetricDatabaseSize is the metric the node reports, as named by the hosted API.
	usageMetricDatabaseSize = "DATABASE_SIZE"
	// maxUsageRange bounds the range of a request: the collector keeps 92 days of hourly means, and a
	// request for more only makes the loop longer.
	maxUsageRange = 400 * 24 * time.Hour
)

// billingPeriod is the calendar month (UTC) that contains now, as [start, end): a node has no billing
// cycle, and Studio prints the period it reads from the subscription and uses it as the default range
// of the Usage page. end is the first instant of the next month, as a billing period's end is on
// hosted.
func billingPeriod(now time.Time) (start, end time.Time) {
	y, m, _ := now.UTC().Date()
	start = time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// usageRange reads start and end from the query (RFC 3339, as Studio sends them) and defaults them to
// the current billing period.
func (s *Server) usageRange(r *http.Request) (start, end time.Time, err error) {
	q := r.URL.Query()
	start, end = billingPeriod(s.now())
	if v := q.Get("start"); v != "" {
		if start, err = time.Parse(time.RFC3339, v); err != nil {
			return start, end, errf(http.StatusBadRequest, "start must be an RFC 3339 timestamp")
		}
	}
	if v := q.Get("end"); v != "" {
		if end, err = time.Parse(time.RFC3339, v); err != nil {
			return start, end, errf(http.StatusBadRequest, "end must be an RFC 3339 timestamp")
		}
	}
	if end.Before(start) {
		return start, end, errf(http.StatusBadRequest, "start must not be after end")
	}
	if end.Sub(start) > maxUsageRange {
		return start, end, errf(http.StatusBadRequest, "the range must not be longer than 400 days")
	}
	return start.UTC(), end.UTC(), nil
}

// usageProjects returns the projects whose usage the request asks for: every database of the
// organization (a branch is a database on the same disk), or the one that project_ref names, which
// must belong to the organization.
func (s *Server) usageProjects(r *http.Request) ([]registry.Project, error) {
	ctx := r.Context()
	org, err := s.orgBySlug(ctx, r.PathValue("slug"))
	if err != nil {
		return nil, err
	}
	all, err := s.reg.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	want := r.URL.Query().Get("project_ref")
	var out []registry.Project
	for _, p := range all {
		if p.Ref == config.SystemRef {
			continue
		}
		o, err := s.orgOf(ctx, &p)
		if err != nil {
			return nil, err
		}
		if o.ID != org.ID || (want != "" && p.Ref != want) {
			continue
		}
		out = append(out, p)
	}
	if want != "" && len(out) == 0 {
		return nil, errNoProject
	}
	slices.SortFunc(out, func(a, b registry.Project) int { return strings.Compare(a.Ref, b.Ref) })
	return out, nil
}

// sizeDays returns the mean database size in bytes of each UTC day of [start, end) that the collector
// has a sample for, by day.
func (s *Server) sizeDays(ref string, start, end time.Time) (map[time.Time]float64, error) {
	days := map[time.Time]float64{}
	if s.metrics == nil {
		return days, nil
	}
	res, err := s.metrics.Query(ref, []string{"pg_database_size"}, start, end, 24*time.Hour)
	if err != nil {
		return nil, mapErr(err)
	}
	for _, row := range res.Rows {
		if v, ok := row.Values["pg_database_size"]; ok && row.At.Before(end) {
			days[row.At.UTC()] = v
		}
	}
	return days, nil
}

func usageGB(bytes float64) float64 { return math.Round(bytes/usageGiB*1e6) / 1e6 }

// orgUsage answers GET /platform/organizations/{slug}/usage[?project_ref=&start=&end=].
func (s *Server) orgUsage(w http.ResponseWriter, r *http.Request) error {
	projects, err := s.usageProjects(r)
	if err != nil {
		return err
	}
	start, end, err := s.usageRange(r)
	if err != nil {
		return err
	}
	var total float64
	allocations := []map[string]any{}
	for _, p := range projects {
		days, err := s.sizeDays(p.Ref, start, end)
		if err != nil {
			return err
		}
		if len(days) == 0 {
			continue
		}
		var sum float64
		for _, v := range days {
			sum += v
		}
		mean := sum / float64(len(days))
		total += mean
		allocations = append(allocations, map[string]any{"ref": p.Ref, "name": p.Name, "usage": usageGB(mean)})
	}
	usages := []map[string]any{}
	if len(allocations) > 0 {
		usages = append(usages, map[string]any{
			"metric": usageMetricDatabaseSize, "usage": usageGB(total), "usage_original": math.Round(total),
			"cost": 0, "available_in_plan": true, "capped": false, "unlimited": true,
			"pricing_strategy": "NONE", "unit_price_desc": "", "project_allocations": allocations,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"usage_billing_enabled": false, "usages": usages})
	return nil
}

// orgUsageDaily answers GET /platform/organizations/{slug}/usage/daily[?project_ref=&start=&end=]: one
// point a day for each metric the node measures, summed over the projects asked for.
func (s *Server) orgUsageDaily(w http.ResponseWriter, r *http.Request) error {
	projects, err := s.usageProjects(r)
	if err != nil {
		return err
	}
	start, end, err := s.usageRange(r)
	if err != nil {
		return err
	}
	byDay := map[time.Time]float64{}
	for _, p := range projects {
		days, err := s.sizeDays(p.Ref, start, end)
		if err != nil {
			return err
		}
		for d, v := range days {
			byDay[d] += v
		}
	}
	keys := make([]time.Time, 0, len(byDay))
	for d := range byDay {
		keys = append(keys, d)
	}
	slices.SortFunc(keys, func(a, b time.Time) int { return a.Compare(b) })
	usages := make([]map[string]any, 0, len(keys))
	for _, d := range keys {
		usages = append(usages, map[string]any{
			"date": d.Format("2006-01-02"), "metric": usageMetricDatabaseSize,
			"usage": usageGB(byDay[d]), "usage_original": math.Round(byDay[d]), "breakdown": nil,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"usages": usages})
	return nil
}
