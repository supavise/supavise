package registry

import (
	"context"
	"time"
)

// MaxAdvisorExceptions is how many rules a project may hold.
const MaxAdvisorExceptions = 500

// AdvisorException is a rule of the Advisor Rules page: lints it selects are hidden from the
// advisors (see internal/advisors.Exception for the selector semantics). Empty strings stand for
// the null of the Management API.
type AdvisorException struct {
	ID            string
	Ref           string
	LintCategory  string
	LintName      string
	LintMetadata  map[string]any
	AssignedTo    string
	IsDisabled    bool
	LastUpdatedBy string
	Note          string
	InsertedAt    time.Time
}

// AdvisorExceptionPatch changes the fields of a rule that are non-nil.
type AdvisorExceptionPatch struct {
	LintCategory  *string
	LintName      *string
	LintMetadata  map[string]any // non-nil replaces
	AssignedTo    *string
	IsDisabled    *bool
	Note          *string
	LastUpdatedBy string
}

// AdvisorStore is the storage of advisor rules. Both registries implement it; it is a separate
// interface so that code that does not touch the advisors does not need it.
type AdvisorStore interface {
	// ListAdvisorExceptions returns the project's rules, oldest first.
	ListAdvisorExceptions(ctx context.Context, ref string) ([]AdvisorException, error)
	// CreateAdvisorExceptions adds rules and returns them with their ids. ErrNotFound for an
	// unknown project; ErrConflict past MaxAdvisorExceptions.
	CreateAdvisorExceptions(ctx context.Context, ref string, in []AdvisorException) ([]AdvisorException, error)
	// UpdateAdvisorException changes one rule; ErrNotFound when the project has none with that id.
	UpdateAdvisorException(ctx context.Context, ref, id string, patch AdvisorExceptionPatch) error
	// DeleteAdvisorExceptions removes the rules with these ids and returns how many existed.
	DeleteAdvisorExceptions(ctx context.Context, ref string, ids []string) (int, error)
}
