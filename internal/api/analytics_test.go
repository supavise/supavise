package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	plat "github.com/supavise/supavise/internal/api/gen/platform"
	v1 "github.com/supavise/supavise/internal/api/gen/v1"
	"github.com/supavise/supavise/internal/reqstats"
)

// The endpoints that need logs answer a 200 with an empty result and a string error that names
// `supavise logs <ref>`: never a bare empty result, which reads as "no errors". The body satisfies
// the spec's string-or-object union for every one of them.
func TestLogEndpointsSayThereAreNoLogs(t *testing.T) {
	f := newFixture(t)
	const pp = "/platform/projects/" + testRef + "/analytics/endpoints/"
	cases := []struct{ method, path, key string }{
		{"GET", pp + "logs.all?sql=select+1", "GET /platform/projects/{ref}/analytics/endpoints/logs.all"},
		{"POST", pp + "logs.all", "POST /platform/projects/{ref}/analytics/endpoints/logs.all"},
		{"GET", pp + "logs.all.otel", "GET /platform/projects/{ref}/analytics/endpoints/logs.all.otel"},
		{"POST", pp + "logs.all.otel", "POST /platform/projects/{ref}/analytics/endpoints/logs.all.otel"},
		{"GET", pp + "logs", "GET /platform/projects/{ref}/analytics/endpoints/logs"},
		{"POST", pp + "logs", "POST /platform/projects/{ref}/analytics/endpoints/logs"},
		{"GET", pp + "functions.combined-stats", "GET /platform/projects/{ref}/analytics/endpoints/functions.combined-stats"},
		{"GET", pp + "functions.req-stats", "GET /platform/projects/{ref}/analytics/endpoints/functions.req-stats"},
		{"GET", pp + "functions.resource-usage", "GET /platform/projects/{ref}/analytics/endpoints/functions.resource-usage"},
		{"GET", pp + "auth.metrics", "GET /platform/projects/{ref}/analytics/endpoints/auth.metrics"},
		{"GET", pp + "api_keys.last_used.otel", "GET /platform/projects/{ref}/analytics/endpoints/api_keys.last_used.otel"},
		{"GET", pp + "project.metrics", "GET /platform/projects/{ref}/analytics/endpoints/project.metrics"},
		{"GET", pp + "project.metrics.otel", "GET /platform/projects/{ref}/analytics/endpoints/project.metrics.otel"},
		{"GET", pp + "service-health", "GET /platform/projects/{ref}/analytics/endpoints/service-health"},
		{"GET", "/v1/projects/" + testRef + "/analytics/endpoints/logs", "GET /v1/projects/{ref}/analytics/endpoints/logs"},
		{"GET", "/v1/projects/" + testRef + "/analytics/endpoints/functions.combined-stats", "GET /v1/projects/{ref}/analytics/endpoints/functions.combined-stats"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			rec := f.do(c.method, c.path, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if rec.Header().Get("X-Supavise-Stub") != "" {
				t.Error("served by the stub")
			}
			validateAgainstSpec(t, c.key, rec.Body.Bytes())
			var out plat.AnalyticsResponseOutput
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out.Result == nil || len(*out.Result) != 0 {
				t.Errorf("result = %v, want an empty array", out.Result)
			}
			if out.Error == nil {
				t.Fatalf("no error in %s", rec.Body)
			}
			msg, err := out.Error.AsAnalyticsResponseOutputError0()
			if err != nil || !strings.Contains(msg, "does not collect logs") || !strings.Contains(msg, "`supavise logs "+testRef+"`") {
				t.Errorf("error = %q (%v), want a string that says logs are not collected and names `supavise logs %s`", msg, err, testRef)
			}
			if !strings.Contains(rec.Body.String(), `"result":[]`) {
				t.Errorf("body %s has no empty result array", rec.Body)
			}
		})
	}
	if rec := f.do("GET", "/platform/projects/bbbbbbbbbbbbbbbbbbbb/analytics/endpoints/logs.all", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown project: %d, want 404", rec.Code)
	}
}

// Hosted removed the v1 logs.all endpoint; its spec answers 410.
func TestV1LogsAllIsGone(t *testing.T) {
	f := newFixture(t)
	rec := f.do("GET", "/v1/projects/"+testRef+"/analytics/endpoints/logs.all", nil)
	if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "removed") {
		t.Errorf("v1 logs.all: %d %s, want 410", rec.Code, rec.Body)
	}
}

// usageFixture is a server whose clock stands at a fixed time and that has a request counter.
func usageFixture(t *testing.T, now time.Time) (*fixture, *reqstats.Counter) {
	f := newFixture(t)
	stats := reqstats.New()
	srv, err := NewServer(Deps{Registry: f.reg, Secrets: f.mgr.sec, Manager: f.mgr, Config: f.cfg, PGMetaURL: f.meta.URL,
		Requests: stats, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv
	f.ownerOn(srv)
	return f, stats
}

func TestUsageAPICounts(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 30, 20, 0, time.UTC)
	f, stats := usageFixture(t, now)
	stats.Add(testRef, reqstats.Rest, now)
	stats.Add(testRef, reqstats.Rest, now.Add(-time.Second))
	stats.Add(testRef, reqstats.GraphQL, now) // not in the chart: hosted's rest counter matches /rest only
	stats.Add(testRef, reqstats.Auth, now.Add(-10*time.Minute))
	stats.Add(testRef, reqstats.Storage, now.Add(-3*time.Hour))
	stats.Add(testRef, reqstats.Realtime, now.Add(-2*24*time.Hour))
	stats.Add(testRef, reqstats.Rest, now.Add(-6*24*time.Hour))
	stats.Add("bbbbbbbbbbbbbbbbbbbb", reqstats.Rest, now) // another project

	get := func(path, key, interval string) []v1.V1GetUsageApiCountResponseOutput {
		t.Helper()
		rec := f.do("GET", path+"?interval="+interval, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", interval, rec.Code, rec.Body)
		}
		validateAgainstSpec(t, key, rec.Body.Bytes())
		var out v1.V1GetUsageApiCountResponseOutput
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Error != nil || out.Result == nil {
			t.Fatalf("%s: error %v result %v", interval, out.Error, out.Result)
		}
		return []v1.V1GetUsageApiCountResponseOutput{out}
	}
	type sums struct{ rows, rest, auth, storage, realtime int }
	sum := func(o v1.V1GetUsageApiCountResponseOutput) (s sums) {
		for _, r := range *o.Result {
			s.rows++
			s.rest += int(r.TotalRestRequests)
			s.auth += int(r.TotalAuthRequests)
			s.storage += int(r.TotalStorageRequests)
			s.realtime += int(r.TotalRealtimeRequests)
		}
		return s
	}
	for _, base := range []string{"/platform/projects/" + testRef + "/analytics/endpoints/usage.api-counts", "/v1/projects/" + testRef + "/analytics/endpoints/usage.api-counts"} {
		key := "GET " + strings.Replace(base, testRef, "{ref}", 1)
		// 1hr: 61 minute buckets (12:30 back to 11:30), the minute of "now" last.
		o := get(base, key, "1hr")[0]
		rows := *o.Result
		if got := sum(o); got != (sums{rows: 61, rest: 2, auth: 1}) {
			t.Errorf("%s 1hr = %+v, want 61 rows, rest 2, auth 1", base, got)
		}
		if !rows[0].Timestamp.Equal(time.Date(2026, 10, 10, 11, 30, 0, 0, time.UTC)) || !rows[60].Timestamp.Equal(time.Date(2026, 10, 10, 12, 30, 0, 0, time.UTC)) {
			t.Errorf("1hr runs from %v to %v, want 11:30 to 12:30", rows[0].Timestamp, rows[60].Timestamp)
		}
		if rows[60].TotalRestRequests != 2 || rows[50].TotalAuthRequests != 1 {
			t.Errorf("1hr: last minute rest = %v, ten minutes back auth = %v, want 2 and 1", rows[60].TotalRestRequests, rows[50].TotalAuthRequests)
		}
		// 1day: hour buckets, 25 of them; the storage request three hours ago is in.
		if got := sum(get(base, key, "1day")[0]); got != (sums{rows: 25, rest: 2, auth: 1, storage: 1}) {
			t.Errorf("%s 1day = %+v", base, got)
		}
		// 7day: UTC day buckets, 8 of them; the realtime request two days back and the rest six days back are in.
		if got := sum(get(base, key, "7day")[0]); got != (sums{rows: 8, rest: 3, auth: 1, storage: 1, realtime: 1}) {
			t.Errorf("%s 7day = %+v", base, got)
		}
		// No interval, or one the spec does not list: the day chart.
		if rec := f.do("GET", base, nil); rec.Code != 200 || strings.Count(rec.Body.String(), `"timestamp"`) != 25 {
			t.Errorf("%s without interval: %d, %d rows", base, rec.Code, strings.Count(rec.Body.String(), `"timestamp"`))
		}
	}
	if rec := f.do("GET", "/platform/projects/bbbbbbbbbbbbbbbbbbbb/analytics/endpoints/usage.api-counts?interval=1hr", nil); rec.Code != 404 {
		t.Errorf("unknown project: %d, want 404", rec.Code)
	}
}

func TestUsageAPIRequestsCount(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 30, 20, 0, time.UTC)
	f, stats := usageFixture(t, now)
	stats.Add(testRef, reqstats.Rest, now)
	stats.Add(testRef, reqstats.Functions, now.Add(-5*time.Hour))
	stats.Add(testRef, reqstats.GraphQL, now.Add(-23*time.Hour))           // 13:00 yesterday: inside the 24 hourly buckets
	stats.Add(testRef, reqstats.Storage, now.Add(-24*time.Hour-time.Hour)) // outside
	for _, path := range []string{
		"/platform/projects/" + testRef + "/analytics/endpoints/usage.api-requests-count",
		"/v1/projects/" + testRef + "/analytics/endpoints/usage.api-requests-count",
	} {
		rec := f.do("GET", path, nil)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		validateAgainstSpec(t, "GET /v1/projects/{ref}/analytics/endpoints/usage.api-requests-count", rec.Body.Bytes())
		var out v1.V1GetUsageApiRequestsCountResponseOutput
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Result == nil || len(*out.Result) != 1 || (*out.Result)[0].Count != 3 {
			t.Errorf("%s = %s, want a count of 3", path, rec.Body)
		}
	}
}

// A server without a request counter says so instead of charting zeros.
func TestUsageEndpointsWithoutACounter(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"usage.api-counts", "usage.api-requests-count"} {
		rec := f.do("GET", "/platform/projects/"+testRef+"/analytics/endpoints/"+name, nil)
		var out plat.AnalyticsResponseOutput
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Error == nil || out.Result == nil || len(*out.Result) != 0 {
			t.Errorf("%s: %d %s, want an empty result and an error", name, rec.Code, rec.Body)
		}
	}
}
