package branching

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/supavise/supavise/internal/branching/schemadiff"
	"github.com/supavise/supavise/internal/registry"
)

// DiffOptions are the query parameters of GET /v1/branches/{id}/diff.
type DiffOptions struct {
	// IncludedSchemas is a comma separated list of schemas ("public,auth"). Empty compares the user
	// schemas, plus the policies and triggers users added to the tables of the managed ones.
	IncludedSchemas string
	// PGDelta is the `pgdelta` parameter. Hosted offers two engines (Migra and pg-delta); this node
	// has one, so both values give the same answer.
	PGDelta bool
}

func (o DiffOptions) read() schemadiff.ReadOptions {
	var ro schemadiff.ReadOptions
	for _, n := range strings.Split(o.IncludedSchemas, ",") {
		if n = strings.TrimSpace(n); n != "" {
			ro.Schemas = append(ro.Schemas, n)
		}
	}
	return ro
}

// Diff is DiffWith with the default options.
func (s *Service) Diff(ctx context.Context, idOrRef string) (string, error) {
	return s.DiffWith(ctx, idOrRef, DiffOptions{})
}

// DiffWith returns, as SQL text, what the branch changed in its schema: the statements that would
// take the parent to the branch's schema where the branch moved away from the schema it started
// with. It is empty when nothing changed (Studio shows "No changes detected" for an empty body). A
// change the engine cannot express is a comment line that begins with schemadiff.UnsupportedMarker.
//
// The comparison is against the schema base recorded when the branch was made and at its last push
// or merge, not against the parent as it is, so that what the parent got meanwhile is not mistaken
// for something the branch removed. A branch with no base (made by an earlier release) is compared
// with the parent as it is, and only additions and changes are listed.
func (s *Service) DiffWith(ctx context.Context, idOrRef string, o DiffOptions) (string, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "diff")
	if err != nil {
		return "", err
	}
	if b, err = s.wakeIfIdle(ctx, b, "diff"); err != nil {
		return "", err
	}
	if err := s.activeBranch(ctx, b); err != nil {
		return "", err
	}
	parent, err := s.activeParent(ctx, b)
	if err != nil {
		return "", err
	}
	res, _, err := s.schemaChanges(ctx, b, parent, o.read())
	if err != nil {
		return "", err
	}
	return res.SQL(), nil
}

// schemaChanges plans the branch's changes and returns the branch's schema it read.
func (s *Service) schemaChanges(ctx context.Context, b *Branch, parent *registry.Project, ro schemadiff.ReadOptions) (*schemadiff.Result, *schemadiff.Snapshot, error) {
	sd := s.schemaDB()
	if sd == nil {
		return nil, nil, errors.New("this node cannot read the schema of a database")
	}
	branchSnap, err := sd.Snapshot(ctx, b.Ref, ro)
	if err != nil {
		return nil, nil, fmt.Errorf("read the schema of the branch: %w", err)
	}
	base, ok, err := s.getBase(ctx, b.Ref)
	if err != nil {
		return nil, nil, err
	}
	if ok && (len(ro.Schemas) == 0 || base.Covers(ro.Schemas)) {
		if len(ro.Schemas) > 0 {
			base = base.Restrict(ro.Schemas)
		}
		return schemadiff.Diff(base, branchSnap, schemadiff.Options{}), branchSnap, nil
	}
	parentSnap, err := sd.Snapshot(ctx, parent.Ref, ro)
	if err != nil {
		return nil, nil, fmt.Errorf("read the schema of the parent: %w", err)
	}
	return schemadiff.Diff(parentSnap, branchSnap, schemadiff.Options{KeepRemoved: true}), branchSnap, nil
}
