package branching

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// A deletion scheduled with force=false pauses the branch, so that the dashboard's Restore works,
// and Restore cancels the deletion and starts the branch again.
func TestScheduledDeletePausesAndRestoreResumes(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("a", nil)

	got, err := h.svc.Delete(ctx, b.Ref, DeleteOptions{Schedule: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.DeletionScheduledAt == nil || got.ProjectStatus != registry.StatusInactive {
		t.Fatalf("scheduled branch = %+v", got)
	}
	if want := h.clock().Add(time.Hour); !got.DeletionScheduledAt.Equal(want.UTC()) {
		t.Fatalf("deadline %v, want %v", got.DeletionScheduledAt, want)
	}
	if reason := h.svc.PauseReason(ctx, b.Ref); reason != "owner" {
		t.Fatalf("pause reason = %q", reason)
	}
	if h.eng.paused[b.Ref] != 1 || len(h.eng.deletes) != 0 {
		t.Fatalf("pauses %d, deletes %v", h.eng.paused[b.Ref], h.eng.deletes)
	}

	back, err := h.svc.Restore(ctx, b.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if back.DeletionScheduledAt != nil || back.ProjectStatus != registry.StatusActiveHealthy || h.eng.resumed[b.Ref] != 1 {
		t.Fatalf("restored branch = %+v, resumes %d", back, h.eng.resumed[b.Ref])
	}
	// Nothing to restore now.
	if _, err := h.svc.Restore(ctx, b.Ref); !isConflict(err) {
		t.Fatalf("restore of a running, unscheduled branch: %v", err)
	}
}

func isConflict(err error) bool { return err != nil && strings.Contains(err.Error(), "conflict") }

// The sweeper deletes the paused branch at the deadline, and not before.
func TestScheduledDeleteIsSweptAtTheDeadline(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("a", nil)
	if _, err := h.svc.Delete(ctx, b.Ref, DeleteOptions{Schedule: true}); err != nil {
		t.Fatal(err)
	}
	h.advance(59 * time.Minute)
	if res, err := h.svc.Sweep(ctx, false); err != nil || len(res.Expired) != 0 {
		t.Fatalf("swept before the deadline: %+v %v", res, err)
	}
	h.advance(2 * time.Minute)
	res, err := h.svc.Sweep(ctx, false)
	if err != nil || len(res.Expired) != 1 || res.Expired[0] != b.Ref {
		t.Fatalf("sweep at the deadline: %+v %v", res, err)
	}
	if _, err := h.reg.GetProject(ctx, b.Ref); err == nil {
		t.Fatal("the branch is still there")
	}
}

// A branch the node had paused for idleness becomes the owner's when its deletion is scheduled: a
// request must not wake what is on its way out.
func TestScheduledDeleteTurnsAnIdlePauseIntoTheOwners(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("a", nil)
	if err := h.eng.PauseWith(ctx, b.Ref, lifecycle.PauseOptions{Reason: registry.PauseIdle}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Delete(ctx, b.Ref, DeleteOptions{Schedule: true}); err != nil {
		t.Fatal(err)
	}
	if h.svc.PauseReason(ctx, b.Ref) != "owner" {
		t.Fatalf("pause reason = %q", h.svc.PauseReason(ctx, b.Ref))
	}
	if err := h.svc.Wake(ctx, b.Ref); err == nil || h.eng.resumed[b.Ref] != 0 {
		t.Fatalf("a request woke a branch scheduled for deletion: %v", err)
	}
	if h.eng.paused[b.Ref] != 1 {
		t.Fatalf("an already paused branch was paused again: %d", h.eng.paused[b.Ref])
	}
}

// A pause that fails does not undo the schedule.
func TestScheduledDeleteStandsWhenThePauseFails(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("a", nil)
	h.eng.pauseErr = errBoom
	got, err := h.svc.Delete(ctx, b.Ref, DeleteOptions{Schedule: true})
	if err != nil || got.DeletionScheduledAt == nil {
		t.Fatalf("scheduled = %+v, %v", got, err)
	}
	if got.ProjectStatus != registry.StatusActiveHealthy {
		t.Fatalf("status = %s", got.ProjectStatus)
	}
}

var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "boom" }

// Resetting a branch also removes the Storage objects of the old cluster; deleting one does too (the
// engine's own delete), and neither takes the objects of a branch that merely pauses.
func TestResetPurgesObjectsAndKeepsTheRecord(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("a", nil)
	if err := h.svc.removeBranch(ctx, branchOfRef(t, h, b.Ref), removeOptions{keepRecord: true}); err != nil {
		t.Fatal(err)
	}
	if !h.eng.keptRecord[b.Ref] || !h.eng.purged[b.Ref] {
		t.Fatalf("kept record %v, purge objects %v", h.eng.keptRecord[b.Ref], h.eng.purged[b.Ref])
	}
}

func branchOfRef(t *testing.T, h *harness, ref string) *Branch {
	t.Helper()
	p, err := h.reg.GetProject(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return branchOf(p)
}

// ---- expiry warning ---------------------------------------------------------------------------

type posted struct {
	mu   sync.Mutex
	got  []map[string]any
	hdrs []http.Header
}

func (p *posted) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		p.mu.Lock()
		p.got = append(p.got, m)
		p.hdrs = append(p.hdrs, r.Header.Clone())
		p.mu.Unlock()
	})
}

// n counts the expiry warnings received (the branch's create posted its own run.completed).
func (p *posted) n() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, m := range p.got {
		if m["type"] == "branch.expiring" {
			n++
		}
	}
	return n
}

// last is the latest warning and its headers.
func (p *posted) last() (map[string]any, http.Header) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.got) - 1; i >= 0; i-- {
		if p.got[i]["type"] == "branch.expiring" {
			return p.got[i], p.hdrs[i]
		}
	}
	return nil, nil
}

func expiringHarness(t *testing.T) (*harness, *posted) {
	t.Helper()
	h := newHarness(t, func(c *config.Config) {
		c.Branching.DefaultTTL = "48h"
		c.Branching.AllowPrivateNotifyURLs = true // the receiver is on loopback
	})
	return h, &posted{}
}

func TestSweepWarnsOnceBeforeABranchWithAHostnameExpires(t *testing.T) {
	h, rec := expiringHarness(t)
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	ctx := context.Background()
	// The node's signing key lives under the system project.
	if err := h.reg.CreateProject(ctx, &registry.Project{Ref: config.SystemRef, Name: "system", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	b := h.create("preview", func(in *CreateInput) { in.NotifyURL = srv.URL })
	quiet := h.create("quiet", nil) // no hostname: nothing to warn about
	if err := h.reg.PutVanitySubdomain(ctx, b.Ref, "preview-one", h.cfg.VanityHost("preview-one")); err != nil {
		t.Fatal(err)
	}

	h.advance(23 * time.Hour) // 25 hours left: outside the 24 hour window
	if res, _ := h.svc.Sweep(ctx, false); len(res.Warned) != 0 {
		t.Fatalf("warned too early: %+v", res)
	}
	h.advance(2 * time.Hour) // 23 hours left
	res, err := h.svc.Sweep(ctx, false)
	if err != nil || len(res.Warned) != 1 || res.Warned[0] != b.Ref {
		t.Fatalf("sweep inside the window: %+v %v", res, err)
	}
	if has(res.Warned, quiet.Ref) {
		t.Fatal("a branch with no hostname was warned about")
	}
	evs, _ := h.reg.ListEvents(ctx, b.Ref, 20)
	var found map[string]any
	for _, e := range evs {
		if e.Kind == "branch.expiring" {
			_ = json.Unmarshal(e.Payload, &found)
		}
	}
	names, _ := found["hostnames"].([]any)
	if found == nil || len(names) != 1 || names[0] != h.cfg.VanityHost("preview-one") {
		t.Fatalf("event payload = %v", found)
	}
	if parent, _ := h.reg.ListEvents(ctx, parentRef, 20); !hasEvent(parent, "branch.expiring") {
		t.Fatal("the parent's events do not carry the warning")
	}
	if rec.n() != 1 {
		t.Fatalf("notify_url received %d posts", rec.n())
	}
	body, hdr := rec.last()
	if body == nil || hdr.Get("webhook-signature") == "" || !strings.HasPrefix(hdr.Get("webhook-id"), "msg_") {
		t.Fatalf("post = %v, headers %v", body, hdr)
	}

	// Once per expiry.
	h.advance(time.Hour)
	if res, _ := h.svc.Sweep(ctx, false); len(res.Warned) != 0 || rec.n() != 1 {
		t.Fatalf("warned again: %+v, posts %d", res, rec.n())
	}
	// A moved expiry is warned about again when it comes close.
	p, _ := h.reg.GetProject(ctx, b.Ref)
	nb := *p.Branch
	later := h.clock().Add(48 * time.Hour)
	nb.ExpiresAt = &later
	if err := h.reg.UpdateBranch(ctx, b.Ref, &nb); err != nil {
		t.Fatal(err)
	}
	h.advance(30 * time.Hour)
	if res, _ := h.svc.Sweep(ctx, false); !has(res.Warned, b.Ref) || rec.n() != 2 {
		t.Fatalf("not warned about the new expiry: %+v, posts %d", res, rec.n())
	}
	// A dry run warns nobody.
	if res, _ := h.svc.Sweep(ctx, true); len(res.Warned) != 0 {
		t.Fatalf("dry run warned: %+v", res)
	}
}

func TestPersistentBranchIsNotWarned(t *testing.T) {
	h, _ := expiringHarness(t)
	ctx := context.Background()
	b := h.create("keep", func(in *CreateInput) { in.Persistent = true })
	if err := h.reg.PutVanitySubdomain(ctx, b.Ref, "keep-one", h.cfg.VanityHost("keep-one")); err != nil {
		t.Fatal(err)
	}
	h.advance(47 * time.Hour)
	if res, _ := h.svc.Sweep(ctx, false); len(res.Warned) != 0 {
		t.Fatalf("a persistent branch was warned about: %+v", res)
	}
}

func hasEvent(evs []registry.Event, kind string) bool {
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}
