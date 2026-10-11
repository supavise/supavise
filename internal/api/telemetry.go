package api

import "net/http"

func (s *Server) routesTelemetry(add func(string, handlerFunc)) {
	add("GET /platform/telemetry/feature-flags", s.telemetryFeatureFlags)
}

// featureFlags are the PostHog flags this node answers with. Studio asks for them on every page
// (packages/common/feature-flags.tsx) and reads the few it knows through usePHFlag, which is
// undefined for a flag the answer lacks. Hosted runs no flag on the node's behalf, so the answer
// holds only the flags whose value a node must fix to behave like hosted today:
//
//   - dataApiRevokeOnCreateDefault: true. The create-project form (ProjectCreationForm.tsx,
//     SecurityOptions.tsx) starts its "Automatically expose new tables" box unchecked for a user in
//     the treatment arm of this flag and checked otherwise, and hosted has put every user in the
//     treatment arm since 2026-05-30. Without the flag the box would start checked and a project
//     created from the dashboard would keep the grants hosted no longer gives (data_api_revoke_default_privileges).
//     The same default applies to a body that omits the field (dataAPIOptions).
var featureFlags = map[string]any{
	"dataApiRevokeOnCreateDefault": true,
}

// telemetryFeatureFlags answers GET /platform/telemetry/feature-flags. The organization and project
// the query names do not change the flags.
func (s *Server) telemetryFeatureFlags(w http.ResponseWriter, _ *http.Request) error {
	writeJSON(w, http.StatusOK, featureFlags)
	return nil
}
