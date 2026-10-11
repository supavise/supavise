package placement

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/registry"
)

// The Data API statements run in the project's database on its home node: the leader's own plane for
// a project homed there, the agent of the other node, over the plane path, for one homed elsewhere.
func TestRouterApplyDataAPIFollowsTheHome(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMemory()
	mustCreate(t, reg, "second")
	a, b := testRef, "bcdefghijklmnopqrstu"
	for _, ref := range []string{a, b} {
		if err := reg.CreateProject(ctx, &registry.Project{Ref: ref, Name: ref}); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.SetProjectNode(ctx, b, "n2", 1); err != nil {
		t.Fatal(err)
	}
	leader, follower := newFakeLocal(), newFakeLocal()
	mux := mesh.NewMux()
	if err := Register(mux.Handle, HandlerDeps{
		Agent: &recordingAgent{}, Plane: follower, Resolver: RegistryResolver{Reg: reg}, Members: members("n2", "n1", 5),
	}); err != nil {
		t.Fatal(err)
	}
	rpc := &muxRPC{mux: mux, caller: "n1"}
	r := NewRouter(RouterOptions{Local: leader, Resolver: RegistryResolver{Reg: reg}, Self: func() string { return "n1" }, RPC: rpc, Epoch: func() int64 { return 5 }})

	pa, pb := testProject("n1"), testProject("n2")
	pa.Ref, pb.Ref = a, b
	setup := lifecycle.DataAPISetup{RevokeDefaultPrivileges: true, APISchema: true}
	if err := r.ApplyDataAPI(ctx, pa, setup); err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyDataAPI(ctx, pb, lifecycle.DataAPISetup{RevokeDefaultPrivileges: true}); err != nil {
		t.Fatal(err)
	}
	if !leader.has("ApplyDataAPI "+a+" revoke=true api=true") || leader.has("ApplyDataAPI "+b+" revoke=true api=false") {
		t.Fatalf("the leader's plane saw %v", leader.calls)
	}
	if !follower.has("ApplyDataAPI "+b+" revoke=true api=false") || follower.has("ApplyDataAPI "+a+" revoke=true api=true") {
		t.Fatalf("n2's plane saw %v", follower.calls)
	}
	if got, want := rpc.callLog(), "POST /peer/v1/projects/"+b+"/plane/apply_data_api"; got != want {
		t.Fatalf("mesh calls:\n%s\nwant\n%s", got, want)
	}

	// A refusal on the home comes back as itself, and a project the agent does not serve is refused.
	boom := errors.New("cluster in recovery")
	follower.err["ApplyDataAPI"] = boom
	if err := r.ApplyDataAPI(ctx, pb, setup); err == nil || !strings.Contains(err.Error(), boom.Error()) {
		t.Fatalf("an error of the home node: %v", err)
	}
}
