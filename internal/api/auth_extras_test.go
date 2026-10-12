package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/supavise/supavise/internal/registry"
)

// overrideUpstream makes the fixture's project reach url for the service svc.
func (f *fixture) overrideUpstream(svc, url string) {
	prev := f.srv.upstreamOverride
	f.srv.upstreamOverride = func(p *registry.Project, s string) string {
		if s == svc && p.Ref == testRef {
			return url
		}
		return prev(p, s)
	}
}

// factorsGoTrue is the part of a project's GoTrue admin API that lists and removes the MFA factors of a user.
type factorsGoTrue struct {
	*httptest.Server
	mu      sync.Mutex
	factors map[string][]string // user -> factor ids
	calls   []string
	auth    []string
	// refuse names a factor whose removal answers 500.
	refuse string
}

func newFactorsGoTrue(t *testing.T) *factorsGoTrue {
	g := &factorsGoTrue{factors: map[string][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/users/{user}/factors", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.calls = append(g.calls, "GET "+r.URL.Path)
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		ids, ok := g.factors[r.PathValue("user")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"error_code":"user_not_found","msg":"User not found"}`))
			return
		}
		out := []map[string]any{}
		for _, id := range ids {
			out = append(out, map[string]any{"id": id, "factor_type": "totp", "status": "verified"})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("DELETE /admin/users/{user}/factors/{factor}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.calls = append(g.calls, "DELETE "+r.URL.Path)
		user, factor := r.PathValue("user"), r.PathValue("factor")
		if factor == g.refuse {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"msg":"database error"}`))
			return
		}
		var keep []string
		for _, id := range g.factors[user] {
			if id != factor {
				keep = append(keep, id)
			}
		}
		g.factors[user] = keep
		_ = json.NewEncoder(w).Encode(map[string]any{"id": factor})
	})
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

const someUser = "33333333-4444-4555-8666-777777777777"

// The dashboard's "Remove MFA factors" used to toast success and remove nothing: a stub answered it.
func TestRemoveMFAFactorsCallsGoTrueForEachFactor(t *testing.T) {
	f := newFixture(t)
	g := newFactorsGoTrue(t)
	f.overrideUpstream(upGoTrue, g.URL)
	service, _ := f.projectKeys(t)
	g.factors[someUser] = []string{"f1111111-0000-4000-8000-000000000001", "f1111111-0000-4000-8000-000000000002"}
	path := "/platform/auth/" + testRef + "/users/" + someUser + "/factors"

	rec := f.do("DELETE", path, nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Supavise-Stub") != "" {
		t.Error("served by the stub")
	}
	want := []string{
		"GET /admin/users/" + someUser + "/factors",
		"DELETE /admin/users/" + someUser + "/factors/f1111111-0000-4000-8000-000000000001",
		"DELETE /admin/users/" + someUser + "/factors/f1111111-0000-4000-8000-000000000002",
	}
	if !reflect.DeepEqual(g.calls, want) {
		t.Errorf("GoTrue calls:\n got %v\nwant %v", g.calls, want)
	}
	if g.auth[0] != "Bearer "+service {
		t.Errorf("GoTrue got %q, want the project's service key", g.auth[0])
	}
	if len(g.factors[someUser]) != 0 {
		t.Errorf("factors left: %v", g.factors[someUser])
	}
	// A user with none has nothing to remove, and the answer is the same.
	g.calls = nil
	if rec := f.do("DELETE", path, nil); rec.Code != 200 || len(g.calls) != 1 {
		t.Errorf("a user without factors: %d, calls %v", rec.Code, g.calls)
	}
}

func TestRemoveMFAFactorsFailures(t *testing.T) {
	f := newFixture(t)
	g := newFactorsGoTrue(t)
	f.overrideUpstream(upGoTrue, g.URL)
	path := func(user string) string { return "/platform/auth/" + testRef + "/users/" + user + "/factors" }

	// A user GoTrue does not know.
	if rec := f.do("DELETE", path("44444444-4444-4444-8444-444444444444"), nil); rec.Code != 404 {
		t.Errorf("unknown user: %d %s", rec.Code, rec.Body)
	}
	// One factor that cannot be removed: the others still are, and the error names it.
	g.factors[someUser] = []string{"a", "b", "c"}
	g.refuse = "b"
	rec := f.do("DELETE", path(someUser), nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Removed 2 of 3") || !strings.Contains(rec.Body.String(), "b") {
		t.Errorf("a factor GoTrue refuses: %d %s", rec.Code, rec.Body)
	}
	if !reflect.DeepEqual(g.factors[someUser], []string{"b"}) {
		t.Errorf("factors left: %v, want only the refused one", g.factors[someUser])
	}
	// An id that climbs out of its place is refused before GoTrue is called.
	g.calls = nil
	if rec := f.do("DELETE", path("..%2F..%2Fsettings"), nil); rec.Code != 400 || len(g.calls) != 0 {
		t.Errorf("id with separators: %d, GoTrue calls %v", rec.Code, g.calls)
	}
	// GoTrue down.
	g.Close()
	if rec := f.do("DELETE", path(someUser), nil); rec.Code != http.StatusBadGateway {
		t.Errorf("GoTrue down: %d", rec.Code)
	}
	// Unknown and paused projects.
	if rec := f.do("DELETE", "/platform/auth/bbbbbbbbbbbbbbbbbbbb/users/"+someUser+"/factors", nil); rec.Code != 404 {
		t.Errorf("unknown project: %d", rec.Code)
	}
	_ = f.reg.SetProjectStatus(t.Context(), testRef, registry.StatusInactive)
	if rec := f.do("DELETE", path(someUser), nil); rec.Code != http.StatusConflict {
		t.Errorf("paused project: %d, want 409", rec.Code)
	}
}

// Reset to default clears the template's overrides and answers with the project's Auth config: Studio caches the
// answer as the config. A stub answered a zeroed object, so the Auth pages showed an empty Site URL until a reload.
func TestResetAuthTemplateClearsTheOverridesAndAnswersTheConfig(t *testing.T) {
	f := newFixture(t)
	f.mustDo("PATCH", authPlatform, map[string]any{
		"SITE_URL":                              "https://app.example.com",
		"URI_ALLOW_LIST":                        "https://app.example.com/**",
		"MAILER_TEMPLATES_CONFIRMATION_CONTENT": "<p>Welcome</p>",
		"MAILER_SUBJECTS_CONFIRMATION":          "Welcome!",
		"MAILER_SUBJECTS_RECOVERY":              "Recover your account",
		"MAILER_TEMPLATES_MFA_FACTOR_ENROLLED_NOTIFICATION_CONTENT": "<p>A factor was added</p>",
	}, 200)
	reset := "/platform/auth/" + testRef + "/templates/confirmation/reset"

	rec := f.do("POST", reset, nil)
	if rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Supavise-Stub") != "" {
		t.Error("served by the stub")
	}
	got := bodyMap(t, rec)
	if got["MAILER_TEMPLATES_CONFIRMATION_CONTENT"] != "" || got["MAILER_SUBJECTS_CONFIRMATION"] != "" {
		t.Errorf("the template is not reset: content %q, subject %q", got["MAILER_TEMPLATES_CONFIRMATION_CONTENT"], got["MAILER_SUBJECTS_CONFIRMATION"])
	}
	// The answer is the config: the same body as the GET, with the real Site URL and the other templates.
	if got["SITE_URL"] != "https://app.example.com" || got["URI_ALLOW_LIST"] != "https://app.example.com/**" {
		t.Errorf("the reset touched the URL settings: %v %v", got["SITE_URL"], got["URI_ALLOW_LIST"])
	}
	if got["MAILER_SUBJECTS_RECOVERY"] != "Recover your account" || got["MAILER_TEMPLATES_MFA_FACTOR_ENROLLED_NOTIFICATION_CONTENT"] != "<p>A factor was added</p>" {
		t.Errorf("the reset touched other templates: %v", got)
	}
	validateAgainstSpec(t, "GET /platform/auth/{ref}/config", rec.Body.Bytes())
	again := bodyMap(t, f.mustDo("GET", authPlatform, nil, 200))
	if !reflect.DeepEqual(got, again) {
		t.Errorf("the answer to the reset differs from GET config")
	}
	// GoTrue stops getting the subject.
	env, err := f.srv.settings.AuthEnv(t.Context(), testRef, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := env["GOTRUE_MAILER_SUBJECTS_CONFIRMATION"]; ok {
		t.Errorf("GoTrue still gets the subject: %v", env["GOTRUE_MAILER_SUBJECTS_CONFIRMATION"])
	}
	if env["GOTRUE_MAILER_SUBJECTS_RECOVERY"] != "Recover your account" {
		t.Errorf("another template's subject changed: %v", env["GOTRUE_MAILER_SUBJECTS_RECOVERY"])
	}

	// A template with an underscore in its name has a dash in the path; resetting one that holds nothing is fine.
	f.mustDo("POST", "/platform/auth/"+testRef+"/templates/mfa-factor-enrolled-notification/reset", nil, rec.Code)
	if again := bodyMap(t, f.mustDo("GET", authPlatform, nil, 200)); again["MAILER_TEMPLATES_MFA_FACTOR_ENROLLED_NOTIFICATION_CONTENT"] != "" {
		t.Errorf("the notification template is not reset: %v", again["MAILER_TEMPLATES_MFA_FACTOR_ENROLLED_NOTIFICATION_CONTENT"])
	}
	f.mustDo("POST", reset, nil, rec.Code)

	// Not a template; not a project.
	if rec := f.do("POST", "/platform/auth/"+testRef+"/templates/not-a-template/reset", nil); rec.Code != 400 {
		t.Errorf("unknown template: %d", rec.Code)
	}
	if rec := f.do("POST", "/platform/auth/bbbbbbbbbbbbbbbbbbbb/templates/confirmation/reset", nil); rec.Code != 404 {
		t.Errorf("unknown project: %d", rec.Code)
	}
}

// Studio's Auth > Emails offers Reset template only for a template that the config reports as custom
// (MAILER_TEMPLATES_CUSTOM_CONTENTS and MAILER_SUBJECTS_CUSTOM_CONTENTS, by setting name: hasCustomTemplate in
// TemplateEditor.tsx). The two objects were the spec's minimal ones, every flag false, so a template that the
// user had saved could not be reset from the dashboard.
func TestAuthConfigReportsTheTemplatesThatAreCustom(t *testing.T) {
	f := newFixture(t)
	read := func() (templates, subjects map[string]any) {
		rec := f.mustDo("GET", authPlatform, nil, 200)
		validateAgainstSpec(t, "GET /platform/auth/{ref}/config", rec.Body.Bytes())
		got := bodyMap(t, rec)
		templates, _ = got["MAILER_TEMPLATES_CUSTOM_CONTENTS"].(map[string]any)
		subjects, _ = got["MAILER_SUBJECTS_CUSTOM_CONTENTS"].(map[string]any)
		if len(templates) == 0 || len(subjects) == 0 {
			t.Fatalf("the config reports no custom flags: %v %v", templates, subjects)
		}
		return templates, subjects
	}
	custom := func(flags map[string]any) []string {
		var out []string
		for k, v := range flags {
			if v == true {
				out = append(out, k)
			}
		}
		sort.Strings(out)
		return out
	}

	// Nothing is saved: nothing is custom.
	templates, subjects := read()
	if got := custom(templates); len(got) != 0 {
		t.Errorf("templates custom on a fresh project: %v", got)
	}
	if got := custom(subjects); len(got) != 0 {
		t.Errorf("subjects custom on a fresh project: %v", got)
	}

	// A saved body or subject is custom, by the name of its setting, and only that one.
	f.mustDo("PATCH", authPlatform, map[string]any{
		"MAILER_TEMPLATES_CONFIRMATION_CONTENT":                     "<p>Welcome</p>",
		"MAILER_TEMPLATES_MFA_FACTOR_ENROLLED_NOTIFICATION_CONTENT": "<p>A factor was added</p>",
		"MAILER_SUBJECTS_RECOVERY":                                  "Recover your account",
	}, 200)
	templates, subjects = read()
	if got := strings.Join(custom(templates), ","); got != "MAILER_TEMPLATES_CONFIRMATION_CONTENT,MAILER_TEMPLATES_MFA_FACTOR_ENROLLED_NOTIFICATION_CONTENT" {
		t.Errorf("custom templates: %s", got)
	}
	if got := strings.Join(custom(subjects), ","); got != "MAILER_SUBJECTS_RECOVERY" {
		t.Errorf("custom subjects: %s", got)
	}

	// A form that sends the markers back with its change still saves.
	patch := map[string]any{
		"SITE_URL":                         "https://app.example.com",
		"MAILER_TEMPLATES_CUSTOM_CONTENTS": templates,
		"MAILER_SUBJECTS_CUSTOM_CONTENTS":  subjects,
	}
	f.mustDo("PATCH", authPlatform, patch, 200)

	// Clearing the text makes the template default again, as the reset does.
	f.mustDo("PATCH", authPlatform, map[string]any{"MAILER_SUBJECTS_RECOVERY": ""}, 200)
	if _, subjects = read(); len(custom(subjects)) != 0 {
		t.Errorf("an emptied subject is still custom: %v", custom(subjects))
	}
	f.mustDo("POST", "/platform/auth/"+testRef+"/templates/confirmation/reset", nil, 200)
	if templates, _ = read(); strings.Join(custom(templates), ",") != "MAILER_TEMPLATES_MFA_FACTOR_ENROLLED_NOTIFICATION_CONTENT" {
		t.Errorf("custom templates after resetting the confirmation one: %v", custom(templates))
	}
}
