package branching

import (
	"context"
	"fmt"
	"strings"

	"github.com/supavise/supavise/internal/lifecycle"
)

var _ SchemaExposer = (*lifecycle.Engine)(nil)

// SchemaExposer is the optional capability of the Engine behind the schemas PostgREST exposes on a
// branch whose database starts empty. PostgREST does not start on a schema that does not exist, and
// the schemas a parent exposes are made by the migrations, the seed or the repository that the branch
// is built from after its project exists. So such a branch starts on the default schemas, and the list
// is applied once the schemas are built. *lifecycle.Engine has it; an Engine without it leaves the
// branch on the default schemas.
type SchemaExposer interface {
	// SavedExposedSchemas returns the PostgREST db_schema saved for ref, "" for the default.
	SavedExposedSchemas(ctx context.Context, ref string) (string, error)
	// SetExposedSchemas saves list as the db_schema of ref ("" returns it to the default) and does
	// not restart PostgREST.
	SetExposedSchemas(ctx context.Context, ref, list string) error
	// ExposeSchemasThatExist saves the names of list that are schemas of ref's database as its
	// db_schema, restarts PostgREST on them, and returns the names it left out because they are not
	// schemas (yet).
	ExposeSchemasThatExist(ctx context.Context, ref, list string) (exposed, missing []string, err error)
}

// schemaHold is the list of exposed schemas a schema-only branch is made without and gets back once
// its schemas are built.
type schemaHold struct {
	x    SchemaExposer
	list string
	// created is true once the project of a new branch exists: from then on the branch's settings can
	// keep the list (a reset replays the history and looks there).
	created bool
	// done is true when the list was applied (or was not wanted any more).
	done bool
	// exposed and missing are what ExposeSchemasThatExist answered.
	exposed, missing []string
}

// holdSchemas decides which exposed schemas the branch is built without. It runs before the project
// is made or made again, for a branch whose database starts empty:
//
//   - a new branch takes its parent's list; the engine does not copy it into the branch's settings
//     for a project with no seed, so PostgREST first starts on the default schemas;
//   - a reset recreates the cluster empty under the branch's own saved list, which PostgREST could not
//     start on, so the list is taken off the settings for the build.
//
// It returns nil when there is nothing to hold or the Engine cannot. A failure to read the list is
// logged and the branch is built on the default schemas.
func (s *Service) holdSchemas(ctx context.Context, j *createJob) *schemaHold {
	x, ok := s.eng.(SchemaExposer)
	if !ok {
		return nil
	}
	from := j.parent.Ref
	if j.recreate {
		from = j.ref
	}
	list, err := x.SavedExposedSchemas(ctx, from)
	if err != nil {
		s.log.Warn("could not read the exposed schemas a branch is built without", "ref", j.ref, "from", from, "err", err)
		return nil
	}
	if list == "" {
		return nil
	}
	h := &schemaHold{x: x, list: list}
	if j.recreate {
		if err := x.SetExposedSchemas(ctx, j.ref, ""); err != nil {
			s.log.Warn("could not take the exposed schemas off a branch before it is recreated", "ref", j.ref, "err", err)
			return nil
		}
		h.created = true
	}
	s.logRun(ctx, "PostgREST starts on the default schemas; %s is exposed once the schema is built", list)
	return h
}

// exposeHeld applies the held list after the schemas are built: the names that exist, in order. A
// list the build saved itself (a repository's config.toml) is the branch's own and stays. A name that
// is not a schema is left out and said in the run's log: the branch's schema does not have everything
// the parent's has, and the branch is not failed for it.
func (s *Service) exposeHeld(ctx context.Context, j *createJob, h *schemaHold) error {
	if h == nil || h.done {
		return nil
	}
	if cur, err := h.x.SavedExposedSchemas(ctx, j.ref); err == nil && cur != "" {
		h.done = true
		s.logRun(ctx, "the branch exposes %s, set while it was built; the parent's %s is not applied", cur, h.list)
		return nil
	}
	exposed, missing, err := h.x.ExposeSchemasThatExist(ctx, j.ref, h.list)
	if err != nil {
		return fmt.Errorf("expose the parent's schemas through PostgREST: %w", err)
	}
	h.done, h.exposed, h.missing = true, exposed, missing
	switch {
	case len(exposed) > 0:
		s.logRun(ctx, "PostgREST exposes %s", strings.Join(exposed, ", "))
	default:
		s.logRun(ctx, "PostgREST stays on the default schemas")
	}
	if len(missing) > 0 {
		s.logRun(ctx, "not exposed, because the branch has no such schema: %s", strings.Join(missing, ", "))
	}
	return nil
}

// keepHeld runs when the build failed before the list was applied: the list goes back into the settings
// of the branch, so that a reset of the branch exposes it after its schemas are built. A list the build
// saved itself is left alone.
func (s *Service) keepHeld(ctx context.Context, j *createJob, h *schemaHold) {
	if h == nil || h.done || !h.created {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if cur, err := h.x.SavedExposedSchemas(ctx, j.ref); err != nil || cur != "" {
		return
	}
	if err := h.x.SetExposedSchemas(ctx, j.ref, h.list); err != nil {
		s.log.Warn("could not keep the exposed schemas of a branch whose build failed", "ref", j.ref, "schemas", h.list, "err", err)
	}
}
