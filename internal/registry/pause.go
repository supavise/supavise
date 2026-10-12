package registry

import (
	"context"
	"time"
)

// PauseIdle is the reason of a pause the node made because a branch was not used. The first HTTP
// request or database connection to a branch paused for it wakes the branch, and so does an operation
// that needs the branch to run (a merge, a push, a diff, a reset, the Git integration's commit). It is
// the only reason that wakes by itself.
const PauseIdle = "idle"

// PauseQuarantine is the reason of a stop the node made because a branch could not be detached from its
// parent: it still carries the parent's database passwords, subscriptions, foreign servers or keys. Nothing
// wakes it by itself, a request, a database connection and a commit of the repository included; the
// owner resumes it on purpose, or resets or deletes the branch.
const PauseQuarantine = "quarantine"

// WakesByItself reports whether a pause with this reason ends when something uses the project: only the
// node's pause for idleness does. The owner's pause (the empty reason, and what a project paused before
// the reason was recorded counts as) and a quarantine wait for a person.
func WakesByItself(reason string) bool { return reason == PauseIdle }

// Pause says why and since when a project is paused (migration 1520).
type Pause struct {
	Ref string
	// Reason is "" for a pause the owner asked for, PauseIdle for one the node made because the branch
	// was not used, and PauseQuarantine for a stop the node made because the branch carries the
	// parent's credentials.
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
