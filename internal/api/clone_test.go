package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/registry"
)

// cloningSource is a backup source that also restores as a new project, the way *backup.Service does:
// RestoreWith creates the project under newRef (here in the fake registry) after the release channel lets it.
type cloningSource struct {
	*fakeBackupSource
	f       *fixture
	t       *testing.T
	release chan struct{} // RestoreWith waits for it; nil: no wait
	fail    error

	mu    sync.Mutex
	calls []cloneCall
}

type cloneCall struct {
	ref    string
	target time.Time
	newRef string
	opts   backup.RestoreOptions
}

func (c *cloningSource) RestoreWith(ctx context.Context, ref string, target time.Time, newRef string, opts backup.RestoreOptions) (*registry.Project, error) {
	c.mu.Lock()
	c.calls = append(c.calls, cloneCall{ref, target, newRef, opts})
	c.mu.Unlock()
	if c.release != nil {
		<-c.release
	}
	if c.fail != nil {
		return nil, c.fail
	}
	// The restore names the project after the backup's and places it in the organization the backup recorded.
	p := c.f.mgr.addProject(c.t, newRef, "First project (restored)", c.f.org.ID, registry.StatusActiveHealthy)
	return p, nil
}

func (c *cloningSource) last() cloneCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[len(c.calls)-1]
}

func newCloneFixture(t *testing.T) (*backupFixture, *cloningSource) {
	b := newBackupFixture(t)
	cs := &cloningSource{fakeBackupSource: b.src, f: b.fixture, t: t}
	b.srv.backups = cs
	return b, cs
}

const (
	cloneURL  = "/platform/database/" + testRef + "/clone"
	statusURL = cloneURL + "/status"
)

func (b *backupFixture) cloneStatus(t *testing.T) []map[string]any {
	t.Helper()
	rec := b.do("GET", statusURL, nil)
	if rec.Code != 200 {
		t.Fatalf("GET clone/status: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Supavise-Stub") != "" {
		t.Error("served by the stub")
	}
	validateAgainstSpec(t, "GET /platform/database/{ref}/clone/status", rec.Body.Bytes())
	body := bodyMap(t, rec)
	var out []map[string]any
	for _, c := range body["clones"].([]any) {
		out = append(out, c.(map[string]any))
	}
	return out
}

func (b *backupFixture) waitCloneStatus(t *testing.T, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cl := b.cloneStatus(t); len(cl) > 0 && cl[0]["status"] == want {
			return cl[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no clone reached %s: %v", want, b.cloneStatus(t))
	return nil
}

// The dialog lists the backups it can start from and the size the new project will have.
func TestCloneListsTheBackups(t *testing.T) {
	b, _ := newCloneFixture(t)
	rec := b.do("GET", cloneURL, nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Supavise-Stub") != "" {
		t.Error("served by the stub")
	}
	validateAgainstSpec(t, "GET /platform/database/{ref}/clone", rec.Body.Bytes())
	got := bodyMap(t, rec)
	if got["pitr_enabled"] != true || len(got["backups"].([]any)) != 2 {
		t.Errorf("backups %v", got)
	}
	if got["target_compute_size"] != sizeOf(b.project).Name || got["target_volume_size_gb"] == nil {
		t.Errorf("target size: %v %v", got["target_compute_size"], got["target_volume_size_gb"])
	}
	if got["physicalBackupData"].(map[string]any)["earliestPhysicalBackupDateUnix"] == nil {
		t.Errorf("physicalBackupData %v", got["physicalBackupData"])
	}
}

// A clone starts asynchronously, shows as IN_PROGRESS while it runs, gets the name and the database password
// the dialog asked for, lands in the source's organization, and ends COMPLETED.
func TestCloneRunsAsynchronouslyAndFinishesWithTheNameAndPassword(t *testing.T) {
	b, cs := newCloneFixture(t)
	cs.release = make(chan struct{})
	rec := b.do("POST", cloneURL, map[string]any{"cloneBackupId": b.midID, "newProjectName": "Restored copy", "newDbPass": "a-long-new-password"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST clone: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Supavise-Stub") != "" {
		t.Error("served by the stub")
	}
	validateAgainstSpec(t, "POST /platform/database/{ref}/clone", rec.Body.Bytes())
	resp := bodyMap(t, rec)
	newRef, _ := resp["target_project_ref"].(string)
	if resp["source_project_ref"] != testRef || len(newRef) != 20 || newRef == testRef {
		t.Fatalf("answer %v", resp)
	}

	// It runs: the dialog polls the status.
	cl := b.cloneStatus(t)
	if len(cl) != 1 || cl[0]["status"] != "IN_PROGRESS" || cl[0]["target_project"].(map[string]any)["ref"] != newRef || cl[0]["target_project"].(map[string]any)["name"] != "Restored copy" {
		t.Fatalf("status while running: %v", cl)
	}
	// A second clone of the same project waits for the first.
	if rec := b.do("POST", cloneURL, map[string]any{"cloneBackupId": b.midID, "newProjectName": "Again", "newDbPass": "a-long-new-password"}); rec.Code != http.StatusConflict {
		t.Errorf("second clone while one runs: %d %s", rec.Code, rec.Body)
	}

	close(cs.release)
	done := b.waitCloneStatus(t, "COMPLETED")
	if done["target_project"].(map[string]any)["name"] != "Restored copy" {
		t.Errorf("target %v", done["target_project"])
	}
	// What the restore got: the backup the dialog picked, up to that backup, files left out (as hosted).
	c := cs.last()
	if c.ref != testRef || c.newRef != newRef || !c.opts.ToBackup || c.opts.BackupID != b.mid.ID || !c.opts.SkipFiles || !c.target.IsZero() {
		t.Errorf("RestoreWith got %+v", c)
	}
	// The new project is named as asked, in the source's organization, with the password asked for.
	p, err := b.reg.GetProject(context.Background(), newRef)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Restored copy" || p.OrgID != b.org.ID {
		t.Errorf("new project %q in org %d, want the dialog's name in org %d", p.Name, p.OrgID, b.org.ID)
	}
	b.mgr.mu.Lock()
	pw := append([]string(nil), b.mgr.passwords...)
	b.mgr.mu.Unlock()
	if len(pw) != 1 || pw[0] != newRef+":a-long-new-password" {
		t.Errorf("database password changes: %v", pw)
	}
	// And a second clone can start now.
	cs.release = nil
	if rec := b.do("POST", cloneURL, map[string]any{"recoveryTimeTarget": b.now.Add(-time.Hour).Unix(), "newProjectName": "Again", "newDbPass": "a-long-new-password"}); rec.Code != http.StatusCreated {
		t.Errorf("clone after the first finished: %d %s", rec.Code, rec.Body)
	}
}

func TestCloneToAPointInTime(t *testing.T) {
	b, cs := newCloneFixture(t)
	at := b.now.Add(-time.Hour).Truncate(time.Second)
	rec := b.do("POST", cloneURL, map[string]any{"recoveryTimeTarget": at.Unix(), "newProjectName": "pit", "newDbPass": "a-long-new-password"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST clone: %d %s", rec.Code, rec.Body)
	}
	b.waitCloneStatus(t, "COMPLETED")
	c := cs.last()
	if !c.target.Equal(at) || c.opts.ToBackup || c.opts.BackupID != "" {
		t.Errorf("RestoreWith got %+v, want the time and no pinned backup", c)
	}
	// A backup and a time after it: the WAL replays from that backup.
	rec = b.do("POST", cloneURL, map[string]any{"cloneBackupId": b.oldID, "recoveryTimeTarget": at.Unix(), "newProjectName": "both", "newDbPass": "a-long-new-password"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST clone with both: %d %s", rec.Code, rec.Body)
	}
	b.waitCloneStatus(t, "COMPLETED")
	if c := cs.last(); c.opts.BackupID != b.old.ID || c.opts.ToBackup || !c.target.Equal(at) {
		t.Errorf("RestoreWith got %+v", c)
	}
}

func TestCloneIsRefusedWithTheReason(t *testing.T) {
	b, cs := newCloneFixture(t)
	good := map[string]any{"cloneBackupId": b.midID, "newProjectName": "n", "newDbPass": "a-long-new-password"}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for kk, vv := range good {
			m[kk] = vv
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	for _, tc := range []struct {
		name string
		body map[string]any
		want int
		msg  string
	}{
		{"no name", with("newProjectName", ""), 400, "newProjectName"},
		{"short password", with("newDbPass", "short"), 400, "password must be between"},
		{"neither a backup nor a time", with("cloneBackupId", nil), 400, "Pick a backup"},
		{"unknown backup", with("cloneBackupId", 99999), 404, "not found"},
		{"fractional backup", with("cloneBackupId", 1.5), 400, "backup id"},
		{"time before the first backup", map[string]any{"recoveryTimeTarget": b.old.StopTime.Add(-time.Hour).Unix(), "newProjectName": "n", "newDbPass": "a-long-new-password"}, 400, "recovery time must be between"},
		{"time after the window", map[string]any{"recoveryTimeTarget": b.now.Add(time.Hour).Unix(), "newProjectName": "n", "newDbPass": "a-long-new-password"}, 400, "recovery time must be between"},
		{"time before the chosen backup", with("recoveryTimeTarget", b.old.StopTime.Add(-time.Minute).Unix()), 400, "recovery time must be between"},
	} {
		rec := b.do("POST", cloneURL, tc.body)
		if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.msg) {
			t.Errorf("%s: %d %s, want %d with %q", tc.name, rec.Code, rec.Body, tc.want, tc.msg)
		}
	}
	if len(cs.calls) != 0 {
		t.Errorf("a refused request reached the restore: %v", cs.calls)
	}
	if rec := b.do("POST", "/platform/database/bbbbbbbbbbbbbbbbbbbb/clone", good); rec.Code != 404 {
		t.Errorf("unknown project: %d", rec.Code)
	}
	// A node without a backup service, or whose service cannot restore as a new project.
	b.srv.backups = nil
	if rec := b.do("POST", cloneURL, good); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("without backups: %d %s", rec.Code, rec.Body)
	}
	b.srv.backups = b.src // reads backups, cannot clone
	if rec := b.do("POST", cloneURL, good); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a source that cannot clone: %d %s", rec.Code, rec.Body)
	}
	// A project with no backup yet.
	b.srv.backups = &cloningSource{fakeBackupSource: &fakeBackupSource{}, f: b.fixture, t: t}
	if rec := b.do("POST", cloneURL, good); rec.Code != 400 || !strings.Contains(rec.Body.String(), "no backup to restore from") {
		t.Errorf("no backup yet: %d %s", rec.Code, rec.Body)
	}
}

// A clone that fails reads FAILED with the reason in the log and the events; one whose process stopped before
// recording the outcome reads FAILED too; one whose project was deleted reads REMOVED.
func TestCloneStatusOfFailedInterruptedAndRemovedClones(t *testing.T) {
	b, cs := newCloneFixture(t)
	cs.fail = errors.New("recovery did not reach the target")
	body := map[string]any{"cloneBackupId": b.midID, "newProjectName": "doomed", "newDbPass": "a-long-new-password"}
	if rec := b.do("POST", cloneURL, body); rec.Code != http.StatusCreated {
		t.Fatalf("POST clone: %d %s", rec.Code, rec.Body)
	}
	b.waitCloneStatus(t, "FAILED")
	events, _ := b.reg.ListEvents(context.Background(), testRef, 20)
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if !strings.Contains(strings.Join(kinds, ","), eventCloneFailed) {
		t.Errorf("events %v lack %s", kinds, eventCloneFailed)
	}

	// A request whose outcome was never recorded, with no process running it.
	_ = b.reg.AppendEvent(context.Background(), testRef, eventCloneRequested, map[string]any{"as": "dddddddddddddddddddd", "name": "interrupted"})
	var interrupted map[string]any
	for _, c := range b.cloneStatus(t) {
		if c["target_project"].(map[string]any)["ref"] == "dddddddddddddddddddd" {
			interrupted = c
		}
	}
	if interrupted == nil || interrupted["status"] != "FAILED" {
		t.Errorf("an interrupted clone: %v", interrupted)
	}

	// A finished clone whose project was deleted since.
	cs.fail = nil
	if rec := b.do("POST", cloneURL, map[string]any{"cloneBackupId": b.midID, "newProjectName": "short-lived", "newDbPass": "a-long-new-password"}); rec.Code != http.StatusCreated {
		t.Fatalf("POST clone: %d %s", rec.Code, rec.Body)
	}
	done := b.waitCloneStatus(t, "COMPLETED")
	ref := done["target_project"].(map[string]any)["ref"].(string)
	if err := b.reg.DeleteProject(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if got := b.cloneStatus(t)[0]["status"]; got != "REMOVED" {
		t.Errorf("a deleted clone: %v", got)
	}
}

// The page is offered only where a clone can run, and by role: Owners and Administrators start one, every
// member who reads backups lists them.
func TestCloneIsOfferedByCapabilityAndRole(t *testing.T) {
	b, _ := newCloneFixture(t)
	features := func() map[string]bool {
		rec := b.do("GET", "/platform/profile", nil)
		var out struct {
			Disabled []string `json:"disabled_features"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		m := map[string]bool{}
		for _, d := range out.Disabled {
			m[d] = true
		}
		return m
	}
	if features()["database:restore_to_new_project"] {
		t.Error("the page is hidden on a node whose backup service can clone")
	}
	b.srv.backups = b.src
	if !features()["database:restore_to_new_project"] {
		t.Error("the page is shown on a node whose backup service cannot clone")
	}
	b.srv.backups = nil
	if !features()["database:restore_to_new_project"] {
		t.Error("the page is shown on a node without a backup service")
	}

	b.srv.backups = &cloningSource{fakeBackupSource: b.src, f: b.fixture, t: t}
	dev := "33333333-4444-4555-8666-777777777770"
	b.addMember(dev, members.RoleDeveloper)
	tok := b.signJWT(map[string]any{"sub": dev, "email": "dev2@example.test", "role": "authenticated"})
	if rec := b.doAs(tok, "POST", cloneURL, map[string]any{"cloneBackupId": b.midID, "newProjectName": "n", "newDbPass": "a-long-new-password"}); rec.Code != http.StatusForbidden {
		t.Errorf("a Developer starts a clone: %d", rec.Code)
	}
	if rec := b.doAs(tok, "GET", cloneURL, nil); rec.Code != 200 {
		t.Errorf("a Developer lists the backups: %d", rec.Code)
	}
}
