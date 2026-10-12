package branching

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// repoStub stands in for the repository a pull request's branch is built from: its schema is the
// parent's migrations (the fake database keeps no real schema), and it deploys the functions
// "hello", which the parent has too, and "fresh", which it does not.
type repoStub struct{ c *contentHarness }

func (r repoStub) Schema(_ context.Context, _ *Run, ref string) (string, error) {
	r.c.db.mu.Lock()
	r.c.db.migs[ref] = append([]Migration(nil), r.c.db.migs[parentRef]...)
	r.c.db.mu.Unlock()
	return "built from the repository", nil
}

func (r repoStub) Functions(_ context.Context, _ *Run, ref string) error {
	r.c.fns.put(ref, "hello", "d-pr", 1)
	r.c.fns.put(ref, "fresh", "f1", 1)
	return nil
}

// The Edge Functions of a branch a repository builds are the repository's, and the base a push and a
// merge compare with is the parent's functions at creation, as for a branch that copied them.
func TestABranchBuiltFromARepositoryPushesAndMergesFunctionsAgainstTheParentsAtCreation(t *testing.T) {
	c := newContentHarness(t, nil)
	c.fns.put(parentRef, "hello", "d-hello", 3)
	c.fns.put(parentRef, "other", "d-other", 1)
	b := c.create("pr", func(in *CreateInput) { in.SchemaSource = repoStub{c} })
	c.mustState(b, registry.BranchFunctionsDeployed)

	base, _ := registry.BranchContentOf(c.reg).GetBranchFunctionBase(context.Background(), b.Ref)
	if want := map[string]string{"hello": "d-hello", "other": "d-other"}; !reflect.DeepEqual(base, want) {
		t.Fatalf("function base = %v, want the parent's functions at creation %v", base, want)
	}
	if got, _ := c.fns.Digests(context.Background(), b.Ref); !reflect.DeepEqual(got, map[string]string{"hello": "d-pr", "fresh": "f1"}) {
		t.Fatalf("the branch's functions are not the repository's: %v", got)
	}

	// Update branch: the pull request's hello stays; the parent's function the repository lacks comes in.
	if _, err := c.svc.Push(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := c.wait(b.Ref)
	c.mustState(got, registry.BranchFunctionsDeployed)
	d, _ := c.fns.Digests(context.Background(), b.Ref)
	if want := map[string]string{"hello": "d-pr", "fresh": "f1", "other": "d-other"}; !reflect.DeepEqual(d, want) {
		t.Fatalf("branch functions after the push = %v, want %v", d, want)
	}
	if !strings.Contains(got.Detail, "kept the branch's version of hello") || strings.Contains(got.Detail, "overwrote") {
		t.Fatalf("push detail = %q", got.Detail)
	}

	// Merge: the pull request's change is the branch's, so it deploys without force.
	if _, err := c.svc.Merge(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got = c.wait(b.Ref)
	c.mustState(got, registry.BranchFunctionsDeployed)
	pd, _ := c.fns.Digests(context.Background(), parentRef)
	if want := map[string]string{"hello": "d-pr", "other": "d-other", "fresh": "f1"}; !reflect.DeepEqual(pd, want) {
		t.Fatalf("parent functions after the merge = %v, want %v", pd, want)
	}
}

// A function the parent changed after the branch began is the parent's change: with the pull request's
// change to the same function it is a conflict, as for any branch.
func TestAFunctionBothSidesChangedOnARepositoryBranchIsAConflict(t *testing.T) {
	c := newContentHarness(t, nil)
	c.fns.put(parentRef, "hello", "d-hello", 3)
	b := c.create("pr", func(in *CreateInput) { in.SchemaSource = repoStub{c} })
	c.fns.put(parentRef, "hello", "d-hello-2", 4)
	if _, err := c.svc.Merge(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	got := c.wait(b.Ref)
	c.mustState(got, registry.BranchMigrationsFailed)
	if !strings.Contains(got.Detail, "both changed the Edge Function(s) hello") {
		t.Fatalf("detail = %q", got.Detail)
	}
	// Forced, the pull request's version is deployed.
	if _, err := c.svc.Merge(context.Background(), b.Ref, ActionInput{Force: true}); err != nil {
		t.Fatal(err)
	}
	c.mustState(c.wait(b.Ref), registry.BranchFunctionsDeployed)
	if d, _ := c.fns.Digests(context.Background(), parentRef); d["hello"] != "d-pr" {
		t.Fatalf("parent hello = %q", d["hello"])
	}
}

func linkOf(t *testing.T, c *contentHarness, ref string, seeded bool) {
	t.Helper()
	l := &registry.GitHubBranchLink{BranchRef: ref, ProjectRef: parentRef, RepositoryID: 1, RepositoryName: "o/r", InstallationID: 1, PRNumber: 7, HeadRef: "feature", HeadSHA: "abc"}
	if seeded {
		now := time.Now()
		l.SeededAt = &now
	}
	if err := registry.GitHubOf(c.reg).PutGitHubBranchLink(context.Background(), l); err != nil {
		t.Fatal(err)
	}
}

// A reset of a pull request's preview branch replays the branch's own history: the parent's stored seed
// is not the repository's and is not applied, and the link forgets that the repository's seed was
// applied, so that the next commit applies it again.
func TestResetOfARepositoryBranchAppliesNoParentSeedAndForgetsTheRepositorysSeed(t *testing.T) {
	c := newContentHarness(t, nil)
	if err := c.svc.SetSeed(context.Background(), parentRef, "insert into t values (1)"); err != nil {
		t.Fatal(err)
	}
	c.eng.onDelete = func(ref string, _ lifecycle.DeleteOptions) { c.harness.db.forget(ref) }

	pr := c.create("pr", func(in *CreateInput) { in.SchemaSource = repoStub{c} })
	linkOf(t, c, pr.Ref, true)
	plain := c.create("plain", nil)
	if len(c.db.scripts[plain.Ref]) != 1 {
		t.Fatalf("a plain branch was not seeded with the parent's seed: %v", c.db.scripts[plain.Ref])
	}

	if _, err := c.svc.Reset(context.Background(), pr.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	c.mustState(c.wait(pr.Ref), registry.BranchFunctionsDeployed)
	if len(c.db.scripts[pr.Ref]) != 0 {
		t.Fatalf("the parent's seed ran on a repository's branch: %v", c.db.scripts[pr.Ref])
	}
	l, err := registry.GitHubOf(c.reg).GetGitHubBranchLink(context.Background(), pr.Ref)
	if err != nil || l.SeededAt != nil {
		t.Fatalf("link after the reset: %+v, %v", l, err)
	}
	if log := c.runLog(pr.Ref); !strings.Contains(log, "the next commit to the pull request applies the repository's seed again") {
		t.Fatalf("run log:\n%s", log)
	}

	// A branch with no link is reset as before: the parent's seed runs again, and nothing is cleared.
	if _, err := c.svc.Reset(context.Background(), plain.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	c.mustState(c.wait(plain.Ref), registry.BranchFunctionsDeployed)
	if len(c.db.scripts[plain.Ref]) != 1 {
		t.Fatalf("a reset of a plain branch did not apply the parent's seed: %v", c.db.scripts[plain.Ref])
	}
}

// The link does not say who made the branch. One made by hand before the pull request (it got the
// parent's seed when it was created) is reset like a preview branch once a pull request follows it:
// no seed of the parent, and the next commit applies the repository's. The docs say so.
func TestAResetOfABranchMadeByHandThatAPullRequestFollowsIsResetLikeAPreviewBranch(t *testing.T) {
	c := newContentHarness(t, nil)
	if err := c.svc.SetSeed(context.Background(), parentRef, "insert into t values (1)"); err != nil {
		t.Fatal(err)
	}
	c.eng.onDelete = func(ref string, _ lifecycle.DeleteOptions) { c.harness.db.forget(ref) }
	hand := c.create("hand", nil)
	if len(c.db.scripts[hand.Ref]) != 1 {
		t.Fatalf("the branch was not seeded with the parent's seed when it was made: %v", c.db.scripts[hand.Ref])
	}
	linkOf(t, c, hand.Ref, true) // adopted by a pull request: marked seeded, because it has its own data

	if _, err := c.svc.Reset(context.Background(), hand.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	c.mustState(c.wait(hand.Ref), registry.BranchFunctionsDeployed)
	if len(c.db.scripts[hand.Ref]) != 0 {
		t.Fatalf("the parent's seed ran on a branch a pull request follows: %v", c.db.scripts[hand.Ref])
	}
	if l, err := registry.GitHubOf(c.reg).GetGitHubBranchLink(context.Background(), hand.Ref); err != nil || l.SeededAt != nil {
		t.Fatalf("link after the reset: %+v, %v", l, err)
	}
}

// A failed reset leaves the record alone: the seed is forgotten only for a branch that was rebuilt.
func TestAFailedResetOfARepositoryBranchKeepsTheRecordOfItsSeed(t *testing.T) {
	c := newContentHarness(t, nil)
	c.eng.onDelete = func(ref string, _ lifecycle.DeleteOptions) { c.harness.db.forget(ref) }
	pr := c.create("pr", func(in *CreateInput) { in.SchemaSource = repoStub{c} })
	linkOf(t, c, pr.Ref, true)
	c.db.mu.Lock()
	c.db.applyErr[pr.Ref] = errFake("migration failed")
	c.db.mu.Unlock()
	if _, err := c.svc.Reset(context.Background(), pr.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	if got := c.wait(pr.Ref); got.State != registry.BranchMigrationsFailed {
		t.Fatalf("state = %s", got.State)
	}
	if l, _ := registry.GitHubOf(c.reg).GetGitHubBranchLink(context.Background(), pr.Ref); l == nil || l.SeededAt == nil {
		t.Fatalf("a failed reset cleared the record of the seed: %+v", l)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
