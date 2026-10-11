package api

import (
	"context"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/registry"
)

// The custom-hostname and vanity-subdomain calls of Studio's Custom domains page take a branch ref
// like a project's: the page renders for a branch ("for your branch") and claims, verifies and
// activates its own hostname, with the branch's host as the CNAME target.
func TestCustomHostnameOnABranch(t *testing.T) {
	f := newFixture(t)
	dns := f.withDNS()
	ref, id := makeBranch(t, f, "feat")
	p := "/v1/projects/" + ref + "/custom-hostname"

	rec := f.do("GET", p, nil)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "custom hostname configuration") {
		t.Fatalf("GET before initialize: %d %s", rec.Code, rec.Body)
	}
	rec = f.do("POST", p+"/initialize", map[string]any{"custom_hostname": "preview.example.org"})
	if rec.Code != 201 {
		t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, keyHostnameNew, rec.Body.Bytes())
	res := mapAt(t, decodeBody(t, rec), "data", "result")
	if mapAt(t, res, "custom_origin_server") != ref+".api.example.test" {
		t.Fatalf("the CNAME target is %v, want the branch's own host", mapAt(t, res, "custom_origin_server"))
	}
	txtName, txtValue := mapAt(t, res, "ssl", "txt_name").(string), mapAt(t, res, "ssl", "txt_value").(string)
	dns.txt[txtName] = []string{txtValue}
	dns.host["preview.example.org"] = []string{"203.0.113.7"}
	if rec := f.do("POST", p+"/reverify", nil); rec.Code != 201 || mapAt(t, decodeBody(t, rec), "status") != "4_origin_setup_completed" {
		t.Fatalf("reverify: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", p+"/activate", nil); rec.Code != 201 || mapAt(t, decodeBody(t, rec), "status") != "5_services_reconfigured" {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body)
	}
	if rt := f.routeFor("preview.example.org"); rt == nil || rt.Ref != ref || rt.Kind != registry.RouteCustom {
		t.Fatalf("route: %+v", rt)
	}
	if got := f.mgr.applied; len(got) != 1 || got[0] != ref+" auth" {
		t.Fatalf("the branch's GoTrue was not reconfigured: %v", got)
	}
	// The parent has none of it.
	if rec := f.do("GET", "/v1/projects/"+testRef+"/custom-hostname", nil); rec.Code != 400 {
		t.Fatalf("the parent's hostname: %d %s", rec.Code, rec.Body)
	}

	// Deleting the branch frees the hostname and its route, and the parent can use the name.
	if rec := f.do("DELETE", "/v1/branches/"+id, nil); rec.Code != 200 {
		t.Fatalf("delete the branch: %d %s", rec.Code, rec.Body)
	}
	if f.routeFor("preview.example.org") != nil {
		t.Fatal("the route survived the branch")
	}
	if hs, _ := f.reg.ListCustomHostnames(context.Background()); len(hs) != 0 {
		t.Fatalf("hostnames left: %+v", hs)
	}
	if rec := f.do("POST", "/v1/projects/"+testRef+"/custom-hostname/initialize", map[string]any{"custom_hostname": "preview.example.org"}); rec.Code != 201 {
		t.Fatalf("the parent could not take the freed name: %d %s", rec.Code, rec.Body)
	}
}

func TestVanityOnABranch(t *testing.T) {
	f := newFixture(t)
	f.withDNS()
	ref, _ := makeBranch(t, f, "feat")
	p := "/v1/projects/" + ref + "/vanity-subdomain"
	rec := f.do("POST", p+"/activate", map[string]any{"vanity_subdomain": "feat-one"})
	if rec.Code != 201 || mapAt(t, decodeBody(t, rec), "custom_domain") != "feat-one.api.example.test" {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body)
	}
	if rt := f.routeFor("feat-one.api.example.test"); rt == nil || rt.Ref != ref || rt.Kind != registry.RouteVanity {
		t.Fatalf("route: %+v", rt)
	}
	// The parent sees the name as taken.
	if rec := f.do("POST", "/v1/projects/"+testRef+"/vanity-subdomain/activate", map[string]any{"vanity_subdomain": "feat-one"}); rec.Code != 409 {
		t.Fatalf("the parent took the branch's name: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("DELETE", p, nil); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
}
