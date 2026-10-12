package branching

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/supavise/supavise/internal/registry"
)

// FunctionCarrier is the part of the Edge Function store that branching uses to carry functions
// between a branch and its parent. internal/api implements it over the API's store
// (branches_functions.go); Deps.Functions may hold one. A Deps.Functions that is only a
// FunctionSource lets a merge read digests and nothing more.
type FunctionCarrier interface {
	FunctionSource
	// Copy copies the deployments of the slugs (all of from's when slugs is nil) from one project to
	// another, with their files, and returns the digest of each. A function that is new to the target
	// gets a new id and keeps the source's version, update time and digest; one the target has keeps
	// its id and creation time and gets a newer version. The runtime is told with Applied.
	Copy(ctx context.Context, from, to string, slugs []string) (map[string]string, error)
	// PutSecrets stores secrets (name to plain value) as the project's Edge Function secrets,
	// replacing those of the same names.
	PutSecrets(ctx context.Context, ref string, secrets map[string]string) error
	// Applied tells the Edge Functions runtime that the functions or secrets of a project changed.
	Applied(ctx context.Context, ref string) error
}

func (s *Service) carrier() FunctionCarrier {
	c, _ := s.fns.(FunctionCarrier)
	return c
}

func (s *Service) functionBase(ctx context.Context, branchRef string) map[string]string {
	st := registry.BranchContentOf(s.reg)
	if st == nil {
		return nil
	}
	m, err := st.GetBranchFunctionBase(ctx, branchRef)
	if err != nil {
		s.log.Warn("could not read the Edge Function base of a branch", "ref", branchRef, "err", err)
		return nil
	}
	return m
}

func (s *Service) putFunctionBase(ctx context.Context, branchRef string, set map[string]string) error {
	st := registry.BranchContentOf(s.reg)
	if st == nil || len(set) == 0 {
		return nil
	}
	return st.PutBranchFunctionBase(ctx, branchRef, set, nil)
}

// carryAtCreate is the deploy step of a new branch: the parent's Edge Functions are copied into it
// (code, settings, entrypoint, import map and static files; the runtime serves them at once) and the
// secrets of the create body become the branch's function secrets. The parent's own secrets are never
// copied: they are per project, as on hosted.
func (s *Service) carryAtCreate(ctx context.Context, parentRef, branchRef string, secretsIn map[string]string) error {
	c := s.carrier()
	if c == nil {
		return nil
	}
	copied, err := c.Copy(ctx, parentRef, branchRef, nil)
	if err != nil {
		return fmt.Errorf("copy the parent's Edge Functions: %w", err)
	}
	if err := s.putFunctionBase(ctx, branchRef, copied); err != nil {
		return fmt.Errorf("record the Edge Functions the branch started with: %w", err)
	}
	changed := len(copied) > 0
	if len(secretsIn) > 0 {
		if err := c.PutSecrets(ctx, branchRef, secretsIn); err != nil {
			return fmt.Errorf("store the branch's Edge Function secrets: %w", err)
		}
		changed = true
		s.logRun(ctx, "stored %d Edge Function secret(s) for the branch (secrets are never copied from the parent)", len(secretsIn))
	}
	if len(copied) > 0 {
		s.logRun(ctx, "copied %d Edge Function(s) from the parent: %s", len(copied), strings.Join(sortedKeys(copied), ", "))
	}
	if changed {
		if err := c.Applied(ctx, branchRef); err != nil {
			return fmt.Errorf("start the branch's Edge Functions: %w", err)
		}
	}
	return nil
}

// serveKeptFunctions is the deploy step of a reset: the branch keeps its Edge Functions and secrets
// (they live in the API's store, not in the cluster that was rebuilt), and the runtime is told to
// serve them again now and not at its next reconcile, which dropped them while the cluster was gone.
func (s *Service) serveKeptFunctions(ctx context.Context, branchRef string) error {
	c := s.carrier()
	if c == nil {
		return nil
	}
	if err := c.Applied(ctx, branchRef); err != nil {
		return fmt.Errorf("start the branch's Edge Functions: %w", err)
	}
	return nil
}

// pushFunctions brings the parent's Edge Functions into the branch: a function the parent has and the
// branch lacks is added, and one both have in another form is overwritten with the parent's, unless
// only the branch changed it. Functions only the branch has are left alone, and nothing is deleted
// (a deletion is done on the branch). It returns what it did, for the run log.
//
// A function with no recorded base is not overwritten unless force is set. The node cannot tell who
// changed it: a branch that an earlier release made has no base at all (v0.3.x copied no functions and
// recorded nothing, so every function such a branch holds is its own work), and a function both sides
// created apart has none either. Overwriting would destroy what the branch deployed, where a push of
// v0.3.x never touched functions. The branch keeps its copy and the run says so; force takes the
// parent's.
func (s *Service) pushFunctions(ctx context.Context, b *Branch, parent *registry.Project, force bool) (string, error) {
	c := s.carrier()
	if c == nil {
		return "", nil
	}
	pd, err := c.Digests(ctx, parent.Ref)
	if err != nil {
		return "", fmt.Errorf("read the parent's Edge Functions: %w", err)
	}
	bd, err := c.Digests(ctx, b.Ref)
	if err != nil {
		return "", fmt.Errorf("read the branch's Edge Functions: %w", err)
	}
	base := s.functionBase(ctx, b.Ref)
	var take, added, updated, kept, unrecorded []string
	synced := map[string]string{}
	for _, slug := range sortedKeys(pd) {
		dp := pd[slug]
		db, has := bd[slug]
		b0, known := base[slug]
		switch {
		case has && db == dp:
			synced[slug] = dp
		case !has:
			take, added = append(take, slug), append(added, slug)
		case known && dp == b0:
			kept = append(kept, slug) // only the branch changed it
		case !known && !force:
			unrecorded = append(unrecorded, slug) // who changed it is not known: the branch's copy stays, and so does the base
		default:
			take = append(take, slug)
			switch {
			case !known:
				updated = append(updated, slug+" (the branch's version is gone; the node had no record of what the branch started with)")
			case db != b0:
				updated = append(updated, slug+" (the branch's own changes are gone)")
			default:
				updated = append(updated, slug)
			}
		}
	}
	if len(take) > 0 {
		copied, err := c.Copy(ctx, parent.Ref, b.Ref, take)
		if err != nil {
			return "", fmt.Errorf("copy the parent's Edge Functions: %w", err)
		}
		for k, v := range copied {
			synced[k] = v
		}
		if err := c.Applied(ctx, b.Ref); err != nil {
			return "", fmt.Errorf("start the branch's Edge Functions: %w", err)
		}
	}
	if err := s.putFunctionBase(ctx, b.Ref, synced); err != nil {
		return "", fmt.Errorf("record the Edge Functions the branch is in step with: %w", err)
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+strings.Join(added, ", "))
	}
	if len(updated) > 0 {
		parts = append(parts, "overwrote "+strings.Join(updated, ", "))
	}
	if len(kept) > 0 {
		parts = append(parts, "kept the branch's version of "+strings.Join(kept, ", ")+" (the parent did not change it)")
	}
	if len(unrecorded) > 0 {
		parts = append(parts, "kept the branch's version of "+strings.Join(unrecorded, ", ")+
			" (the parent has it in another form and the node has no record of what the branch started with, so it cannot tell who changed it; push with force takes the parent's)")
	}
	if len(parts) == 0 {
		return "", nil
	}
	note := "Edge Functions: " + strings.Join(parts, "; ")
	s.logRun(ctx, "%s", note)
	return note, nil
}

// functionMerge is what a merge will do with the Edge Functions: the slugs it deploys to the parent
// and the slugs both sides changed.
type functionMerge struct {
	deploy    []string
	conflicts []string
	// unrecorded are the slugs that both sides have in different forms and that have no base entry: a
	// branch an earlier release made, or a function both sides created apart. Who changed them is not
	// known, so they stop a merge like a conflict does, and force deploys the branch's version of them.
	unrecorded []string
	// synced are the digests both sides will have afterwards, which become the base.
	synced map[string]string
}

// planFunctionMerge works out which of the branch's functions go to the parent. A function the
// branch changed since the base (or created) is deployed; one only the parent changed is left; one
// both changed in different ways is a conflict; one the parent has in another form and the base does not
// list is unrecorded (functionMerge.unrecorded). Nothing is ever deleted from the parent.
func (s *Service) planFunctionMerge(ctx context.Context, b *Branch, parent *registry.Project) (*functionMerge, error) {
	c := s.carrier()
	if c == nil {
		return &functionMerge{}, nil
	}
	bd, err := c.Digests(ctx, b.Ref)
	if err != nil {
		return nil, fmt.Errorf("read the branch's Edge Functions: %w", err)
	}
	pd, err := c.Digests(ctx, parent.Ref)
	if err != nil {
		return nil, fmt.Errorf("read the parent's Edge Functions: %w", err)
	}
	base := s.functionBase(ctx, b.Ref)
	m := &functionMerge{synced: map[string]string{}}
	for _, slug := range sortedKeys(bd) {
		db := bd[slug]
		dp, onParent := pd[slug]
		b0, known := base[slug]
		switch {
		case onParent && dp == db:
			m.synced[slug] = db
		case !onParent:
			if known && db == b0 {
				continue // the parent removed it and the branch did not change it: it stays removed
			}
			m.deploy = append(m.deploy, slug)
		case !known:
			m.unrecorded = append(m.unrecorded, slug)
		default:
			switch {
			case db == b0:
			case dp != b0:
				m.conflicts = append(m.conflicts, slug)
			default:
				m.deploy = append(m.deploy, slug)
			}
		}
	}
	return m, nil
}

// refuse is the error that stops a merge before it runs anything when the plan holds a function both
// sides changed, or one the node cannot tell who changed; nil when force is set or there are none.
// The hint differs: a push takes the parent's version of a function both changed, but it keeps the
// branch's version of one with no base, so for that one only force moves it.
func (m *functionMerge) refuse(force bool) error {
	if force || (len(m.conflicts) == 0 && len(m.unrecorded) == 0) {
		return nil
	}
	var parts []string
	if len(m.conflicts) > 0 {
		parts = append(parts, fmt.Sprintf("the branch and the parent both changed the Edge Function(s) %s; update the branch (push) to take the parent's version, or use force to deploy the branch's version",
			strings.Join(m.conflicts, ", ")))
	}
	if len(m.unrecorded) > 0 {
		parts = append(parts, fmt.Sprintf("the branch and the parent have the Edge Function(s) %s in different forms, and the node has no record of what the branch started with (it was made by an earlier release), so it cannot tell who changed them; use force to deploy the branch's version, or push with force to take the parent's",
			strings.Join(m.unrecorded, ", ")))
	}
	return fmt.Errorf("%w: %s", ErrDiverged, strings.Join(parts, "; "))
}

// deployFunctions runs the plan: the deploy step of a merge. force deploys the branch's version of a
// function both sides changed.
func (s *Service) deployFunctions(ctx context.Context, b *Branch, parent *registry.Project, m *functionMerge, force bool) (string, error) {
	c := s.carrier()
	if c == nil {
		return "", nil
	}
	slugs := append([]string{}, m.deploy...)
	if force {
		slugs = append(slugs, m.conflicts...)
		slugs = append(slugs, m.unrecorded...)
	}
	sort.Strings(slugs)
	if len(slugs) > 0 {
		copied, err := c.Copy(ctx, b.Ref, parent.Ref, slugs)
		if err != nil {
			return "", fmt.Errorf("deploy the branch's Edge Functions to the parent: %w", err)
		}
		for k, v := range copied {
			m.synced[k] = v
		}
		if err := c.Applied(ctx, parent.Ref); err != nil {
			return "", fmt.Errorf("start the parent's Edge Functions: %w", err)
		}
	}
	if err := s.putFunctionBase(ctx, b.Ref, m.synced); err != nil {
		return "", fmt.Errorf("record the Edge Functions both sides have: %w", err)
	}
	if len(slugs) == 0 {
		return "", nil
	}
	note := fmt.Sprintf("deployed %d Edge Function(s) to %s: %s", len(slugs), parent.Ref, strings.Join(slugs, ", "))
	s.logRun(ctx, "%s", note)
	return note, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
