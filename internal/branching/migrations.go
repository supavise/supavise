package branching

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/supavise/supavise/internal/branching/schemadiff"
	"github.com/supavise/supavise/internal/registry"
)

// Migration is one row of supabase_migrations.schema_migrations: what `supabase db push`
// and the Management API's apply-migration record.
type Migration struct {
	Version    string
	Name       string
	Statements []string
	// Covered is set on a migration whose changes another migration of the same history already holds
	// (the version of that one): the diff Studio's merge page recorded is the whole of what the branch
	// changed, tracked migrations included, so the tracked ones are kept in the history without being
	// run again. A replay, a push and a merge record a covered migration and do not run it. It is
	// stored in the created_by column of the history table; it takes no part in comparing migrations.
	Covered string
}

// coveredPrefix begins the created_by value of a covered migration.
const coveredPrefix = "supavise:covered:"

// sql is the migration as one script, for diffs and equality.
func (m Migration) sql() string { return strings.Join(m.Statements, ";\n") }

func sameMigration(a, b Migration) bool {
	return a.Name == b.Name && normalizeSQL(a.sql()) == normalizeSQL(b.sql())
}

func normalizeSQL(s string) string {
	return strings.Join(strings.Fields(strings.TrimRight(strings.TrimSpace(s), ";")), " ")
}

// Conflict is a version both histories have, with different content.
type Conflict struct {
	Version string
	Source  Migration
	Target  Migration
}

// Plan compares a source history with a target history.
type Plan struct {
	// Apply are the source's migrations the target lacks, in version order.
	Apply []Migration
	// TargetOnly are the target's migrations the source lacks.
	TargetOnly []Migration
	// Conflicts are versions on both sides with different content (never in Apply).
	Conflicts []Conflict
	// OutOfOrder are versions in Apply that sort before the target's latest version: they
	// would run after a migration that came later in version order.
	OutOfOrder []string
}

// Compare diffs two migration histories. Versions compare as strings, which orders the
// CLI's 14-digit timestamps correctly.
func Compare(source, target []Migration) Plan {
	t := make(map[string]Migration, len(target))
	latest := ""
	for _, m := range target {
		t[m.Version] = m
		latest = max(latest, m.Version)
	}
	s := make(map[string]Migration, len(source))
	var p Plan
	for _, m := range source {
		s[m.Version] = m
		have, ok := t[m.Version]
		switch {
		case !ok:
			p.Apply = append(p.Apply, m)
			if m.Version < latest {
				p.OutOfOrder = append(p.OutOfOrder, m.Version)
			}
		case !sameMigration(m, have):
			p.Conflicts = append(p.Conflicts, Conflict{Version: m.Version, Source: m, Target: have})
		}
	}
	for _, m := range target {
		if _, ok := s[m.Version]; !ok {
			p.TargetOnly = append(p.TargetOnly, m)
		}
	}
	byVersion := func(ms []Migration) {
		sort.Slice(ms, func(i, j int) bool { return ms[i].Version < ms[j].Version })
	}
	byVersion(p.Apply)
	byVersion(p.TargetOnly)
	sort.Slice(p.Conflicts, func(i, j int) bool { return p.Conflicts[i].Version < p.Conflicts[j].Version })
	sort.Strings(p.OutOfOrder)
	return p
}

// Summary describes a plan for error messages and events.
func (p Plan) Summary() string {
	var parts []string
	if len(p.TargetOnly) > 0 {
		parts = append(parts, "target has migrations the source lacks: "+versions(p.TargetOnly))
	}
	if len(p.Conflicts) > 0 {
		var v []string
		for _, c := range p.Conflicts {
			v = append(v, c.Version)
		}
		parts = append(parts, "versions with different content: "+strings.Join(v, ", "))
	}
	if len(p.OutOfOrder) > 0 {
		parts = append(parts, "migrations older than the target's latest: "+strings.Join(p.OutOfOrder, ", "))
	}
	return strings.Join(parts, "; ")
}

func versions(ms []Migration) string {
	v := make([]string, len(ms))
	for i, m := range ms {
		v[i] = m.Version
	}
	return strings.Join(v, ", ")
}

// Database runs SQL on a project's "postgres" database as the postgres role.
type Database interface {
	// Migrations returns the project's recorded migration history (empty when it has none).
	Migrations(ctx context.Context, ref string) ([]Migration, error)
	// Apply runs the migrations in order and records each one. With o.Atomic they run in a
	// single transaction (all or nothing) and fall back to one transaction each when a
	// statement cannot run inside a transaction block.
	Apply(ctx context.Context, ref string, ms []Migration, o ApplyOptions) (ApplyResult, error)
	// RunScript executes a script (a seed) in one transaction.
	RunScript(ctx context.Context, ref, script string) error
}

// ApplyOptions tune Database.Apply.
type ApplyOptions struct {
	Atomic bool
	// LockTimeout bounds how long a statement waits for a lock (0: no limit). Merges set it
	// so that DDL on a busy parent fails instead of queueing behind, and blocking, traffic.
	LockTimeout time.Duration
	// Verify, with Atomic, is called while Apply holds the project's apply lock with the
	// target's migration history as it is then, before anything runs. The history the plan was
	// made from may have changed while Apply waited for the lock (another merge); an error
	// from Verify aborts Apply without running anything.
	Verify func(current []Migration) error
}

// ApplyResult reports how Apply ran.
type ApplyResult struct {
	Applied int
	// NonAtomic is set when the all-or-nothing transaction was not possible.
	NonAtomic bool
}

// pgDatabase is the Database over pgx.
type pgDatabase struct {
	dsn func(ctx context.Context, ref string) (string, error)
}

func (d *pgDatabase) conn(ctx context.Context, ref string) (*pgx.Conn, error) {
	dsn, err := d.dsn(ctx, ref)
	if err != nil {
		return nil, err
	}
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cc.RuntimeParams["application_name"] = "supavise-branching"
	return pgx.ConnectConfig(ctx, cc)
}

func closeConn(c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Close(ctx)
}

const migrationsTable = "supabase_migrations.schema_migrations"

// Migrations implements Database.
func (d *pgDatabase) Migrations(ctx context.Context, ref string) ([]Migration, error) {
	c, err := d.conn(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer closeConn(c)
	return readMigrations(ctx, c)
}

// readMigrations reads the migration history over an open connection.
func readMigrations(ctx context.Context, c *pgx.Conn) ([]Migration, error) {
	var has bool
	if err := c.QueryRow(ctx, `select to_regclass('`+migrationsTable+`') is not null`).Scan(&has); err != nil {
		return nil, err
	}
	if !has {
		return nil, nil
	}
	// A history written by another tool may lack the created_by column.
	var hasBy bool
	if err := c.QueryRow(ctx, `select exists (select 1 from pg_attribute where attrelid = '`+migrationsTable+`'::regclass and attname = 'created_by' and not attisdropped)`).Scan(&hasBy); err != nil {
		return nil, err
	}
	by := `''`
	if hasBy {
		by = `coalesce(created_by, '')`
	}
	rows, err := c.Query(ctx, `select version, coalesce(name, ''), coalesce(statements, '{}'), `+by+` from `+migrationsTable+` order by version`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Migration, error) {
		var m Migration
		var createdBy string
		err := r.Scan(&m.Version, &m.Name, &m.Statements, &createdBy)
		m.Covered = strings.TrimPrefix(createdBy, coveredPrefix)
		if m.Covered == createdBy {
			m.Covered = "" // not a marker
		}
		return m, err
	})
}

const ensureMigrationsTable = `
create schema if not exists supabase_migrations;
create table if not exists ` + migrationsTable + ` (version text not null primary key, statements text[], name text);
alter table ` + migrationsTable + ` add column if not exists created_by text;
alter table ` + migrationsTable + ` add column if not exists idempotency_key text unique;
alter table ` + migrationsTable + ` add column if not exists rollback text[];`

// notInTransaction is SQLSTATE 25001 (active_sql_transaction): CREATE INDEX CONCURRENTLY,
// VACUUM and the like.
func notInTransaction(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "25001"
}

func applyOne(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, m Migration) error {
	for i, st := range m.Statements {
		if strings.TrimSpace(st) == "" {
			continue
		}
		if _, err := q.Exec(ctx, st); err != nil {
			return fmt.Errorf("migration %s (statement %d): %w", m.Version, i+1, err)
		}
	}
	// A plain insert: a version that is already recorded aborts the transaction instead of
	// letting its DDL run twice with only one record.
	_, err := q.Exec(ctx, `insert into `+migrationsTable+` (version, name, statements) values ($1, nullif($2, ''), $3)`, m.Version, m.Name, m.Statements)
	return err
}

// Apply implements Database.
func (d *pgDatabase) Apply(ctx context.Context, ref string, ms []Migration, o ApplyOptions) (ApplyResult, error) {
	var res ApplyResult
	if len(ms) == 0 {
		return res, nil
	}
	c, err := d.conn(ctx, ref)
	if err != nil {
		return res, err
	}
	defer closeConn(c)
	if o.LockTimeout > 0 {
		if _, err := c.Exec(ctx, fmt.Sprintf(`set lock_timeout = %d`, o.LockTimeout.Milliseconds())); err != nil {
			return res, err
		}
	}
	if _, err := c.Exec(ctx, ensureMigrationsTable); err != nil {
		return res, fmt.Errorf("prepare %s: %w", migrationsTable, err)
	}
	if o.Atomic {
		// One merge at a time per project: two would interleave their DDL. The lock belongs to
		// this session, so it also covers the statement-by-statement fallback below, and it is
		// released when the connection closes. The decision to apply was made before the lock
		// was held: look again, so that a version another merge just applied is not run twice, and let
		// the caller check its whole plan (divergence included) against the history as it is now.
		if _, err := c.Exec(ctx, `select pg_advisory_lock(hashtext('supavise.branching.apply'))`); err != nil {
			return res, fmt.Errorf("wait for other migrations on %s: %w", ref, err)
		}
		current, err := readMigrations(ctx, c)
		if err != nil {
			return res, err
		}
		if err := recheck(ref, ms, current, o.Verify); err != nil {
			return res, err
		}
	}
	if o.Atomic {
		err := pgx.BeginFunc(ctx, c, func(tx pgx.Tx) error {
			for _, m := range ms {
				if err := applyOne(ctx, tx, m); err != nil {
					return err
				}
			}
			return nil
		})
		if err == nil {
			res.Applied = len(ms)
			return res, nil
		}
		if !notInTransaction(err) {
			return res, err
		}
		res.NonAtomic = true
	}
	for _, m := range ms {
		if err := applyEach(ctx, c, m); err != nil {
			return res, err
		}
		res.Applied++
	}
	return res, nil
}

// recheck is Apply's look at the target's history once it holds the lock: none of the
// migrations may be there already, and the caller's own check (Verify) must still pass.
func recheck(ref string, ms, current []Migration, verify func([]Migration) error) error {
	have := map[string]bool{}
	for _, m := range current {
		have[m.Version] = true
	}
	var dup []string
	for _, m := range ms {
		if have[m.Version] {
			dup = append(dup, m.Version)
		}
	}
	if len(dup) > 0 {
		sort.Strings(dup)
		return fmt.Errorf("%w: %s already applied to %s while this operation waited; read the histories again", ErrDiverged, strings.Join(dup, ", "), ref)
	}
	if verify != nil {
		return verify(current)
	}
	return nil
}

// applyEach runs one migration in its own transaction, or outside one when a statement
// refuses to run inside a transaction block.
func applyEach(ctx context.Context, c *pgx.Conn, m Migration) error {
	err := pgx.BeginFunc(ctx, c, func(tx pgx.Tx) error { return applyOne(ctx, tx, m) })
	if err == nil || !notInTransaction(err) {
		return err
	}
	return applyOne(ctx, c, m)
}

// RunScript implements Database.
func (d *pgDatabase) RunScript(ctx context.Context, ref, script string) error {
	c, err := d.conn(ctx, ref)
	if err != nil {
		return err
	}
	defer closeConn(c)
	return pgx.BeginFunc(ctx, c, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, script)
		return err
	})
}

// upTo keeps the migrations with version <= v ("" keeps all); v must name one of them.
func upTo(ms []Migration, v string) ([]Migration, error) {
	if v == "" {
		return ms, nil
	}
	var out []Migration
	found := false
	for _, m := range ms {
		if m.Version <= v {
			out = append(out, m)
		}
		found = found || m.Version == v
	}
	if !found {
		return nil, invalid("migration_version %s is not a migration the target lacks", v)
	}
	return out, nil
}

// replay gives a schema-only branch its migration history, then the seed. A new branch gets the
// parent's history, or, when the parent has none (it was built in the SQL editor), a baseline of the
// parent's schema (baselineFor). A reset replays the branch's own history instead (createJob.history).
// upTo limits it to a version; "0" is an empty database and runs no seed.
func (s *Service) replay(ctx context.Context, j *createJob) (migrations int, seeded bool, err error) {
	s.phase(ctx, StepMigrate)
	var hist []Migration
	if j.useHistory {
		hist = j.history
	} else {
		if hist, err = s.db.Migrations(ctx, j.parent.Ref); err != nil {
			return 0, false, fmt.Errorf("read the parent's migration history: %w", err)
		}
		if len(hist) == 0 && j.upTo != "0" {
			if hist, err = s.baselineFor(ctx, j); err != nil {
				return 0, false, err
			}
		}
	}
	switch j.upTo {
	case "":
	case "0":
		hist = nil
	default:
		cut := hist[:0:0]
		found := false
		for _, m := range hist {
			if m.Version <= j.upTo {
				cut = append(cut, m)
			}
			found = found || m.Version == j.upTo
		}
		if !found {
			return 0, false, invalid("migration_version %s is not in the migration history", j.upTo)
		}
		hist = cut
	}
	if len(hist) > 0 {
		s.setState(ctx, j.ref, registry.BranchRunningMigration, fmt.Sprintf("replaying %d migration(s)", len(hist)), nil)
		if _, err := s.applyHistory(ctx, j.ref, hist, ApplyOptions{}); err != nil {
			return 0, false, fmt.Errorf("replay the migrations: %w", err)
		}
	}
	if j.upTo == "0" {
		return 0, false, nil
	}
	seed := j.in.Seed
	if strings.TrimSpace(seed) == "" {
		if seed, err = s.GetSeed(ctx, j.parent.Ref); err != nil {
			return 0, false, err
		}
	}
	if strings.TrimSpace(seed) != "" {
		s.phase(ctx, StepSeed)
		s.setState(ctx, j.ref, registry.BranchRunningMigration, "applying the seed", nil)
		if err := s.db.RunScript(ctx, j.ref, seed); err != nil {
			return len(hist), false, fmt.Errorf("apply the seed: %w", err)
		}
		return len(hist), true, nil
	}
	return len(hist), false, nil
}

// applyHistory runs the migrations of ms that are not covered and records the covered ones without
// running them (they are in the history so that it matches the other side's, and their changes come
// with the migration that covers them). Without a SchemaDatabase every migration runs.
func (s *Service) applyHistory(ctx context.Context, ref string, ms []Migration, o ApplyOptions) (ApplyResult, error) {
	var run, covered []Migration
	for _, m := range ms {
		if m.Covered != "" && s.schemaDB() != nil {
			covered = append(covered, m)
		} else {
			run = append(run, m)
		}
	}
	res, err := s.db.Apply(ctx, ref, run, o)
	if err != nil {
		return res, err
	}
	if len(covered) > 0 {
		if _, err := s.schemaDB().Record(ctx, ref, covered); err != nil {
			return res, fmt.Errorf("record the migrations whose changes another migration holds: %w", err)
		}
	}
	return res, nil
}

// withCovered returns the migrations marked as covered by the migration at version by.
func withCovered(ms []Migration, by string) []Migration {
	out := make([]Migration, len(ms))
	for i, m := range ms {
		m.Covered = by
		out[i] = m
	}
	return out
}

// ---- schemas and history without running it ---------------------------------------------------

// SchemaDatabase is what a Database adds when it can read a project's schema and write its
// migration history without running it. The pgx Database implements it; a Database that does
// not leaves the branching operations that need it (the baseline, the schema diff, the base a
// branch is compared with) out.
type SchemaDatabase interface {
	// Snapshot reads the schema of ref's database.
	Snapshot(ctx context.Context, ref string, o schemadiff.ReadOptions) (*schemadiff.Snapshot, error)
	// Roles returns the names of the roles of ref's cluster.
	Roles(ctx context.Context, ref string) (map[string]bool, error)
	// Record puts migrations into the history of ref without running them, under the lock Apply
	// takes. A version that is already there with the same content is left; with other content it is
	// an error. It returns the versions it recorded.
	Record(ctx context.Context, ref string, ms []Migration) ([]string, error)
	// InitHistory records m as the first migration of ref, without running it, only when the
	// history has none. It reports whether it did.
	InitHistory(ctx context.Context, ref string, m Migration) (bool, error)
}

// schemaDB returns the Database's schema abilities, nil when it has none.
func (s *Service) schemaDB() SchemaDatabase {
	sd, _ := s.db.(SchemaDatabase)
	return sd
}

const applyLockSQL = `select pg_advisory_lock(hashtext('supavise.branching.apply'))`

// Snapshot implements SchemaDatabase.
func (d *pgDatabase) Snapshot(ctx context.Context, ref string, o schemadiff.ReadOptions) (*schemadiff.Snapshot, error) {
	c, err := d.conn(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer closeConn(c)
	tx, err := c.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// Qualified names in everything the catalog functions print, whatever the role's search_path is.
	if _, err := tx.Exec(ctx, `set local search_path = pg_catalog`); err != nil {
		return nil, err
	}
	return schemadiff.Read(ctx, tx, o)
}

// Roles implements SchemaDatabase.
func (d *pgDatabase) Roles(ctx context.Context, ref string) (map[string]bool, error) {
	c, err := d.conn(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer closeConn(c)
	rows, err := c.Query(ctx, `select rolname from pg_roles`)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

// Record implements SchemaDatabase.
func (d *pgDatabase) Record(ctx context.Context, ref string, ms []Migration) ([]string, error) {
	if len(ms) == 0 {
		return nil, nil
	}
	c, err := d.conn(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer closeConn(c)
	if _, err := c.Exec(ctx, ensureMigrationsTable); err != nil {
		return nil, fmt.Errorf("prepare %s: %w", migrationsTable, err)
	}
	if _, err := c.Exec(ctx, applyLockSQL); err != nil {
		return nil, fmt.Errorf("wait for other migrations on %s: %w", ref, err)
	}
	var done []string
	err = pgx.BeginFunc(ctx, c, func(tx pgx.Tx) error {
		for _, m := range ms {
			var have []string
			var name *string
			switch err := tx.QueryRow(ctx, `select statements, name from `+migrationsTable+` where version = $1`, m.Version).Scan(&have, &name); {
			case err == nil:
				cur := Migration{Version: m.Version, Statements: have}
				if name != nil {
					cur.Name = *name
				}
				if !sameMigration(cur, m) {
					return fmt.Errorf("%w: version %s is already recorded on %s with other content", ErrDiverged, m.Version, ref)
				}
				if m.Covered != "" {
					// the same migration, now known to be covered by another
					if _, err := tx.Exec(ctx, `update `+migrationsTable+` set created_by = $2 where version = $1`, m.Version, coveredPrefix+m.Covered); err != nil {
						return err
					}
				}
				continue
			case !errors.Is(err, pgx.ErrNoRows):
				return err
			}
			marker := ""
			if m.Covered != "" {
				marker = coveredPrefix + m.Covered
			}
			if _, err := tx.Exec(ctx, `insert into `+migrationsTable+` (version, name, statements, created_by) values ($1, nullif($2, ''), $3, nullif($4, ''))`, m.Version, m.Name, m.Statements, marker); err != nil {
				return err
			}
			done = append(done, m.Version)
		}
		return nil
	})
	return done, err
}

// InitHistory implements SchemaDatabase.
func (d *pgDatabase) InitHistory(ctx context.Context, ref string, m Migration) (bool, error) {
	c, err := d.conn(ctx, ref)
	if err != nil {
		return false, err
	}
	defer closeConn(c)
	if _, err := c.Exec(ctx, ensureMigrationsTable); err != nil {
		return false, fmt.Errorf("prepare %s: %w", migrationsTable, err)
	}
	if _, err := c.Exec(ctx, applyLockSQL); err != nil {
		return false, fmt.Errorf("wait for other migrations on %s: %w", ref, err)
	}
	inserted := false
	err = pgx.BeginFunc(ctx, c, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `select count(*) from `+migrationsTable).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `insert into `+migrationsTable+` (version, name, statements) values ($1, nullif($2, ''), $3)`, m.Version, m.Name, m.Statements); err != nil {
			return err
		}
		inserted = true
		return nil
	})
	return inserted, err
}

// scriptHash identifies a diff: the SHA-256 of its text with whitespace folded, so that a diff
// recorded through the API and one computed again compare equal.
func scriptHash(script string) string {
	sum := sha256.Sum256([]byte(normalizeSQL(script)))
	return hex.EncodeToString(sum[:])
}
