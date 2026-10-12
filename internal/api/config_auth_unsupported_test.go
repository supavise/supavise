package api

import (
	"strings"
	"testing"
)

// Auth > Audit Logs > "Disable Postgres writes" used to save, toast, refetch and show the old value: the
// setting sat in the set of names Studio's twin lists and the node did not store. It is a setting of the
// schema now, rendered as GOTRUE_AUDIT_LOG_DISABLE_POSTGRES.
func TestAuditLogDisablePostgresIsStoredAndRendered(t *testing.T) {
	f := newFixture(t)
	if got := bodyMap(t, f.mustDo("GET", authPlatform, nil, 200)); got["AUDIT_LOG_DISABLE_POSTGRES"] != false {
		t.Fatalf("default %v, want false", got["AUDIT_LOG_DISABLE_POSTGRES"])
	}
	got := bodyMap(t, f.mustDo("PATCH", authPlatform, map[string]any{"AUDIT_LOG_DISABLE_POSTGRES": true}, 200))
	if got["AUDIT_LOG_DISABLE_POSTGRES"] != true {
		t.Fatalf("the answer to the save says %v", got["AUDIT_LOG_DISABLE_POSTGRES"])
	}
	// A reload: the switch keeps its position.
	if got := bodyMap(t, f.mustDo("GET", authPlatform, nil, 200)); got["AUDIT_LOG_DISABLE_POSTGRES"] != true {
		t.Errorf("after a reload the switch says %v", got["AUDIT_LOG_DISABLE_POSTGRES"])
	}
	env, err := f.srv.settings.AuthEnv(t.Context(), testRef, "")
	if err != nil {
		t.Fatal(err)
	}
	if env["GOTRUE_AUDIT_LOG_DISABLE_POSTGRES"] != "true" {
		t.Errorf("GoTrue's environment: %v", env)
	}
	f.mustDo("PATCH", authPlatform, map[string]any{"AUDIT_LOG_DISABLE_POSTGRES": false}, 200)
	if env, _ = f.srv.settings.AuthEnv(t.Context(), testRef, ""); env["GOTRUE_AUDIT_LOG_DISABLE_POSTGRES"] == "true" {
		t.Errorf("switched off, GoTrue still gets %q", env["GOTRUE_AUDIT_LOG_DISABLE_POSTGRES"])
	}
}

// A setting the node cannot honor is refused with the reason, and nothing of the same save is applied. A value
// equal to what the GET reports passes, so a form that sends the whole object still saves.
func TestAuthPatchRefusesASettingThatWouldChangeAndCannotBeHonored(t *testing.T) {
	f := newFixture(t)
	rec := f.do("PATCH", authPlatform, map[string]any{"MFA_ALLOW_LOW_AAL": true, "MFA_MAX_ENROLLED_FACTORS": 4})
	if rec.Code != 400 {
		t.Fatalf("MFA_ALLOW_LOW_AAL to true: %d %s", rec.Code, rec.Body)
	}
	msg := bodyMap(t, rec)["message"].(string)
	for _, want := range []string{"MFA_ALLOW_LOW_AAL", "GOTRUE_SESSIONS_ALLOW_LOW_AAL", "nothing was saved"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if got := bodyMap(t, f.mustDo("GET", authPlatform, nil, 200)); got["MFA_MAX_ENROLLED_FACTORS"] != float64(10) || got["MFA_ALLOW_LOW_AAL"] != false {
		t.Errorf("the refused save changed the settings: factors %v, low AAL %v", got["MFA_MAX_ENROLLED_FACTORS"], got["MFA_ALLOW_LOW_AAL"])
	}

	// The value the GET reports is not a change.
	got := bodyMap(t, f.mustDo("PATCH", authPlatform, map[string]any{"MFA_ALLOW_LOW_AAL": false, "MFA_MAX_ENROLLED_FACTORS": 4}, 200))
	if got["MFA_MAX_ENROLLED_FACTORS"] != float64(4) {
		t.Errorf("the valid key of a save with an unchanged marker was not saved: %v", got["MFA_MAX_ENROLLED_FACTORS"])
	}

	// A name that is no setting at all.
	rec = f.do("PATCH", authPlatform, map[string]any{"NOT_A_SETTING": "x"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "NOT_A_SETTING (it is not a setting of this node)") {
		t.Errorf("unknown name: %d %s", rec.Code, rec.Body)
	}
	f.mustDo("PATCH", authPlatform, map[string]any{"NOT_A_SETTING": nil}, 200)

	// The hooks route takes only hook_* and ignores the rest, as before.
	f.mustDo("PATCH", authPlatform+"/hooks", map[string]any{"MFA_ALLOW_LOW_AAL": true}, 200)
	// The public API ignores the names it does not know, as before: a newer client must not be refused.
	f.mustDo("PATCH", authV1, map[string]any{"mfa_allow_low_aal": true, "a_newer_setting": 1}, 200)
}

// Studio's forms send the fields they show, but a client may send back more than it changes: the markers it read
// come back unchanged and the save passes.
func TestAuthPatchWithTheMarkersItReadPasses(t *testing.T) {
	f := newFixture(t)
	read := bodyMap(t, f.mustDo("GET", authPlatform, nil, 200))
	patch := map[string]any{"SITE_URL": "https://app.example.com"}
	for _, k := range []string{"MFA_ALLOW_LOW_AAL", "INDEX_WORKER_ENSURE_USER_SEARCH_INDEXES_EXIST", "MAILER_SUBJECTS_CUSTOM_CONTENTS", "MAILER_TEMPLATES_CUSTOM_CONTENTS"} {
		if _, ok := read[k]; !ok {
			t.Fatalf("the GET does not report %s", k)
		}
		patch[k] = read[k]
	}
	back := bodyMap(t, f.mustDo("PATCH", authPlatform, patch, 200))
	if back["SITE_URL"] != "https://app.example.com" {
		t.Errorf("site_url %v after the markers came back", back["SITE_URL"])
	}
	f.mustDo("PATCH", authPlatform, patch, 200) // and again
}
