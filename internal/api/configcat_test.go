package api

import (
	"encoding/json"
	"slices"
	"testing"
)

// The flag document is what configcat-js 9.5.1 (configcat-common 9.3.1) fetches from the Management API's origin:
// {"f": {key: {"t": type, "v": {"b": bool} | {"s": string}}}}. The SDK builds a Setting from each entry
// (t < 0 marks an already unwrapped value, the other types name the member of v) and a config without "p"
// is never redirected.
func TestConfigCatDocument(t *testing.T) {
	f := newFixture(t)
	// No credentials: the dashboard reads it before and without signing in, from its own origin.
	rec := f.doAs("", "GET", configCatPath+"?sdk=js-auto-9.5.1", nil, "Origin", f.cfg.DashboardURL())
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != f.cfg.DashboardURL() {
		t.Errorf("Access-Control-Allow-Origin %q, want the dashboard %q", got, f.cfg.DashboardURL())
	}
	if rec.Header().Get("X-Supavise-Stub") != "" {
		t.Error("served by the stub")
	}
	var doc struct {
		P json.RawMessage `json:"p"`
		F map[string]struct {
			T int            `json:"t"`
			V map[string]any `json:"v"`
		} `json:"f"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.P != nil {
		t.Errorf("preferences %s: without them the SDK never redirects", doc.P)
	}
	if len(doc.F) < 60 {
		t.Fatalf("%d flags, want every key Studio reads", len(doc.F))
	}
	for name, s := range doc.F {
		switch s.T {
		case ccBool:
			if _, ok := s.V["b"].(bool); !ok || len(s.V) != 1 {
				t.Errorf("%s: v = %v, want {\"b\": bool}", name, s.V)
			}
		case ccString:
			if _, ok := s.V["s"].(string); !ok || len(s.V) != 1 {
				t.Errorf("%s: v = %v, want {\"s\": string}", name, s.V)
			}
		default:
			t.Errorf("%s: type %d", name, s.T)
		}
	}
	// The flags that matter.
	if v := doc.F["allowDataBranching"]; v.T != ccBool || v.V["b"] != true {
		t.Errorf("allowDataBranching = %v, want true: it is what shows Include data", v)
	}
	if v := doc.F["supaviseBranchClones"]; v.V["b"] != true {
		t.Errorf("supaviseBranchClones = %v, want true on a node that serves branches", v)
	}
	if v := doc.F["warehouse"]; v.T != ccString || v.V["s"] != "none" {
		t.Errorf("warehouse = %v, want the string \"none\": Studio reads it with a string default", v)
	}
	// Everything else is off, as it is for a Studio without ConfigCat.
	for name, s := range doc.F {
		if name == "allowDataBranching" || name == "supaviseBranchClones" || name == "warehouse" {
			continue
		}
		if s.V["b"] != false {
			t.Errorf("%s = %v, want false", name, s.V)
		}
	}
}

// A node without a branching service has nothing to clone with.
func TestConfigCatDocumentWithoutBranching(t *testing.T) {
	doc := configCatDocument(false)
	flags := doc["f"].(map[string]ccSetting)
	if flags["supaviseBranchClones"].V["b"] != false || flags["allowDataBranching"].V["b"] != true {
		t.Errorf("flags %v", flags)
	}
}

// The list stays sorted and free of duplicates, so that a key added in the wrong place is noticed.
func TestConfigCatKeysAreSortedAndUnique(t *testing.T) {
	if !slices.IsSorted(configCatKeys) {
		t.Error("configCatKeys is not sorted")
	}
	seen := map[string]bool{}
	for _, k := range configCatKeys {
		if seen[k] {
			t.Errorf("%s is listed twice", k)
		}
		seen[k] = true
	}
	for _, own := range []string{"allowDataBranching", "supaviseBranchClones", "warehouse"} {
		if seen[own] {
			t.Errorf("%s is set by configCatDocument and must not be in configCatKeys", own)
		}
	}
}
