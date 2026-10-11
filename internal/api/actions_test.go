package api

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/registry"
)

// stepStatuses reads the statuses of a run's steps from a run response, in order.
func stepStatuses(t *testing.T, run any) string {
	t.Helper()
	var out []string
	for _, s := range run.(map[string]any)["run_steps"].([]any) {
		m := s.(map[string]any)
		out = append(out, m["name"].(string)+"="+m["status"].(string))
	}
	return strings.Join(out, ",")
}

func TestActionRuns(t *testing.T) {
	f := newFixture(t)
	f.bdb.migs[testRef] = []branching.Migration{{Version: "20260101000000", Name: "init", Statements: []string{"create table t (id int)"}}}
	rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "feat"})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	ref, id := jsonField(t, rec, "project_ref").(string), jsonField(t, rec, "id").(string)

	// The run of the create exists with all its seven steps the moment the branch is listed (Studio
	// reads a run without steps as running for ever), and is listed under the branch.
	list := f.do("GET", "/v1/projects/"+ref+"/actions", nil)
	if list.Code != 200 {
		t.Fatalf("list: %d %s", list.Code, list.Body)
	}
	validateAgainstSpec(t, "GET /v1/projects/{ref}/actions", list.Body.Bytes())
	runs := decodeBody(t, list).([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %s", list.Body)
	}
	create := runs[0].(map[string]any)
	runID := create["id"].(string)
	if n := len(create["run_steps"].([]any)); n != 7 {
		t.Fatalf("a new run has %d steps, want 7: %v", n, create)
	}
	if create["branch_id"] != id || create["check_run_id"] != nil || create["workdir"] != nil {
		t.Fatalf("run = %v", create)
	}
	f.waitBranch(t, "feat", "FUNCTIONS_DEPLOYED")
	detail := f.do("GET", "/v1/projects/"+ref+"/actions/"+runID, nil)
	validateAgainstSpec(t, "GET /v1/projects/{ref}/actions/{run_id}", detail.Body.Bytes())
	run := decodeBody(t, detail)
	if got := stepStatuses(t, run); got != "clone=PAUSED,pull=EXITED,health=EXITED,configure=PAUSED,migrate=EXITED,seed=PAUSED,deploy=EXITED" {
		t.Fatalf("steps = %s", got)
	}

	f.run(t, []step{
		{key: "HEAD /v1/projects/{ref}/actions", path: "/v1/projects/" + ref + "/actions", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			if rec.Header().Get("X-Total-Count") != "1" || rec.Body.Len() != 0 {
				t.Errorf("HEAD: count %q, body %d bytes", rec.Header().Get("X-Total-Count"), rec.Body.Len())
			}
		}},
		{key: "GET /v1/projects/{ref}/actions/{run_id}/logs", path: "/v1/projects/" + ref + "/actions/" + runID + "/logs", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("content type = %q", ct)
			}
			if !strings.Contains(rec.Body.String(), "[migrate]") || !strings.Contains(rec.Body.String(), "replaying 1 migration(s)") {
				t.Errorf("log = %q", rec.Body)
			}
		}},
		// The parent has no run of this create: 404 under the other ref, for the detail and the log.
		{key: "GET /v1/projects/{ref}/actions/{run_id}", path: "/v1/projects/" + testRef + "/actions/" + runID, status: 404},
		{key: "GET /v1/projects/{ref}/actions/{run_id}/logs", path: "/v1/projects/" + testRef + "/actions/" + runID + "/logs", status: 404},
		{key: "GET /v1/projects/{ref}/actions/{run_id}", path: "/v1/projects/" + ref + "/actions/not-a-uuid", status: 404},
		{key: "GET /v1/projects/{ref}/actions/{run_id}", path: "/v1/projects/zzzzzzzzzzzzzzzzzzzz/actions/" + runID, status: 404},
		{key: "GET /v1/projects/{ref}/actions", path: "/v1/projects/" + testRef + "/actions", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			if strings.TrimSpace(rec.Body.String()) != "[]" || rec.Header().Get("X-Total-Count") != "0" {
				t.Errorf("the parent lists %s (%s)", rec.Body, rec.Header().Get("X-Total-Count"))
			}
		}},
	})

	// A merge is owned by the parent: its run is listed there, answers 200 under the parent and
	// 404 under the branch, which is how Studio's two polls tell which ref owns it.
	f.bdb.migs[ref] = append(f.bdb.migs[ref], branching.Migration{Version: "20260110000000", Name: "feature", Statements: []string{"create table f (id int)"}})
	mrec := f.do("POST", "/v1/branches/"+id+"/merge", map[string]any{})
	if mrec.Code != 201 {
		t.Fatalf("merge: %d %s", mrec.Code, mrec.Body)
	}
	mergeRun := jsonField(t, mrec, "workflow_run_id").(string)
	f.waitBranch(t, "feat", "FUNCTIONS_DEPLOYED")
	f.run(t, []step{
		{key: "GET /v1/projects/{ref}/actions/{run_id}", path: "/v1/projects/" + testRef + "/actions/" + mergeRun, check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			if got := stepStatuses(t, decodeBody(t, rec)); !strings.Contains(got, "migrate=EXITED") || !strings.Contains(got, "deploy=EXITED") || jsonField(t, rec, "branch_id") != id {
				t.Errorf("merge run = %s", rec.Body)
			}
		}},
		{key: "GET /v1/projects/{ref}/actions/{run_id}", path: "/v1/projects/" + ref + "/actions/" + mergeRun, status: 404},
		{key: "GET /v1/projects/{ref}/actions", path: "/v1/projects/" + testRef + "/actions", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			if jsonField(t, rec, "0.id") != mergeRun || len(decodeBody(t, rec).([]any)) != 1 {
				t.Errorf("parent's runs = %s", rec.Body)
			}
		}},
	})

	// Pagination: newest first, X-Total-Count holds the total.
	for _, op := range []string{"push", "reset"} {
		if r := f.do("POST", "/v1/branches/"+id+"/"+op, map[string]any{}); r.Code != 201 {
			t.Fatalf("%s: %d %s", op, r.Code, r.Body)
		}
		f.waitBranch(t, "feat", "FUNCTIONS_DEPLOYED")
	}
	page := f.do("GET", "/v1/projects/"+ref+"/actions?limit=2&offset=0", nil)
	if n := len(decodeBody(t, page).([]any)); n != 2 || page.Header().Get("X-Total-Count") != "3" {
		t.Fatalf("first page: %d runs, total %s", n, page.Header().Get("X-Total-Count"))
	}
	validateAgainstSpec(t, "GET /v1/projects/{ref}/actions", page.Body.Bytes())
	last := f.do("GET", "/v1/projects/"+ref+"/actions?limit=2&offset=2", nil)
	if n := len(decodeBody(t, last).([]any)); n != 1 || jsonField(t, last, "0.id") != runID {
		t.Fatalf("last page: %s", last.Body)
	}
	if head := f.do("HEAD", "/v1/projects/"+ref+"/actions", nil); head.Header().Get("X-Total-Count") != "3" {
		t.Fatalf("HEAD count = %q", head.Header().Get("X-Total-Count"))
	}
	if bad := f.do("GET", "/v1/projects/"+ref+"/actions?limit=-3&offset=junk", nil); bad.Code != 200 {
		t.Fatalf("lenient paging: %d", bad.Code)
	}
}

func TestActionRunStatusPatch(t *testing.T) {
	f := newFixture(t)
	rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "ci"})
	ref := jsonField(t, rec, "project_ref").(string)
	f.waitBranch(t, "ci", "FUNCTIONS_DEPLOYED")

	// A run that a CI job drives: the node is not working on it.
	const ciRun = "99999999-9999-4999-8999-999999999999"
	r := &registry.BranchRun{ID: ciRun, OwnerRef: ref, BranchRef: ref, ParentRef: testRef, Op: "push", Status: registry.RunRunning, Workdir: "supabase", CheckRunID: 987654321012}
	for _, n := range registry.BranchRunSteps {
		r.Steps = append(r.Steps, registry.BranchRunStep{Name: n, Status: registry.StepCreated})
	}
	if err := registry.BranchRunsOf(f.reg).CreateBranchRun(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/" + ref + "/actions/" + ciRun
	// Zero steps never reads as finished; a created run with steps is still running.
	detail := f.do("GET", path, nil)
	if got := stepStatuses(t, decodeBody(t, detail)); !strings.HasPrefix(got, "clone=CREATED") {
		t.Fatalf("steps = %s", got)
	}
	if jsonField(t, detail, "workdir") != "supabase" || strconv.FormatFloat(jsonField(t, detail, "check_run_id").(float64), 'f', -1, 64) != "987654321012" {
		t.Fatalf("run = %s", detail.Body)
	}
	validateAgainstSpec(t, "GET /v1/projects/{ref}/actions/{run_id}", detail.Body.Bytes())

	f.run(t, []step{
		{key: "PATCH /v1/projects/{ref}/actions/{run_id}/status", path: path + "/status", body: map[string]any{"clone": "EXITED", "pull": "RUNNING"}, check: want("message", "ok")},
	})
	if got := stepStatuses(t, decodeBody(t, f.do("GET", path, nil))); !strings.HasPrefix(got, "clone=EXITED,pull=RUNNING,health=CREATED") {
		t.Fatalf("steps = %s", got)
	}
	// Every step done: the run is a success by Studio's rule (EXITED or PAUSED everywhere).
	all := map[string]any{}
	for _, n := range registry.BranchRunSteps {
		all[n] = "EXITED"
	}
	all["configure"] = "PAUSED"
	if rec := f.do("PATCH", path+"/status", all); rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	got, _ := registry.BranchRunsOf(f.reg).GetBranchRun(t.Context(), ciRun)
	if got.Status != registry.RunSuccess {
		t.Fatalf("run = %+v", got)
	}
	// A DEAD step fails it.
	if rec := f.do("PATCH", path+"/status", map[string]any{"migrate": "DEAD"}); rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	if got, _ = registry.BranchRunsOf(f.reg).GetBranchRun(t.Context(), ciRun); got.Status != registry.RunFailed {
		t.Fatalf("run = %+v", got)
	}
	for name, body := range map[string]any{
		"a status this node never uses": map[string]any{"pull": "RESTARTING"},
		"an unknown status":             map[string]any{"pull": "ASLEEP"},
	} {
		if rec := f.do("PATCH", path+"/status", body); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	// Not under the parent, which does not own it, and not for a run that does not exist.
	if rec := f.do("PATCH", "/v1/projects/"+testRef+"/actions/"+ciRun+"/status", map[string]any{"pull": "EXITED"}); rec.Code != 404 {
		t.Errorf("patch under the wrong ref: %d", rec.Code)
	}
	if rec := f.do("PATCH", path+"x/status", map[string]any{"pull": "EXITED"}); rec.Code != 404 {
		t.Errorf("patch of an unknown run: %d", rec.Code)
	}
}

func TestActionRunsWithoutBranching(t *testing.T) {
	f := newFixture(t)
	srv, err := NewServer(Deps{Registry: f.reg, Secrets: f.srv.sec, Manager: f.mgr, Config: f.cfg, PGMetaURL: f.meta.URL})
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv
	f.ownerOn(srv)
	rec := f.do("GET", "/v1/projects/"+testRef+"/actions", nil)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" || rec.Header().Get("X-Total-Count") != "0" {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("HEAD", "/v1/projects/"+testRef+"/actions", nil); rec.Code != 200 || rec.Header().Get("X-Total-Count") != "0" {
		t.Fatalf("head: %d", rec.Code)
	}
	if rec := f.do("GET", "/v1/projects/"+testRef+"/actions/00000000-0000-4000-8000-000000000000", nil); rec.Code != 404 {
		t.Fatalf("get: %d", rec.Code)
	}
	if rec := f.do("GET", "/v1/projects/"+testRef+"/actions/00000000-0000-4000-8000-000000000000/logs", nil); rec.Code != 404 {
		t.Fatalf("logs: %d", rec.Code)
	}
	if rec := f.do("PATCH", "/v1/projects/"+testRef+"/actions/00000000-0000-4000-8000-000000000000/status", map[string]any{"pull": "EXITED"}); rec.Code != 404 {
		t.Fatalf("patch: %d", rec.Code)
	}
}
