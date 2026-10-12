package api

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/health"
)

type healthEntry struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Status  string `json:"status"`
	Error   string `json:"error"`
}

func projectHealth(t *testing.T, f *fixture, query string) map[string]healthEntry {
	t.Helper()
	rec := f.do("GET", "/v1/projects/"+testRef+"/health"+query, nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var list []healthEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	by := map[string]healthEntry{}
	for _, e := range list {
		by[e.Name] = e
	}
	return by
}

// Studio's project home asks for services=functions to fill its Edge Functions row; upstream probed a function
// of Supabase's own platform, which made every project of a node without internet access read Unhealthy. The
// entry is the state of the node's Edge Runtime, and it is added only when the request names it.
func TestProjectHealthReportsTheEdgeRuntimeWhenAsked(t *testing.T) {
	f := newFixture(t)
	if _, ok := projectHealth(t, f, "")["functions"]; ok {
		t.Error("services unnamed: the spec's list has no functions service")
	}
	if _, ok := projectHealth(t, f, "?services=auth,db")["functions"]; ok {
		t.Error("services=auth,db: functions was not asked for")
	}

	// Off in the configuration: not a failure.
	f.cfg.Functions.Enabled = false
	got := projectHealth(t, f, "?services=functions")
	if e := got["functions"]; e.Status != "DISABLED" || e.Healthy || e.Error == "" || len(got) != 1 {
		t.Errorf("functions off: %+v (%d entries)", e, len(got))
	}

	f.cfg.Functions.Enabled = true
	edge := func(state health.State, detail string) HealthSource {
		return fakeHealth{rep: report([]health.Component{
			{Name: "daemon", State: health.OK, Critical: true},
			{Name: config.SvcEdgeRuntime, State: state, Detail: detail},
		})}
	}
	cases := []struct {
		name    string
		src     HealthSource
		status  string
		healthy bool
		errHas  string
	}{
		{"no health source", nil, "ACTIVE_HEALTHY", true, ""},
		{"runtime ok", edge(health.OK, ""), "ACTIVE_HEALTHY", true, ""},
		{"runtime starting", edge(health.Warn, "starting"), "COMING_UP", false, "starting"},
		{"runtime failed", edge(health.Fail, "unit is failed/failed"), "UNHEALTHY", false, "failed"},
		{"runtime parked", edge(health.Info, "parked: this node follows the leader"), "UNHEALTHY", false, "parked"},
		{"runtime not reported", fakeHealth{rep: report([]health.Component{{Name: "daemon", State: health.OK}})}, "UNHEALTHY", false, "does not report"},
		{"check failed", fakeHealth{err: errors.New("registry hung")}, "UNHEALTHY", false, "could not be checked"},
	}
	for _, c := range cases {
		f.srv.healthSrc = c.src
		got := projectHealth(t, f, "?services=db,functions")
		e := got["functions"]
		if e.Status != c.status || e.Healthy != c.healthy || (c.errHas == "" && e.Error != "") || (c.errHas != "" && !containsFold(e.Error, c.errHas)) {
			t.Errorf("%s: %+v, want %s healthy=%v error~%q", c.name, e, c.status, c.healthy, c.errHas)
		}
		if _, ok := got["db"]; !ok {
			t.Errorf("%s: the project's own services are gone", c.name)
		}
	}
}
