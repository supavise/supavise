package advisors

// Health lints are asked for by name through POST /v2/projects/{ref}/advisors/run. Hosted
// answers a check it cannot run with advisor_check_unavailable and a project that is not running
// with project_not_active; both are "result only" names that Studio and the MCP server drop
// from what they show (data/lint/health-lints-query.ts, mcp-server-supabase's api-platform.ts),
// so this node reports what it cannot check without raising a false alarm.

// healthChecks are the health names of the Management API specs and why Supavise cannot run them.
// They need a store of the project's service logs (the log_* rates), a metrics and alerting
// pipeline per project (instance_*, db_connection_limit_reached) or probes of hosted's own
// infrastructure (db_connection_failing, db_not_reachable).
var healthChecks = map[string]string{
	"db_not_reachable":                  "probe_unavailable",
	"db_connection_failing":             "probe_unavailable",
	"db_connection_limit_reached":       "metrics_unavailable",
	"instance_telemetry_lost":           "metrics_unavailable",
	"instance_db_down":                  "metrics_unavailable",
	"instance_alert_firing":             "metrics_unavailable",
	"log_data_api_error_rate_high":      "logs_unavailable",
	"log_auth_error_rate_high":          "logs_unavailable",
	"log_storage_error_rate_high":       "logs_unavailable",
	"log_edge_function_error_rate_high": "logs_unavailable",
}

// IsHealthCheck reports whether name is a health lint that Supavise answers with
// advisor_check_unavailable.
func IsHealthCheck(name string) bool {
	_, ok := healthChecks[name]
	return ok
}

// Unavailable is the answer for the health check name when this node cannot run it.
func Unavailable(name string) Lint {
	reason := healthChecks[name]
	if reason == "" {
		reason = "check_unavailable"
	}
	return build(fields{
		Name:        "advisor_check_unavailable",
		Title:       "Advisor check unavailable",
		Level:       LevelInfo,
		Categories:  []string{CategoryHealth},
		Description: "This advisor check could not be run for the project.",
		Detail:      "The " + name + " check needs data this Supavise node does not collect, so it was not run.",
		CacheKey:    "advisor_check_unavailable:" + reason,
		Remediation: DocsURL + "/guides/platform/troubleshooting",
		Metadata:    map[string]any{"type": "health", "entity": "Advisor"},
	})
}

// NotActive is the answer for every requested check when the project is not running.
func NotActive() Lint {
	return build(fields{
		Name:        "project_not_active",
		Title:       "Project is not active",
		Level:       LevelInfo,
		Categories:  []string{CategoryHealth},
		Description: "The project is not running, so its advisor checks were not run.",
		Detail:      "Restore or restart the project, then run the checks again.",
		CacheKey:    "project_not_active",
		Remediation: DocsURL + "/guides/platform/troubleshooting",
		Metadata:    map[string]any{"type": "health", "entity": "Project"},
	})
}
