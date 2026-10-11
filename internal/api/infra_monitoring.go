package api

import (
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/metrics"
	"github.com/supavise/supavise/internal/replicas"
)

// Infrastructure metrics: the series behind Studio's Database report, the Observability overview,
// Compute and Disk page and the replica details. The node's collector (internal/metrics) samples
// each project once a minute from its control groups, one SQL session and its disk quota, and
// keeps the samples on disk; the replication lag of a read replica comes from the replica
// controller's ring (internal/replicas). Which attribute means what, and which have no source,
// is the collector's catalog (metrics.Attributes).
//
// The response has two shapes, as hosted's has: a request for one attribute is answered with its
// series metadata on the response and the value on each point, a request for several with
// values by attribute, one series entry each, and an errors entry for each attribute with no
// source. Studio draws nothing for an attribute whose series entry is missing, so the response has
// one for every name that was asked for, with zero for the points of an attribute Supavise cannot
// measure (see metrics.Assemble).

// MetricsSource is the collector the infra-monitoring and resource-warning routes read:
// *metrics.Service. Nil: the routes answer with series that have no points.
type MetricsSource interface {
	Query(ref string, attrs []string, from, to time.Time, step time.Duration) (metrics.Result, error)
	Warnings(ref string) (metrics.Warnings, bool)
}

func (s *Server) routesInfraMonitoring(add func(string, handlerFunc)) {
	add("GET /platform/projects/{ref}/infra-monitoring", s.infraMonitoring)
	add("GET /platform/projects-resource-warnings", s.projectsResourceWarnings)
}

// lagAttribute is the replication lag in seconds, Studio's name for it.
const lagAttribute = metrics.LagAttribute

// infraIntervals are the buckets of the query's interval parameter.
var infraIntervals = map[string]time.Duration{
	"1m": time.Minute, "5m": 5 * time.Minute, "10m": 10 * time.Minute, "30m": 30 * time.Minute, "1h": time.Hour, "1d": 24 * time.Hour,
}

// infraAttributes are the attributes a request names: attributes (repeated, with or without the
// brackets, or comma-separated) and the older single attribute.
func infraAttributes(r *http.Request) []string {
	var out []string
	q := r.URL.Query()
	for _, v := range slices.Concat(q["attributes"], q["attributes[]"], q["attribute"]) {
		for _, a := range strings.Split(v, ",") {
			if a = strings.TrimSpace(a); a != "" && !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}

func (s *Server) infraMonitoring(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	attrs := infraAttributes(r)
	if len(attrs) == 0 {
		return errf(http.StatusBadRequest, "attributes is required: name at least one metric")
	}
	q := r.URL.Query()
	start, err1 := time.Parse(time.RFC3339, q.Get("startDate"))
	end, err2 := time.Parse(time.RFC3339, q.Get("endDate"))
	if err1 != nil || err2 != nil || end.Before(start) {
		return errf(http.StatusBadRequest, "startDate and endDate must be RFC 3339 timestamps, the start not after the end")
	}
	step := time.Hour
	if v := q.Get("interval"); v != "" {
		if step = infraIntervals[v]; step == 0 {
			return errf(http.StatusBadRequest, "interval must be one of 1m, 5m, 10m, 30m, 1h or 1d")
		}
	}

	var res metrics.Result
	if id := q.Get("databaseIdentifier"); id != "" && id != p.Ref {
		// A read replica: its lag is the one metric Supavise has for it.
		_, ok, err := s.replicaOf(r.Context(), p.Ref, id)
		if err != nil {
			return err
		}
		if !ok || s.replicas == nil {
			return errf(http.StatusNotFound, "Database not found")
		}
		samples, err := s.replicas.Lag(r.Context(), id, start)
		if err != nil {
			return replicaServiceErr(err)
		}
		res = metrics.Assemble(attrs, lagRows(samples, start, end, step), metrics.NotCollectedForReplicas)
	} else if s.metrics != nil {
		if res, err = s.metrics.Query(p.Ref, attrs, start, end, step); err != nil {
			return mapErr(err)
		}
	} else {
		res = metrics.Assemble(attrs, nil, "")
	}
	writeJSON(w, http.StatusOK, infraResponse(attrs, res))
	return nil
}

// lagRows buckets the replica's lag samples: the mean of each interval, as the controller's ring
// holds one point a minute. A negative lag means it was not known and is skipped.
func lagRows(samples []replicas.LagPoint, start, end time.Time, step time.Duration) []metrics.Row {
	type acc struct {
		sum float64
		n   int
	}
	sums := map[time.Time]*acc{}
	for _, sm := range samples {
		if sm.At.Before(start) || sm.At.After(end) || sm.Seconds < 0 {
			continue
		}
		b := sm.At.UTC().Truncate(step)
		if sums[b] == nil {
			sums[b] = &acc{}
		}
		sums[b].sum += sm.Seconds
		sums[b].n++
	}
	rows := make([]metrics.Row, 0, len(sums))
	for b, a := range sums {
		rows = append(rows, metrics.Row{At: b, Values: map[string]float64{lagAttribute: roundMilli(a.sum / float64(a.n))}})
	}
	slices.SortFunc(rows, func(a, b metrics.Row) int { return a.At.Compare(b.At) })
	return rows
}

// infraResponse is the JSON of a result in the shape that matches the number of attributes asked.
func infraResponse(attrs []string, res metrics.Result) map[string]any {
	data := make([]any, 0, len(res.Rows))
	meta := func(m metrics.Meta) map[string]any {
		return map[string]any{"yAxisLimit": m.YAxisLimit, "format": m.Format, "total": m.Total, "totalAverage": m.TotalAverage}
	}
	if len(attrs) == 1 {
		// One attribute: the value sits on the point, with the series' metadata on the response.
		a := attrs[0]
		for _, row := range res.Rows {
			pt := map[string]any{"period_start": ts(row.At)}
			if v, ok := row.Values[a]; ok {
				pt[a] = metrics.FormatValue(a, v)
			}
			data = append(data, pt)
		}
		out := meta(res.Series[a])
		out["data"] = data
		return out
	}
	// Several attributes: values by attribute, one series entry each.
	series := make(map[string]any, len(attrs))
	for _, a := range attrs {
		series[a] = meta(res.Series[a])
	}
	for _, row := range res.Rows {
		vals := make(map[string]any, len(attrs))
		for _, a := range attrs {
			if v, ok := row.Values[a]; ok {
				vals[a] = metrics.FormatValue(a, v)
			}
		}
		data = append(data, map[string]any{"period_start": ts(row.At), "values": vals})
	}
	out := map[string]any{"data": data, "series": series}
	if len(res.Errors) > 0 {
		errs := make(map[string]any, len(res.Errors))
		for a, msg := range res.Errors {
			errs[a] = map[string]any{"message": msg}
		}
		out["errors"] = errs
	}
	return out
}

func roundMilli(v float64) float64 { return math.Round(v*1000) / 1000 }
