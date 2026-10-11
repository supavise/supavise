package branching

import (
	"context"
	"errors"

	"github.com/supavise/supavise/internal/registry"
)

// Update is PATCH /v1/branches/{id}.
type UpdateInput struct {
	Name          *string
	GitBranch     *string
	Persistent    *bool
	NotifyURL     *string
	RequestReview *bool
}

// Update changes the settings of a branch. Making a branch persistent clears its expiry;
// making it non-persistent starts the default lifetime.
func (s *Service) Update(ctx context.Context, idOrRef string, in UpdateInput) (*Branch, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "update")
	if err != nil {
		return nil, err
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
	if in.RequestReview != nil && *in.RequestReview {
		now := s.now().UTC()
		nb.ReviewRequestedAt = &now
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
