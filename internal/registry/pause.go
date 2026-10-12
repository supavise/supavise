package registry

import (
	"context"
	"time"
)

// PauseIdle is the reason of a pause the node made because a branch was not used. The first HTTP
// request or database connection to a branch paused for it wakes the branch; a pause with no
// reason (the owner's) never wakes by itself.
const PauseIdle = "idle"

// Pause says why and since when a project is paused (migration 1520).
type Pause struct {
	Ref string
	// Reason is "" for a pause the owner asked for and PauseIdle for one the node made.
	Reason   string
	PausedAt time.Time
}

// ExpiryWarning is the record of a warning about a branch's expiry.
type ExpiryWarning struct {
	// For is the expiry the warning was about; a branch whose expiry moved is warned again.
	For time.Time
	At  time.Time
}

// LifecycleStore keeps the lifecycle facts that the project row does not: why a project is paused
// and which expiry warnings were sent. Both registries implement it; a caller that finds none
// (BranchLifecycleOf returns nil) treats every pause as the owner's and sends every warning.
type LifecycleStore interface {
	// SetPause records that ref is paused, with its reason, replacing an earlier record. The
	// lifecycle engine writes it before it sets the status INACTIVE. ErrNotFound for an unknown
	// project.
	SetPause(ctx context.Context, ref, reason string, at time.Time) error
	// ClearPause forgets the pause of ref; nothing recorded is not an error.
	ClearPause(ctx context.Context, ref string) error
	// GetPause returns the pause of ref, or ErrNotFound when none is recorded.
	GetPause(ctx context.Context, ref string) (*Pause, error)
	// ListPauses returns every recorded pause.
	ListPauses(ctx context.Context) ([]Pause, error)
	// GetExpiryWarning returns the warning sent for ref, or ErrNotFound when none was.
	GetExpiryWarning(ctx context.Context, ref string) (*ExpiryWarning, error)
	// SetExpiryWarning records a warning about the expiry `for` of ref. ErrNotFound for an unknown
	// project.
	SetExpiryWarning(ctx context.Context, ref string, forExpiry, at time.Time) error
}

// BranchLifecycleOf returns reg's LifecycleStore, or nil when it has none.
func BranchLifecycleOf(reg Registry) LifecycleStore {
	s, _ := reg.(LifecycleStore)
	return s
}

var (
	_ LifecycleStore = (*Memory)(nil)
	_ LifecycleStore = (*Postgres)(nil)
)
