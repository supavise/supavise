package branching

import (
	"context"
	"errors"
	"strings"

	"github.com/supavise/supavise/internal/registry"
)

// UpdateInput is the body of PATCH /v1/branches/{id}. A nil field is left alone.
type UpdateInput struct {
	Name       *string
	GitBranch  *string
	Persistent *bool
	NotifyURL  *string
	// RequestReview true marks the branch as ready for review (Studio's merge request list);
	// false clears the mark. A successful merge of the branch clears it too.
	RequestReview *bool
	// State overrides the branch's status (the six values of the Management API): a CI job that
	// runs the branch's checks reports its result this way.
	State *registry.BranchState
}

// validBranchStates are the statuses PATCH may set.
var validBranchStates = map[registry.BranchState]bool{
	registry.BranchCreatingProject: true, registry.BranchRunningMigration: true,
	registry.BranchMigrationsPassed: true, registry.BranchMigrationsFailed: true,
	registry.BranchFunctionsDeployed: true, registry.BranchFunctionsFailed: true,
}

// Update changes the settings of a branch. Making a branch persistent clears its expiry;
// making it non-persistent starts the default lifetime. On the default branch only GitBranch
// (the Git branch it tracks, hosted's "Production branch") can change.
func (s *Service) Update(ctx context.Context, idOrRef string, in UpdateInput) (*Branch, error) {
	b, err := s.Resolve(ctx, idOrRef)
	if err != nil {
		return nil, err
	}
	if b.IsDefault {
		return s.updateDefault(ctx, b, in)
	}
	p, err := s.project(ctx, b.Ref)
	if err != nil {
		return nil, err
	}
	nb := *p.Branch
	if in.Name != nil && *in.Name != nb.Name {
		if err := ValidName(*in.Name); err != nil {
			return nil, err
		}
		nb.Name = *in.Name
	}
	if in.GitBranch != nil {
		nb.GitBranch = *in.GitBranch
	}
	if in.NotifyURL != nil {
		if *in.NotifyURL != "" {
			if err := validNotifyURL(*in.NotifyURL); err != nil {
				return nil, err
			}
		}
		nb.NotifyURL = *in.NotifyURL
	}
	if in.Persistent != nil && *in.Persistent != nb.Persistent {
		nb.Persistent = *in.Persistent
		if nb.Persistent {
			nb.ExpiresAt = nil
		} else {
			nb.ExpiresAt = s.expiry(0)
		}
	}
	if in.RequestReview != nil {
		if *in.RequestReview {
			now := s.now().UTC()
			nb.ReviewRequestedAt = &now
		} else {
			nb.ReviewRequestedAt = nil
		}
	}
	if in.State != nil && *in.State != nb.State {
		if !validBranchStates[*in.State] {
			return nil, invalid("status %q is not one of the branch statuses", string(*in.State))
		}
		// An operation running here owns the status; the sweeper recovers one that is stuck.
		if op, ok := s.running(b.Ref); ok {
			return nil, conflict("branch %s is busy with %s; its status follows the operation", b.Name, op)
		}
		nb.State, nb.Detail = *in.State, "status set to "+string(*in.State)+" through the API"
	}
	if err := s.reg.UpdateBranch(ctx, b.Ref, &nb); err != nil {
		if errors.Is(err, registry.ErrConflict) {
			return nil, conflict("project %s already has a branch named %q", b.ParentRef, nb.Name)
		}
		return nil, err
	}
	if nb.Name != p.Name {
		if q, err := s.reg.GetProject(ctx, b.Ref); err == nil && q.Name != nb.Name {
			q.Name = nb.Name
			_ = s.reg.UpdateProject(ctx, q)
		}
	}
	return s.Resolve(ctx, b.Ref)
}

// updateDefault is Update for the default branch, which is the project itself: it has a Git
// branch to track and nothing else to change. A request that asks for more is refused with the
// reason, except what Studio's Production branch form sends alongside the Git branch (a name,
// which stays "main").
func (s *Service) updateDefault(ctx context.Context, b *Branch, in UpdateInput) (*Branch, error) {
	switch {
	case in.Name != nil && *in.Name != b.Name && in.GitBranch == nil:
		// A bare rename. Studio's form sends the Git branch's name as branch_name together with git_branch.
		return nil, invalid("the default branch of project %s cannot be renamed", b.Ref)
	case in.Persistent != nil && !*in.Persistent:
		return nil, invalid("the default branch of project %s is always persistent", b.Ref)
	case in.NotifyURL != nil && *in.NotifyURL != "":
		return nil, invalid("the default branch of project %s has no notify_url: it is the project itself", b.Ref)
	case in.RequestReview != nil && *in.RequestReview:
		return nil, invalid("the default branch of project %s cannot be put up for review: other branches merge into it", b.Ref)
	case in.State != nil && *in.State != b.State:
		return nil, invalid("the status of the default branch of project %s follows the merges into it", b.Ref)
	}
	if in.GitBranch != nil {
		st := s.runStore()
		if st == nil {
			return nil, invalid("this node cannot store the Git branch of the default branch")
		}
		if err := st.SetDefaultBranchGit(ctx, b.Ref, strings.TrimSpace(*in.GitBranch)); err != nil {
			if errors.Is(err, registry.ErrNotFound) {
				return nil, ErrNotFound
			}
			return nil, err
		}
	}
	return s.Resolve(ctx, b.Ref)
}

// createDefault is Create with is_default: the default branch exists (it is the project), so the
// answer is that branch, again and again. A Git branch in the request is stored as the one the
// default branch tracks. Nothing is created, and the name in the request is not used.
func (s *Service) createDefault(ctx context.Context, ref string, in CreateInput) (*Branch, error) {
	p, err := s.Parent(ctx, ref)
	if err != nil {
		return nil, err
	}
	if in.GitBranch != "" {
		if _, err := s.updateDefault(ctx, defaultBranchOf(p), UpdateInput{GitBranch: &in.GitBranch}); err != nil {
			return nil, err
		}
	}
	return s.defaultBranch(ctx, p), nil
}

// defaultBranch is the default branch of a project as the Management API shows it: the Git branch
// it tracks and a status that follows the merges into it. A failed merge reads MIGRATIONS_FAILED
// (FUNCTIONS_FAILED when only its deploy step failed) and the reason is its detail; a merge in
// progress reads RUNNING_MIGRATIONS; the status is otherwise FUNCTIONS_DEPLOYED after a merge and
// MIGRATIONS_PASSED before the first.
func (s *Service) defaultBranch(ctx context.Context, p *registry.Project) *Branch {
	b := defaultBranchOf(p)
	st := s.runStore()
	if st == nil {
		return b
	}
	if g, err := st.GetDefaultBranchGit(ctx, p.Ref); err == nil {
		b.GitBranch = g
	}
	// Merges are the only runs the default branch owns.
	runs, _, err := st.ListBranchRuns(ctx, p.Ref, 0, 1)
	if err != nil || len(runs) == 0 {
		return b
	}
	switch last := runs[0]; last.Status {
	case registry.RunRunning:
		b.State, b.Detail = registry.BranchRunningMigration, "merging "+last.BranchRef
	case registry.RunFailed:
		b.State, b.Detail = registry.BranchMigrationsFailed, last.Failure
		if strings.HasPrefix(last.Failure, StepDeploy+":") {
			b.State = registry.BranchFunctionsFailed
		}
	case registry.RunSuccess:
		b.State = registry.BranchFunctionsDeployed
	}
	return b
}
