package api

import (
	"context"
	"net/http"
	"time"

	plat "github.com/supavise/supavise/internal/api/gen/platform"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/reqstats"
)

// Analytics endpoints. On hosted Supabase they answer from Logflare and ClickHouse. A node has
// no log store: it keeps a journal per service unit (read with `supavise logs`), not a table the
// dashboard can query. The endpoints that need logs say so in the body's "error" field instead of
// answering an empty "result", which Studio, the MCP server and the CLI read as "no log lines" and
// "no errors". The two usage endpoints that a request count can answer are answered from
// internal/reqstats.
//
// The "error" of a 200 answer is the spec's string-or-object union. Studio renders a string in the
// logs explorer, on the per-product log pages, in the API report and in the Auth overview
// (docs/reference/studio-platform-calls.md, "Logs and analytics").

func (s *Server) routesAnalytics(add func(string, handlerFunc)) {
	const p = "/platform/projects/{ref}/analytics/endpoints/"
	// Log queries: the explorer, the per-product log pages, the reports and the MCP server's
	// get_logs and query_logs.
	for _, name := range []string{"logs.all", "logs.all.otel", "logs"} {
		add("GET "+p+name, s.logsUnavailable)
		add("POST "+p+name, s.logsUnavailable)
	}
	// Endpoints whose rows Hosted derives from logs (function invocations, the last use of an API
	// key, service health). auth.metrics is a refusal (unsupported.go): Studio's Auth overview shows
	// zero sign-ups for an empty answer.
	for _, name := range []string{
		"functions.combined-stats", "functions.req-stats", "functions.resource-usage",
		"api_keys.last_used.otel", "project.metrics", "project.metrics.otel", "service-health",
	} {
		add("GET "+p+name, s.logsUnavailable)
	}
	add("GET /v1/projects/{ref}/analytics/endpoints/logs", s.logsUnavailable)
	add("GET /v1/projects/{ref}/analytics/endpoints/functions.combined-stats", s.logsUnavailable)
	// Hosted removed logs.all from the Management API on 2026-09-23 (its spec lists only 410).
	add("GET /v1/projects/{ref}/analytics/endpoints/logs.all", func(http.ResponseWriter, *http.Request) error {
		return errf(http.StatusGone, "This endpoint has been removed")
	})

	for _, base := range []string{p, "/v1/projects/{ref}/analytics/endpoints/"} {
		add("GET "+base+"usage.api-counts", s.usageAPICounts)
		add("GET "+base+"usage.api-requests-count", s.usageAPIRequestsCount)
	}
}

// logsUnavailable is the answer of an endpoint that needs logs. The text names the command that
// does read them.
func (s *Server) logsUnavailable(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var out plat.AnalyticsResponseOutput
	var e plat.AnalyticsResponseOutput_Error
	if err := e.FromAnalyticsResponseOutputError0(logsUnavailableMessage(p.Ref, s.logsHost(r.Context(), p))); err != nil {
		return err
	}
	result := []interface{}{}
	out.Result, out.Error = &result, &e
	writeJSON(w, http.StatusOK, out)
	return nil
}

func logsUnavailableMessage(ref, host string) string {
	return "Supavise does not collect logs for the dashboard, so there is nothing to query here. " +
		host + ", run `supavise logs " + ref + "` to read this project's service logs."
}

// logsHost says where `supavise logs` finds a project's journal: on the node that hosts the project,
// because a node's journal holds the units that run there. With one server that is "the server"; on
// a cluster the text names the project's node so that an operator does not run the command on the
// leader and read "no log lines" for a project that lives on a follower.
func (s *Server) logsHost(ctx context.Context, p *registry.Project) string {
	nodes, err := s.reg.ListNodes(ctx)
	if err != nil {
		return "On the server that hosts this project"
	}
	active := 0
	name := ""
	for _, n := range nodes {
		if n.State != registry.NodeLeft {
			active++
		}
		if n.ID == p.NodeID {
			name = n.Name
		}
	}
	switch {
	case active < 2:
		return "On the server"
	case name != "":
		return "On the server that hosts this project (node " + name + ")"
	}
	return "On the server that hosts this project"
}

// intervals are the values of the "interval" parameter of usage.api-counts (both specs) with the
// width of the buckets and how far back they reach. The dashboard asks for 1hr, 1day and 7day: a
// chart of the last hour by minute, of the last day by hour and of the last week by day.
var intervals = map[string]struct {
	back time.Duration
	step reqstats.Step
}{
	"15min": {15 * time.Minute, reqstats.Minute},
	"30min": {30 * time.Minute, reqstats.Minute},
	"1hr":   {time.Hour, reqstats.Minute},
	"3hr":   {3 * time.Hour, reqstats.Minute},
	"1day":  {24 * time.Hour, reqstats.Hour},
	"3day":  {3 * 24 * time.Hour, reqstats.Hour},
	"7day":  {7 * 24 * time.Hour, reqstats.Day},
}

// usageAPICounts answers the project home's request chart: one row per bucket of the interval,
// oldest first, with the requests the edge proxy served on this node to the project's REST, Auth,
// Storage and Realtime APIs. The rows cover every bucket, empty ones included, as the dashboard
// draws them. A bucket is whole, so the first one counts from the start of its minute, hour or UTC
// day. The counts start at zero when the daemon starts and cover this server's proxy only.
func (s *Server) usageAPICounts(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	if s.requests == nil {
		return s.requestsUnavailable(w)
	}
	iv, ok := intervals[r.URL.Query().Get("interval")]
	if !ok {
		iv = intervals["1day"]
	}
	now := s.now()
	series := s.requests.Series(p.Ref, now.Add(-iv.back), now, iv.step)
	rows := make([]map[string]any, 0, len(series))
	for _, b := range series {
		rows = append(rows, map[string]any{
			"timestamp":               b.Start.Format(time.RFC3339),
			"total_auth_requests":     b.Counts.Of(reqstats.Auth),
			"total_realtime_requests": b.Counts.Of(reqstats.Realtime),
			"total_rest_requests":     b.Counts.Of(reqstats.Rest),
			"total_storage_requests":  b.Counts.Of(reqstats.Storage),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": rows}) // the shape of v1.V1GetUsageApiCountResponseOutput
	return nil
}

// usageAPIRequestsCount answers the total number of requests to the project's API over the last
// 24 hours: the current UTC hour and the 23 before it, every service the proxy counts (REST,
// GraphQL, Auth, Storage, Realtime and Functions). Hosted does not document the window; Studio
// does not call this endpoint.
func (s *Server) usageAPIRequestsCount(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	if s.requests == nil {
		return s.requestsUnavailable(w)
	}
	now := s.now()
	var total uint64
	for _, b := range s.requests.Series(p.Ref, now.Add(-23*time.Hour), now, reqstats.Hour) {
		total += b.Counts.Total()
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": []map[string]any{{"count": total}}}) // v1.V1GetUsageApiRequestsCountResponseOutput
	return nil
}

// requestsUnavailable answers a usage endpoint on a server whose process has no request counter
// (a Management API run without the edge proxy).
func (s *Server) requestsUnavailable(w http.ResponseWriter) error {
	var out plat.AnalyticsResponseOutput
	var e plat.AnalyticsResponseOutput_Error
	if err := e.FromAnalyticsResponseOutputError0("This server does not count requests."); err != nil {
		return err
	}
	result := []interface{}{}
	out.Result, out.Error = &result, &e
	writeJSON(w, http.StatusOK, out)
	return nil
}
