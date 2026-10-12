package branching

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// resetMinLife is the shortest remaining lifetime of a non-persistent branch after a reset.
const resetMinLife = 15 * time.Minute

// Reset recreates the branch's database. A schema-only branch replays its own recorded migration
// history (up to MigrationVersion when set, which may name a migration only the branch has; "0"
// gives an empty database) and then the seed: the migrations the branch added itself survive, and
// the tables and data that never became migrations do not. A with_data branch gets a fresh copy of
// the parent's data. The branch keeps its id, name, ref, Edge Functions, function secrets and saved
// settings. A schema-only branch also keeps its credentials; a with_data branch gets new ones, as
// at creation. Anything else written to the branch is gone, and so is the archive of a persistent
// branch (its WAL and base backups: the recreated cluster reuses the ref, and its WAL would collide
// with the old).
//
// Everything that can fail without touching the branch is checked first (the parent is
// running, the migration version exists, a with_data branch has a way to get its data). The old
// cluster is then removed but the branch's registry row is kept, so a failure later leaves the
// branch registered as MIGRATIONS_FAILED: it can be reset again or deleted.
func (s *Service) Reset(ctx context.Context, idOrRef string, in ActionInput) (string, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "reset")
	if err != nil {
		return "", err
	}
	if err := s.checkIdle(ctx, b); err != nil {
		return "", err
	}
	parent, err := s.activeParent(ctx, b)
	if err != nil {
		return "", err
	}
	if in.MigrationVersion != "" && b.WithData {
		return "", invalid("migration_version only applies to a schema-only branch")
	}
	old, err := s.reg.GetProject(ctx, b.Ref)
	if err != nil {
		return "", err
	}
	org := ""
	if old.OrgID != 0 {
		o, err := s.reg.GetOrganizationByID(ctx, old.OrgID)
		if err != nil {
			return "", err
		}
		org = o.Slug
	}
	if err := s.preflightReset(ctx, b, parent, old); err != nil {
		return "", err
	}
	// The branch's own history is read now, while its cluster exists, and the version is judged
	// against it before anything is removed.
	history, useHistory, histNote, err := s.resetHistory(ctx, b, parent, in)
	if err != nil {
		return "", err
	}
	var keys *secrets.ProjectKeys
	if !b.WithData {
		// A branch whose creation failed may have no credentials worth keeping.
		if keys, err = s.eng.Keys(ctx, b.Ref); err != nil {
			keys = nil
		}
	}
	r, err := s.beginRun(ctx, b.Ref, "reset", b.Ref, parent.Ref)
	if err != nil {
		return "", err
	}
	s.setState(ctx, b.Ref, registry.BranchCreatingProject, "resetting from "+parent.Ref, nil)
	s.spawn(r, b.Ref, parent.Ref, func(ctx context.Context) (string, error) {
		s.phase(ctx, StepPull)
		if histNote != "" {
			s.logRun(ctx, "%s", histNote)
		}
		if err := s.removeBranch(ctx, b, removeOptions{keepRecord: true}); err != nil {
			return "", fmt.Errorf("remove the old cluster: %w", err)
		}
		info := *old.Branch
		info.State, info.Detail, info.CloneMethod = registry.BranchCreatingProject, "recreating the project", ""
		if !info.Persistent {
			// The branch keeps the lifetime it was given, but a reset never leaves it less than
			// resetMinLife: it would otherwise expire while the agent starts using it again.
			if floor := s.now().Add(resetMinLife).UTC(); info.ExpiresAt == nil || info.ExpiresAt.Before(floor) {
				info.ExpiresAt = &floor
			}
		}
		info.DeletionScheduledAt = nil
		// With data, the new cluster gets the same outbound policy, applied again after its first
		// start (set below); an opt-out stays an opt-out, and what could not be enforced is tried again.
		// The row is still there: record the lifetime and the cleared method on it now. The policy
		// has its own writer (UpdateBranch never writes it), a compare-and-set from the value the
		// row has right now, so that nothing racing with the reset puts a stale policy back.
		s.setState(ctx, b.Ref, info.State, info.Detail, func(bi *registry.BranchInfo) {
			bi.CloneMethod, bi.ExpiresAt, bi.DeletionScheduledAt = info.CloneMethod, info.ExpiresAt, nil
		})
		if info.WithData {
			cur, err := s.reg.GetProject(ctx, b.Ref)
			if err != nil || cur.Branch == nil {
				return "", fmt.Errorf("read the branch before its policy is set: %v", err)
			}
			info.Egress = s.egressPolicy(cur.Branch.Egress == registry.EgressAllowed)
			if err := s.reg.SetBranchEgress(ctx, b.Ref, cur.Branch.Egress, info.Egress); err != nil {
				return "", fmt.Errorf("record the branch's egress policy: %w", err)
			}
		}
		j := &createJob{
			parent: parent, ref: b.Ref, info: &info, class: old.Class, org: org, recreate: true,
			in: CreateInput{Name: b.Name, GitBranch: b.GitBranch, Persistent: b.Persistent, WithData: b.WithData, NotifyURL: b.NotifyURL},
		}
		if keys != nil {
			j.keys = keys
		}
		j.upTo = in.MigrationVersion
		j.history, j.useHistory = history, useHistory
		j.keepExpiry = true
		return s.doCreate(ctx, j)
	})
	return r.id, nil
}

// preflightReset checks what a reset needs before the old cluster is removed: the same way
// of getting data that creation would use, and for the base-backup way a base backup to
// restore.
func (s *Service) preflightReset(ctx context.Context, b *Branch, parent, old *registry.Project) error {
	if !b.WithData {
		return nil
	}
	if err := s.checkBranchHere(ctx, old, parent.Ref); err != nil {
		return err
	}
	// A base-backup branch holds a full private copy that the reset removes before it clones
	// again, so that space counts; a copy-on-write branch shares blocks with its parent and
	// gives back little, so nothing is assumed.
	var credit int64
	if old.Branch != nil && old.Branch.CloneMethod == MethodBackup {
		credit = dataSize(filepath.Join(s.cfg.Paths().ProjectService(b.Ref, config.SvcPostgres), "data"))
	}
	if err := s.checkDisk(parent.Ref, credit); err != nil {
		return err
	}
	method, _, reason := s.planData(parent.Ref, b.Ref)
	switch method {
	case MethodSchema:
		return invalid("with_data is not possible: %s", reason)
	case MethodBackup:
		if _, err := s.bk.PlanRestoreWith(ctx, parent.Ref, s.now(), backup.RestoreOptions{Latest: true}); err != nil {
			return conflict("cannot reset %s from the base backup of %s: %v", b.Name, parent.Ref, err)
		}
	default:
		if _, err := s.eng.Keys(ctx, parent.Ref); err != nil {
			return fmt.Errorf("read the parent's credentials: %w", err)
		}
	}
	return nil
}

// resetHistory reads the migration history a reset of a schema-only branch replays: the branch's own,
// and checks that migration_version ("0" for an empty database) is in it. note says which history the
// reset uses, for the run log. A branch that has recorded nothing (a creation that failed early), or
// whose cluster is not serving (its state is a failed or unfinished one), is reset from the parent's
// history as a new branch would be, and the version is looked for there. A branch that is registered as
// working and cannot be read is not: replaying the parent's history would drop the migrations the branch
// added itself, so the reset stops and says so, unless Force asks for the parent's history.
func (s *Service) resetHistory(ctx context.Context, b *Branch, parent *registry.Project, in ActionInput) (hist []Migration, useOwn bool, note string, err error) {
	version := in.MigrationVersion
	if b.WithData {
		return nil, false, "", nil
	}
	own, rerr := s.db.Migrations(ctx, b.Ref)
	if rerr != nil && (b.State == registry.BranchFunctionsDeployed || b.State == registry.BranchMigrationsPassed) && !in.Force {
		return nil, false, "", fmt.Errorf("read the branch's migration history (a reset replays it, and without it the migrations the branch added itself are lost; try again when the branch answers, or use force to reset from the parent's history): %w", rerr)
	}
	use := rerr == nil && len(own) > 0
	pool := own
	if use {
		// A diff recorded on the branch and not merged yet holds everything the branch changed, the
		// migrations only the branch has included: replaying both would create their tables twice.
		if pm, perr := s.db.Migrations(ctx, parent.Ref); perr == nil {
			own = coverByRecordedDiff(own, pm)
		}
		pool = own
		note = fmt.Sprintf("the branch is recreated from its own migration history (%d migration(s))", len(own))
	} else {
		pool = nil
		why := "the branch has recorded no migrations"
		if rerr != nil {
			why = fmt.Sprintf("the branch's history could not be read (%v)", rerr)
		}
		note = "the branch is recreated from the parent's migration history: " + why
		if version != "" && version != "0" {
			if pool, err = s.db.Migrations(ctx, parent.Ref); err != nil {
				return nil, false, "", fmt.Errorf("read the parent's migrations: %w", err)
			}
		}
	}
	if version != "" && version != "0" {
		found := false
		for _, m := range pool {
			found = found || m.Version == version
		}
		if !found {
			where := "the migration history of the branch"
			if !use {
				where = "the migration history of the parent (the branch has recorded none)"
			}
			return nil, false, "", invalid("migration_version %s is not in %s", version, where)
		}
	}
	return own, use, note, nil
}

// coverByRecordedDiff marks as covered the branch's own migrations (those the parent lacks) that are
// older than a recorded schema diff of the branch (a branch_merge_* migration the parent lacks): that
// diff was made from the branch's schema when it was recorded and holds their changes. That includes
// the older recordings: Studio records a new one at every click of Merge, each holding the same changes
// and more. The marked ones are recorded when the history is replayed and only what a schema diff does
// not hold of them runs; the newest diff runs.
func coverByRecordedDiff(own, parent []Migration) []Migration {
	have := make(map[string]bool, len(parent))
	for _, m := range parent {
		have[m.Version] = true
	}
	var only []Migration
	for _, m := range own {
		if !have[m.Version] {
			only = append(only, m)
		}
	}
	rec := recordedDiff(only)
	if rec == nil {
		return own
	}
	out := make([]Migration, len(own))
	for i, m := range own {
		if !have[m.Version] && m.Covered == "" && m.Version < rec.Version && !strings.HasPrefix(m.Name, pushMigrationPrefix) {
			m.Covered = rec.Version
		}
		out[i] = m
	}
	return out
}
