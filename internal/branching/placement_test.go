package branching

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/registry"
)

// moveParent homes the parent on a second server and makes the service run on the first.
func moveParent(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	n2 := &registry.Node{Name: "eu-follower", State: registry.NodeActive}
	if err := h.reg.CreateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	cl, err := h.reg.GetCluster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.reg.SetProjectNode(ctx, parentRef, n2.ID, cl.Epoch); err != nil {
		t.Fatal(err)
	}
	h.svc.nodeID = cl.Leader
}

// A branch with data copies the parent's data directory, so a parent homed on another server refuses it
// with a message that names the server and what to do, before anything exists. A schema-only branch only
// replays migrations and works wherever the parent is.
func TestWithDataRefusesAParentOnAnotherServer(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	moveParent(t, h)

	_, err := h.svc.Create(ctx, parentRef, CreateInput{Name: "data", WithData: true})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("create with data: %v", err)
	}
	for _, want := range []string{"eu-follower", parentRef, "without data", "supavise failover " + parentRef} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message %q lacks %q", err, want)
		}
	}
	if len(h.eng.creates) != 0 {
		t.Fatalf("something was created before the refusal: %d", len(h.eng.creates))
	}
	if _, err := h.svc.Get(ctx, parentRef, "data"); err == nil {
		t.Fatal("a branch exists after the refusal")
	}

	// Schema only is fine, and so is a parent on this server.
	b := h.create("schema", nil)
	if b.WithData {
		t.Fatalf("branch = %+v", b)
	}
	h.svc.nodeID = ""
	if err := h.svc.checkParentHere(parentRef); err != nil {
		t.Fatalf("a service outside a cluster refused: %v", err)
	}
}

// A reset with data checks the same thing before it removes the old cluster.
func TestResetWithDataRefusesWhenTheParentIsOnAnotherServer(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	b := h.create("a", nil)
	parent, _ := h.reg.GetProject(ctx, parentRef)
	old, _ := h.reg.GetProject(ctx, b.Ref)
	withData := *b
	withData.WithData = true
	if err := h.svc.preflightReset(ctx, &withData, parent, old); err != nil && !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrConflict) && !strings.Contains(err.Error(), "backup") {
		t.Fatalf("a reset on the parent's own server: %v", err)
	}
	moveParent(t, h)
	err := h.svc.preflightReset(ctx, &withData, parent, old)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "eu-follower") {
		t.Fatalf("reset with data from a parent on another server: %v", err)
	}
	// A schema-only branch replays migrations and does not care.
	if err := h.svc.preflightReset(ctx, b, parent, old); err != nil {
		t.Fatalf("reset of a schema-only branch: %v", err)
	}
}
