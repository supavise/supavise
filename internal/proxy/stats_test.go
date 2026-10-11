package proxy

import (
	"testing"
	"time"

	"github.com/supavise/supavise/internal/reqstats"
)

// Requests that match a route of a project's API are counted under their service, whatever the
// answer: a refused key, a blocked route and a request served by the upstream all count. A path
// that matches no route, a preflight, and requests to the API and Studio hosts do not.
func TestProjectRequestsAreCounted(t *testing.T) {
	stats := reqstats.New()
	h := newHarness(t, func(o *Options) { o.Stats = stats })
	pub := h.k.PublishableKey
	do := func(method, target string, headers ...string) int {
		t.Helper()
		resp, _ := h.project(method, target, headers...)
		return resp.StatusCode
	}
	do("GET", "/rest/v1/todos", "apikey", pub)
	do("GET", "/rest/v1/todos", "apikey", pub)
	do("GET", "/rest/v1/todos") // 401: counted
	do("POST", "/graphql/v1", "apikey", pub)
	do("POST", "/auth/v1/token?grant_type=password", "apikey", pub)
	do("GET", "/auth/v1/verify?token=x")
	do("GET", "/storage/v1/object/public/a/b.png")
	do("GET", "/realtime/v1/api/tenants", "apikey", h.k.SecretKey) // 403, blocked: counted
	do("POST", "/functions/v1/hello", "apikey", pub)
	if got := do("GET", "/nothing/here"); got != 404 {
		t.Fatalf("unrouted path answered %d, want 404", got)
	}
	do("OPTIONS", "/rest/v1/todos", "Origin", "http://studio.example.test", "Access-Control-Request-Method", "GET")
	h.req("GET", "api."+testDomain, "/v1/projects")
	h.req("GET", "studio."+testDomain, "/")

	now := time.Now()
	hours := stats.Series(h.ref, now, now, reqstats.Hour)
	// A request that crossed an hour boundary during the test is split between two buckets; sum them.
	var sum reqstats.Counts
	for _, b := range stats.Series(h.ref, now.Add(-time.Hour), now, reqstats.Hour) {
		for i := range sum {
			sum[i] += b.Counts[i]
		}
	}
	want := reqstats.Counts{reqstats.Rest: 3, reqstats.GraphQL: 1, reqstats.Auth: 2, reqstats.Storage: 1, reqstats.Realtime: 1, reqstats.Functions: 1}
	if sum != want {
		t.Errorf("counts = %v, want %v (hour %v)", sum, want, hours[0].Start)
	}
	if n := stats.Len(); n != 1 {
		t.Errorf("projects with counts = %d, want 1", n)
	}
}

// Without Options.Stats nothing is counted and nothing breaks.
func TestRequestsAreNotCountedWithoutStats(t *testing.T) {
	h := newHarness(t)
	if resp, _ := h.project("GET", "/rest/v1/todos", "apikey", h.k.PublishableKey); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
