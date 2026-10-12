package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

const (
	tagSQL  = "AI_SQL_GENERATOR_OPT_IN"
	tagLog  = "AI_LOG_GENERATOR_OPT_IN"
	tagData = "AI_DATA_GENERATOR_OPT_IN"
)

func tagsOf(t *testing.T, v any) []string {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %v", v)
	}
	raw, ok := m["opt_in_tags"].([]any)
	if !ok {
		t.Fatalf("opt_in_tags is %T (%v), want an array", m["opt_in_tags"], m["opt_in_tags"])
	}
	out := make([]string, len(raw))
	for i, r := range raw {
		out[i], _ = r.(string)
	}
	return out
}

// Studio's AI setting is a PATCH of the organization's opt_in_tags: it has to stick, show in every
// place Studio reads the organization from, and be the Owner's to change.
func TestOrganizationOptInTagsStick(t *testing.T) {
	rf := newRolesFixture(t)
	const org = "/platform/organizations/default"

	if got := tagsOf(t, decodeBody(t, rf.status(200, "owner", "GET", org, nil))); len(got) != 0 {
		t.Fatalf("a new organization has opted in to %v", got)
	}

	rec := rf.status(200, "owner", "PATCH", org, map[string]any{"opt_in_tags": []string{tagSQL, tagLog, tagSQL}})
	if got := tagsOf(t, decodeBody(t, rec)); !slices.Equal(got, []string{tagSQL, tagLog}) {
		t.Fatalf("PATCH answered %v, want the tags once each, in the order sent", got)
	}
	stored, err := rf.reg.GetOrganization(context.Background(), "default")
	if err != nil || !slices.Equal(stored.OptInTags, []string{tagSQL, tagLog}) {
		t.Fatalf("the registry holds %v (%v)", stored, err)
	}

	// Every member sees what the Owner saved, in all three places Studio and the CLI read it.
	for _, role := range []string{"owner", "admin", "dev", "ro"} {
		list := decodeBody(t, rf.status(200, role, "GET", "/platform/organizations", nil)).([]any)
		var found bool
		for _, e := range list {
			if e.(map[string]any)["slug"] == "default" {
				found = true
				if got := tagsOf(t, e); !slices.Equal(got, []string{tagSQL, tagLog}) {
					t.Errorf("%s: the list shows %v", role, got)
				}
			}
		}
		if !found {
			t.Errorf("%s: the organization is not in the list", role)
		}
		if got := tagsOf(t, decodeBody(t, rf.status(200, role, "GET", org, nil))); !slices.Equal(got, []string{tagSQL, tagLog}) {
			t.Errorf("%s: GET organization shows %v", role, got)
		}
	}
	if got := tagsOf(t, decodeBody(t, rf.status(200, "owner", "GET", "/v1/organizations/default", nil))); !slices.Equal(got, []string{tagSQL, tagLog}) {
		t.Errorf("/v1/organizations shows %v", got)
	}

	// A rename leaves the tags; an empty list clears them (Studio sends [] for "Disabled").
	rf.status(200, "owner", "PATCH", org, map[string]any{"name": "Renamed"})
	if got := tagsOf(t, decodeBody(t, rf.status(200, "owner", "GET", org, nil))); !slices.Equal(got, []string{tagSQL, tagLog}) {
		t.Fatalf("a rename changed the tags to %v", got)
	}
	rf.status(200, "owner", "PATCH", org, map[string]any{"opt_in_tags": []string{tagData, tagSQL, tagLog}})
	if got := tagsOf(t, decodeBody(t, rf.status(200, "owner", "GET", org, nil))); !slices.Equal(got, []string{tagData, tagSQL, tagLog}) {
		t.Fatalf("tags = %v", got)
	}
	rf.status(200, "owner", "PATCH", org, map[string]any{"opt_in_tags": []string{}})
	if got := tagsOf(t, decodeBody(t, rf.status(200, "owner", "GET", org, nil))); len(got) != 0 {
		t.Fatalf("an empty list did not clear the tags: %v", got)
	}
}

func TestOrganizationOptInTagsAreChecked(t *testing.T) {
	rf := newRolesFixture(t)
	const org = "/platform/organizations/default"
	rf.status(200, "owner", "PATCH", org, map[string]any{"opt_in_tags": []string{tagSQL}})
	for name, body := range map[string]any{
		"a tag the spec does not list": map[string]any{"opt_in_tags": []string{"AI_ANYTHING"}},
		"a lower-case tag":             map[string]any{"opt_in_tags": []string{"ai_sql_generator_opt_in"}},
		"not an array":                 `{"opt_in_tags": "AI_SQL_GENERATOR_OPT_IN"}`,
		"a tag among good ones":        map[string]any{"opt_in_tags": []string{tagSQL, "x"}},
	} {
		if rec := rf.as("owner", "PATCH", org, body); rec.Code != 400 {
			t.Errorf("%s: %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}
	// A refused request changes nothing.
	if got := tagsOf(t, decodeBody(t, rf.status(200, "owner", "GET", org, nil))); !slices.Equal(got, []string{tagSQL}) {
		t.Fatalf("tags after refused requests = %v", got)
	}
}

// Only the roles that may update the organization change what the AI may see; the others get 403 and
// the tags stay.
func TestOrganizationOptInTagsOnlyForWhoMayUpdateTheOrganization(t *testing.T) {
	rf := newRolesFixture(t)
	const org = "/platform/organizations/default"
	rf.status(200, "owner", "PATCH", org, map[string]any{"opt_in_tags": []string{tagSQL}})
	for _, role := range []string{"admin", "dev", "ro", "scoped", "stranger"} {
		if rec := rf.as(role, "PATCH", org, map[string]any{"opt_in_tags": []string{tagSQL, tagLog, tagData}}); rec.Code != 403 {
			t.Errorf("%s: PATCH opt_in_tags answered %d, want 403", role, rec.Code)
		}
	}
	if got := tagsOf(t, decodeBody(t, rf.status(200, "owner", "GET", org, nil))); !slices.Equal(got, []string{tagSQL}) {
		t.Fatalf("a refused request changed the tags to %v", got)
	}
}

// The model provider's key and URL are in the node's configuration and in Studio's environment, and in no
// answer of the Management API: a Read-only or Developer member (or anyone else) cannot read them from any
// route that has no path parameter beyond the project and organization the fixture has. The test does not
// count towards the route coverage check: it sends the requests itself.
func TestNoGetRouteAnswersWithTheAIProvider(t *testing.T) {
	rf := newRolesFixture(t)
	const key, host = "sk-canary-0123456789abcdef", "ai-canary.example.net"
	keyFile := filepath.Join(t.TempDir(), "studio-ai.key")
	if err := os.WriteFile(keyFile, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	rf.cfg.Studio.AI = config.StudioAI{BaseURL: "https://" + host + "/v1", APIKeyFile: keyFile, Model: "canary-model"}

	ops, err := Operations()
	if err != nil {
		t.Fatal(err)
	}
	fill := strings.NewReplacer("{ref}", testRef, "{slug}", "default", "{branch_id_or_ref}", testRef)
	asked := 0
	for _, op := range ops {
		if op.Method != "GET" {
			continue
		}
		path := fill.Replace(op.Path)
		if strings.Contains(path, "{") {
			continue
		}
		for _, role := range []string{"owner", "admin", "dev", "ro", "scoped"} {
			func() {
				defer func() { _ = recover() }() // a handler the fixture cannot serve is not what this test is about
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set("Authorization", "Bearer "+rf.tokens[role])
				rec := httptest.NewRecorder()
				rf.srv.ServeHTTP(rec, req)
				asked++
				for _, secret := range []string{key, host, "canary-model", keyFile} {
					if strings.Contains(rec.Body.String(), secret) {
						t.Errorf("%s GET %s answers with the AI provider's %q", role, path, secret)
					}
				}
			}()
		}
	}
	if asked < 100 {
		t.Fatalf("only %d requests were sent: the walk does not cover the route table", asked)
	}
}

// beforeUpdate lets a test change the registry between the read and the write of one request, as a
// second Owner's request would: do runs once, just before the first UpdateOrganization.
type beforeUpdate struct {
	registry.Registry
	do func()
}

func (b *beforeUpdate) UpdateOrganization(ctx context.Context, o *registry.Organization) error {
	if do := b.do; do != nil {
		b.do = nil
		do()
	}
	return b.Registry.UpdateOrganization(ctx, o)
}

// A rename read the organization before another Owner saved the AI opt-in; it must not write the
// old tags back, because they decide what Studio sends to a third-party model provider.
func TestOrganizationRenameKeepsTagsSavedMeanwhile(t *testing.T) {
	rf := newRolesFixture(t)
	const org = "/platform/organizations/default"
	rf.srv.reg = &beforeUpdate{Registry: rf.reg, do: func() {
		o, err := rf.reg.GetOrganization(context.Background(), "default")
		if err != nil {
			t.Error(err)
			return
		}
		o.OptInTags = []string{tagSQL}
		if err := rf.reg.UpdateOrganization(context.Background(), o); err != nil {
			t.Error(err)
		}
	}}

	rec := rf.status(200, "owner", "PATCH", org, map[string]any{"name": "Renamed"})
	if got := tagsOf(t, decodeBody(t, rec)); !slices.Equal(got, []string{tagSQL}) {
		t.Errorf("the answer shows %v, want the tags saved meanwhile", got)
	}
	stored, err := rf.reg.GetOrganization(context.Background(), "default")
	if err != nil || stored.Name != "Renamed" || !slices.Equal(stored.OptInTags, []string{tagSQL}) {
		t.Fatalf("the registry holds %+v (%v): the rename undid the opt-in", stored, err)
	}
}
