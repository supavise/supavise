package registry

import (
	"context"
	"time"
)

// BranchSchemaBase is the schema a branch and its parent last agreed on (migration 1510), as the
// JSON of a schemadiff.Snapshot, gzip compressed.
type BranchSchemaBase struct {
	BranchRef   string
	Engine      int
	Fingerprint string
	Snapshot    []byte
	// Source says what took it: "create", "push", "merge" or "reset".
	Source  string
	TakenAt time.Time
}

// BranchContentStore keeps what a branch needs to be compared with its parent: the schema base and
// the base digests of the Edge Functions (migrations 1510 and 1511). Both registries implement it;
// the branching service reaches it through BranchContentOf.
type BranchContentStore interface {
	// GetBranchSchemaBase returns the base of the branch, or ErrNotFound (a branch made before the
	// base existed has none).
	GetBranchSchemaBase(ctx context.Context, branchRef string) (*BranchSchemaBase, error)
	// PutBranchSchemaBase stores b for b.BranchRef, replacing an earlier one. ErrNotFound for an
	// unknown project.
	PutBranchSchemaBase(ctx context.Context, b BranchSchemaBase) error
	// GetBranchFunctionBase returns the base digest of each function slug of the branch.
	GetBranchFunctionBase(ctx context.Context, branchRef string) (map[string]string, error)
	// PutBranchFunctionBase sets the base digest of the slugs in set and forgets those in drop.
	PutBranchFunctionBase(ctx context.Context, branchRef string, set map[string]string, drop []string) error
}

// BranchContentOf returns reg's BranchContentStore, or nil when it has none.
func BranchContentOf(reg Registry) BranchContentStore {
	s, _ := reg.(BranchContentStore)
	return s
}

var (
	_ BranchContentStore = (*Memory)(nil)
	_ BranchContentStore = (*Postgres)(nil)
)
