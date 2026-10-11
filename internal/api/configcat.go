package api

import "net/http"

// The ConfigCat flag document. Studio reads its feature flags from ConfigCat: the browser's configcat-js
// client polls <NEXT_PUBLIC_CONFIGCAT_PROXY_URL>/configuration-files/configcat-proxy/frontend-v2/config_v6.json
// every seven minutes (packages/common/configcat.ts), and without a URL Studio skips the whole lookup and
// reads every flag as false. A node serves the document itself, from the Management API's origin, which is
// where the dashboard already connects (studioEnv sets the variable to that origin, so the CSP's connect-src
// needs no entry of its own).
//
// Only the dashboard's own behavior depends on it. Studio treats two things alike: a flag that is false and a
// flag that no document lists, except that once the store holds any flag, useFlag logs
// `Flag key "x" does not exist in ConfigCat flag store` (a console.error) for every key it reads and the
// document lacks. So the document lists every key Studio reads, false unless a node needs the opposite:
//
//   - allowDataBranching: true. The Create branch dialog shows the Include data switch only with it
//     (CreateBranchModal.tsx). Hosted turns it on per organization.
//   - supaviseBranchClones: true while the node serves branches. Patch 0005 enables the switch with it, in
//     place of the point-in-time-recovery add-on that hosted requires: a node clones the parent's database
//     itself (copy-on-write, or a restore of the latest base backup), and a node that can do neither refuses
//     the create with the reason, which the dialog shows.
//   - warehouse: "none", because Studio reads that one with a string default, and the SDK refuses a bool
//     there.
//
// The list of keys is the useFlag calls of the pinned Studio (internal/versions/versions.yaml studio.tag):
//
//	git grep -h -o -E "useFlag(<[^>]*>)?\(\s*['\"][A-Za-z0-9_]+['\"]" <tag> -- apps packages
//
// plus the keys that sit behind a constant (pgMetaScopedIntrospection, McpElicitURLMode, projectNeedsSecuring,
// projectCreationRestrictedRegions, the etlEnable*PrivateAlpha group). A Studio bump that reads a new key makes
// the Studio crawl fail on the console.error until the key is added here (docs/development.md, "Studio
// crawl").

// configCatPath is the file the SDK fetches for the SDK key Studio uses with a proxy URL.
const configCatPath = "/configuration-files/configcat-proxy/frontend-v2/config_v6.json"

// publicRoutes are the extra routes that need no credentials: the dashboard reads them before and without
// signing in. They are served as written in implemented().
var publicRoutes = []string{"GET " + configCatPath}

// configCatKeys are the flags Studio reads at the pin, all false. Keep the list sorted.
var configCatKeys = []string{
	"ConfigDrift", "McpElicitURLMode", "adminStudioLink", "aikidoDashboardIntegrationEnabled",
	"analyticsBucketsTableCreation", "auditLogsLogDrain", "authOverviewPage", "clockSkewBanner", "compute",
	"contributePage", "customDomainsDisabledDueToQuota", "dashboardPreferences", "databaseLogsConfiguration",
	"defaultRegionRestrictedPool", "devToolbarDefaultOn", "disableAssistantPrompts", "disableLegacyJwtSecretRotation",
	"disableOrioleProjectCreation", "disableProjectCreationAndUpdate", "disableProjectRestarts", "disableProjectTransfer",
	"disableProjectUpgrade", "dopplerDashboardIntegrationEnabled", "edgeFunctionsOverview", "edgeFunctionsRequestMetrics",
	"enableAccountPassword", "enableAuthRecoveryCodes", "enablePasskeyAuth", "etlEnableBigQueryPrivateAlpha",
	"etlEnableClickHousePrivateAlpha", "etlEnableDucklakePrivateAlpha", "etlEnableIcebergPrivateAlpha",
	"etlEnableSnowflakePrivateAlpha", "explorer", "grafanaDashboardIntegrationEnabled", "healthAdvisor",
	"healthAdvisorInHomepage", "improvedUserSearch", "incidentIoStatusPage", "jitDbAccess", "marketplaceIntegrations",
	"newHomepageUsageDeltas", "newProjectInternalOnlyConfiguration", "observabilityOverview", "ongoingIncident",
	"otelLegacyLogs", "otelReports", "otelUnifiedLogs", "pgMetaScopedIntrospection", "platformWebhooks",
	"previewMarketplaceListingsEnabled", "privateApps", "privatelinkReadReplica", "projectCreationRestrictedRegions",
	"projectNeedsSecuring", "resendDashboardIntegrationEnabled", "scopedPAT", "showApiKeysLastUsed",
	"showDiskIOBurstBalanceChart", "showMemoryCommitmentChart", "showMultigresLogs", "showNoticeBanner",
	"showPostgresUpgradeLogs", "simplifiedSupportForm", "sqlEditorLogsSource", "sqlEditorManualSaveForced",
	"storageVersioningPrivateAlpha", "supportAssistantFollowUp", "syslogLogDrain", "textConfirmationModalClickToCopy",
}

// ConfigCat setting types of config_v6.json (configcat-common ConfigJson.ts, SettingType).
const (
	ccBool   = 0
	ccString = 1
)

// ccSetting is one entry of the document's "f" object: t is the setting type and v holds its value under
// the key of that type ("b" for a bool, "s" for a string).
type ccSetting struct {
	T int            `json:"t"`
	V map[string]any `json:"v"`
}

// configCatDocument builds the config_v6.json body. branches says whether the node serves branches
// (supaviseBranchClones). The document has no "p" (preferences) member: the SDK then never redirects, and the
// segment and targeting members are for rules this document does not have.
func configCatDocument(branches bool) map[string]any {
	flags := map[string]ccSetting{}
	for _, k := range configCatKeys {
		flags[k] = ccSetting{T: ccBool, V: map[string]any{"b": false}}
	}
	flags["allowDataBranching"] = ccSetting{T: ccBool, V: map[string]any{"b": true}}
	flags["supaviseBranchClones"] = ccSetting{T: ccBool, V: map[string]any{"b": branches}}
	flags["warehouse"] = ccSetting{T: ccString, V: map[string]any{"s": "none"}}
	return map[string]any{"f": flags}
}

// routesConfigCat serves the flag document. It needs no credentials (publicRoutes); the dashboard's origin
// gets the CORS headers of every API answer, and the preflight of a conditional request is answered by the
// middleware.
func (s *Server) routesConfigCat(add func(string, handlerFunc)) {
	add("GET "+configCatPath, s.configCatConfig)
}

// configCatConfig answers GET /configuration-files/configcat-proxy/frontend-v2/config_v6.json. It sends no
// ETag, so the SDK fetches the whole document at each poll (a few kilobytes every seven minutes).
func (s *Server) configCatConfig(w http.ResponseWriter, _ *http.Request) error {
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, configCatDocument(s.branches != nil))
	return nil
}
