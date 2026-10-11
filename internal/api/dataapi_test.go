package api

import (
	"reflect"
	"testing"

	plat "github.com/supavise/supavise/internal/api/gen/platform"
	v1 "github.com/supavise/supavise/internal/api/gen/v1"
	"github.com/supavise/supavise/internal/lifecycle"
)

// The Data API fields of a create body are read from the platform body of the pinned spec. A
// re-pinned spec that renames one fails here instead of silently ignoring it.
func TestDataAPIFieldsAreTheSpecs(t *testing.T) {
	tags := func(v any) map[string]bool {
		out := map[string]bool{}
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			name, _, _ := splitTag(rt.Field(i).Tag.Get("json"))
			out[name] = true
		}
		return out
	}
	spec := tags(plat.CreateProjectBody{})
	ours := tags(createInput{})
	for _, name := range []string{"data_api_exposed_schemas", "data_api_use_api_schema", "data_api_revoke_default_privileges"} {
		if !spec[name] {
			t.Errorf("%s is not a field of the platform CreateProjectBody any more", name)
		}
		if !ours[name] {
			t.Errorf("createInput does not read %s", name)
		}
	}
	// The v1 body, which the Supabase CLI, the MCP server and API clients use, has none of them: the
	// default of a node applies.
	for name := range tags(v1.V1CreateProjectBody{}) {
		if len(name) > 8 && name[:8] == "data_api" {
			t.Errorf("the v1 body now has %s: read it", name)
		}
	}
}

func splitTag(tag string) (name string, rest string, ok bool) {
	for i := 0; i < len(tag); i++ {
		if tag[i] == ',' {
			return tag[:i], tag[i+1:], true
		}
	}
	return tag, "", false
}

func TestDataAPIOptions(t *testing.T) {
	bp := func(b bool) *bool { return &b }
	sp := func(s ...string) *[]string { s = append([]string{}, s...); return &s }
	for _, tc := range []struct {
		name string
		in   createInput
		want lifecycle.DataAPIOptions
		err  bool
	}{
		{name: "nothing sent is hosted's default for a new project", want: lifecycle.DataAPIOptions{}},
		{name: "Studio, Automatically expose new tables unchecked", in: createInput{DataAPIRevokeDefaultPrivileges: bp(true), DataAPIUseAPISchema: bp(false)}, want: lifecycle.DataAPIOptions{}},
		{name: "Studio, Automatically expose new tables checked", in: createInput{DataAPIRevokeDefaultPrivileges: bp(false), DataAPIUseAPISchema: bp(false)}, want: lifecycle.DataAPIOptions{ExposeNewTables: true}},
		{name: "the Data API off", in: createInput{DataAPIExposedSchemas: sp(), DataAPIRevokeDefaultPrivileges: bp(false)}, want: lifecycle.DataAPIOptions{ExposeNewTables: true, ExposedSchemas: "graphql_public"}},
		{name: "exposed schemas", in: createInput{DataAPIExposedSchemas: sp("public", "app")}, want: lifecycle.DataAPIOptions{ExposedSchemas: "public,app"}},
		{name: "the api schema", in: createInput{DataAPIUseAPISchema: bp(true)}, want: lifecycle.DataAPIOptions{APISchema: true, ExposedSchemas: "api,graphql_public"}},
		{name: "a schema name with a quote", in: createInput{DataAPIExposedSchemas: sp(`a"b`)}, err: true},
	} {
		got, err := dataAPIOptions(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("%s: no error", tc.name)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: got %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}
}

// The create calls reach the lifecycle manager with the options.
func TestCreateProjectPassesTheDataAPIOptions(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name, path string
		body       map[string]any
		want       lifecycle.DataAPIOptions
	}{
		{"v1 without a field", "/v1/projects", map[string]any{"name": "a"}, lifecycle.DataAPIOptions{}},
		{"platform, box checked", "/platform/projects", map[string]any{"name": "b", "data_api_revoke_default_privileges": false, "data_api_use_api_schema": false}, lifecycle.DataAPIOptions{ExposeNewTables: true}},
		{"platform, Data API off", "/platform/projects", map[string]any{"name": "c", "data_api_exposed_schemas": []string{}, "data_api_revoke_default_privileges": false}, lifecycle.DataAPIOptions{ExposeNewTables: true, ExposedSchemas: "graphql_public"}},
		{"platform, api schema", "/platform/projects", map[string]any{"name": "d", "data_api_use_api_schema": true, "data_api_revoke_default_privileges": true}, lifecycle.DataAPIOptions{APISchema: true, ExposedSchemas: "api,graphql_public"}},
	} {
		before := len(f.mgr.created)
		if rec := f.do("POST", tc.path, tc.body); rec.Code != 201 && rec.Code != 200 {
			t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
		f.mgr.mu.Lock()
		got := f.mgr.created[before:]
		f.mgr.mu.Unlock()
		if len(got) != 1 || got[0].DataAPI != tc.want {
			t.Errorf("%s: the manager got %+v, want %+v", tc.name, got, tc.want)
		}
	}
	if rec := f.do("POST", "/platform/projects", map[string]any{"name": "e", "data_api_exposed_schemas": []string{`x"y`}}); rec.Code != 400 {
		t.Fatalf("a schema name with a quote: %d %s", rec.Code, rec.Body)
	}
}

// Studio starts "Automatically expose new tables" unchecked for a user whose flag
// dataApiRevokeOnCreateDefault is on, as every hosted user's is.
func TestFeatureFlagsMakeStudioRevokeByDefault(t *testing.T) {
	f := newFixture(t)
	rec := f.do("GET", "/platform/telemetry/feature-flags?organization_slug=default", nil)
	if rec.Code != 200 || jsonField(t, rec, "dataApiRevokeOnCreateDefault") != true {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
