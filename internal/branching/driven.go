package branching

import (
	"context"

	"github.com/supavise/supavise/internal/registry"
)

// Drive starts an operation on an existing branch whose work is done by fn: the action run, the
// events, the branch's status and its notify_url follow the operation like those of a push or a
// reset, and the call returns the run's id as soon as the operation is registered. The Git
// integration applies the commit of a pull request this way. The branch must be at rest and
// running; a second operation on it is refused while this one lasts.
func (s *Service) Drive(ctx context.Context, idOrRef, op string, fn Driver) (string, error) {
	b, err := s.resolveBranch(ctx, idOrRef, op)
	if err != nil {
		return "", err
	}
	if err := s.checkIdle(ctx, b); err != nil {
		return "", err
	}
	if err := s.activeBranch(ctx, b); err != nil {
		return "", err
	}
	r, err := s.beginRun(ctx, b.Ref, op, b.Ref, b.ParentRef)
	if err != nil {
		return "", err
	}
	s.setState(ctx, b.Ref, registry.BranchRunningMigration, "applying changes from the repository", nil)
	s.spawn(r, b.Ref, b.ParentRef, func(ctx context.Context) (string, error) { return fn(ctx, runFrom(ctx)) })
	return r.id, nil
}

// Database returns what the service runs SQL in project databases with: the Database it was given,
// or pgx through the engine. The Git integration applies a repository's migrations through it.
func (s *Service) Database() Database { return s.db }
