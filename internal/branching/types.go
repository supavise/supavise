package branching

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// DefaultBranchName is the name the Management API gives the branch that is the project
// itself. The parent project is reported as a default branch so that clients that look
// for one (Studio, the CLI) find it; it cannot be created, renamed or deleted.
const DefaultBranchName = "main"

// SchemaSource builds a schema-only branch from something other than its parent: the repository a
// pull request belongs to (internal/github). Hosted builds the branch of a pull request from the
// repository's migrations, seed and functions alone, never from the parent's schema, and this is
// that mode. Create calls the source in place of the replay of the parent's migrations and seed
// and of the copy of the parent's Edge Functions; the branch's project, its keys and its settings
// are made as for any branch.
type SchemaSource interface {
	// Schema runs once the branch's database is healthy. It reports to run (the steps clone,
	// configure, migrate and seed; the run is nil-safe) and returns the detail recorded on the branch.
	Schema(ctx context.Context, run *Run, ref string) (detail string, err error)
	// Functions runs in the deploy step and deploys the branch's Edge Functions.
	Functions(ctx context.Context, run *Run, ref string) error
}

// Driver is the work of an operation that something other than the service does: the runner of
// a Git integration applying a commit to its branch. It reports to run and returns the detail
// recorded on the branch.
type Driver func(ctx context.Context, run *Run) (detail string, err error)

// Branch is the service-level view of a branch: registry fields plus the project's status.
type Branch struct {
	ID        string
	Name      string
	Ref       string // the branch's own project ref
	ParentRef string
	IsDefault bool
	GitBranch string

	// Region is the region of the branch's project, which is its parent's.
	Region string
	// RunID is the action run that Create started for the branch, "" on a branch read later (list
	// the runs of a branch with Service.BranchRuns).
	RunID string

	Persistent bool
	WithData   bool
	// State is the Management API's branch status: it follows the last long operation.
	State registry.BranchState
	// ProjectStatus is the status of the branch's project (its units).
	ProjectStatus registry.Status
	// CloneMethod is how the data was obtained: schema, clonefile, reflink or base-backup.
	CloneMethod string
	// Detail is the outcome of the last operation, an error text after a failure.
	Detail string
	// Egress is the outbound network policy of a branch with data (the registry.Egress*
	// values): "denied" (the branch's Postgres unit reaches loopback only), "pending" (denied
	// once isolation finishes), "allowed" (created with the opt-out) or "unenforced" (denial
	// wanted, but the supervisor cannot confine a unit's network). Empty for schema-only
	// branches and for the default branch.
	Egress string

	CreatedAt           time.Time
	UpdatedAt           time.Time
	ExpiresAt           *time.Time
	DeletionScheduledAt *time.Time
	ReviewRequestedAt   *time.Time
	NotifyURL           string
}

var (
	// ErrNotFound is returned when a branch, or the project it belongs to, does not exist.
	ErrNotFound = errors.New("branching: branch not found")
	// ErrInvalid marks a request the caller can fix (a bad name, an unsupported combination).
	ErrInvalid = errors.New("branching: invalid request")
	// ErrConflict marks a request that clashes with the current state (name taken, another
	// operation running, divergence without force).
	ErrConflict = errors.New("branching: conflict")
	// ErrLimit is returned when creating a branch would exceed the configured caps.
	ErrLimit = errors.New("branching: branch limit reached")
	// ErrDiverged is returned by merge and push when the two histories disagree and force is not set.
	ErrDiverged = fmt.Errorf("%w: migration histories diverged", ErrConflict)
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func conflict(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, a...))
}

// A branch name is what a git branch name can be (slashes included), minus anything that
// is awkward in a URL path or a log line.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)

// ValidName checks a branch name.
func ValidName(name string) error {
	switch {
	case !nameRE.MatchString(name):
		return invalid("branch name %q: use letters, digits and . _ / - (1 to 100 characters, starting with a letter or digit)", name)
	case strings.Contains(name, "..") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, "."):
		return invalid("branch name %q is not allowed", name)
	case name == DefaultBranchName:
		return invalid("%q is the name of the default branch", DefaultBranchName)
	}
	return nil
}

// defaultBranchID derives the stable id of a project's default branch (a version 5 style
// UUID over the ref), so that repeated listings agree.
func defaultBranchID(ref string) string {
	h := sha1.Sum([]byte("supavise-default-branch:" + ref))
	var b [16]byte
	copy(b[:], h[:16])
	b[6] = b[6]&0x0f | 0x50
	b[8] = b[8]&0x3f | 0x80
	return fmtUUID(b)
}

func fmtUUID(b [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID reports whether s looks like a branch id.
func IsUUID(s string) bool { return uuidRE.MatchString(s) }

func branchOf(p *registry.Project) *Branch {
	b := p.Branch
	return &Branch{
		ID: b.ID, Name: b.Name, Ref: p.Ref, ParentRef: b.ParentRef, GitBranch: b.GitBranch, Region: p.Region,
		Persistent: b.Persistent, WithData: b.WithData, State: b.State, ProjectStatus: p.Status,
		CloneMethod: b.CloneMethod, Detail: b.Detail, Egress: b.Egress, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		ExpiresAt: b.ExpiresAt, DeletionScheduledAt: b.DeletionScheduledAt, ReviewRequestedAt: b.ReviewRequestedAt, NotifyURL: b.NotifyURL,
	}
}

// defaultBranchOf is the synthesized default branch of a parent project.
func defaultBranchOf(p *registry.Project) *Branch {
	return &Branch{
		ID: defaultBranchID(p.Ref), Name: DefaultBranchName, Ref: p.Ref, ParentRef: p.Ref, IsDefault: true, Persistent: true, Region: p.Region,
		State: registry.BranchMigrationsPassed, ProjectStatus: p.Status, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}
