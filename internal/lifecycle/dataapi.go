package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// Data API exposure, as hosted Supabase has it since 2026-05-30: a table created in "public" is not
// reachable through PostgREST or GraphQL until a grant says so. The default privileges that used to
// give select, insert, update and delete on new tables, usage and select on new sequences, and execute
// on new functions, to anon, authenticated and service_role are revoked for the role postgres (the role
// the SQL editor, the dashboard and migrations run as). Objects that exist keep their grants. Hosted
// applies the same change to the projects it already runs on 2026-10-30; Supavise applies it once per
// project when a node first runs a project on the release that has it (Engine.ConvergeDataAPI).
//
// Sources. The first two statements are the ones hosted's changelog gives, "Breaking change: tables not
// exposed to Data and GraphQL API automatically",
// https://supabase.com/changelog/45329-breaking-change-tables-not-exposed-to-data-and-graphql-api-automatically.
// It says nothing about functions. The last two are in Studio's own SQL for the same switch (the
// "Automatically expose new tables" setting under Integrations, Data API; buildDefaultPrivilegesSql
// ('revoke') in packages/pg-meta/src/sql/studio/database/privileges.ts at the pinned commit), which
// the create-project form's checkbox of the same name controls and whose telemetry calls it
// "new tables and functions". Studio reads the switch as off only when the functions are revoked too
// (getDefaultPrivilegesStateSql counts three default ACL rows). Revoking functions as well is the safe
// side of the doubt: a migration that works here because it grants execute on its functions also works
// on hosted, and the reverse would not hold.
var RevokeDefaultPrivilegesSQL = []string{
	"alter default privileges for role postgres in schema public revoke select, insert, update, delete on tables from anon, authenticated, service_role",
	"alter default privileges for role postgres in schema public revoke usage, select on sequences from anon, authenticated, service_role",
	"alter default privileges for role postgres in schema public revoke execute on functions from anon, authenticated, service_role",
	"alter default privileges for role postgres in schema public revoke execute on functions from public",
}

// GrantDefaultPrivilegesSQL is the rollback: hosted's page gives the first two statements, Studio's
// switch the last two. It brings back the grants new objects carried before. Objects created since
// the revoke need the grants on all tables, sequences and routines of public too (docs/guide.md).
// Supavise never runs it; a test keeps the guide's copy equal to this one.
var GrantDefaultPrivilegesSQL = []string{
	"alter default privileges for role postgres in schema public grant select, insert, update, delete on tables to anon, authenticated, service_role",
	"alter default privileges for role postgres in schema public grant usage, select on sequences to anon, authenticated, service_role",
	"alter default privileges for role postgres in schema public grant execute on functions to anon, authenticated, service_role",
	"alter default privileges for role postgres in schema public grant execute on functions to public",
}

// APISchemaSQL creates the dedicated schema hosted's "Switch the default API schema" dialog
// makes (Studio, HardenAPIModal and create-and-expose-api-schema-mutation): a schema "api" the
// Data API roles may enter. It carries no table grants, so each table in it needs its own, as the
// dialog says. service_role is added to Studio's anon and authenticated because the secret key
// reaches the schema as service_role. The owner is postgres, the role a user manages the schema as.
var APISchemaSQL = []string{
	"create schema if not exists api authorization postgres",
	"grant usage on schema api to anon, authenticated, service_role",
}

// DisabledDataAPISchema is what a project exposes when it was created with the Data API off
// (data_api_exposed_schemas: []). PostgREST falls back to "public" for an empty db_schema, so an
// empty list cannot switch it off on a node (internal/projectconfig README); graphql_public, which
// every project has and which holds one function that answers "not enabled" until pg_graphql is
// installed, exposes no table.
const DisabledDataAPISchema = "graphql_public"

// DataAPIOptions are the Data API choices of a new project (CreateRequest.DataAPI). The zero value
// is hosted's default for a new project: default privileges revoked, the default exposed schemas.
type DataAPIOptions struct {
	// ExposeNewTables keeps the default grants of tables and sequences in "public" for anon,
	// authenticated and service_role, which is the checkbox "Automatically expose new tables" of
	// Studio's create-project form (data_api_revoke_default_privileges: false). False revokes them.
	ExposeNewTables bool
	// APISchema also creates the dedicated schema "api" (data_api_use_api_schema).
	APISchema bool
	// ExposedSchemas is the comma-separated PostgREST db_schema to save once the schemas exist
	// (PostgREST restarts on it); empty keeps the default (public,graphql_public).
	ExposedSchemas string
	// CopyFrom is the ref of the project a seeded create (a restore as new, a data branch) takes the
	// recorded choice from. The seed carries the database, default privileges included, so no SQL
	// runs; only the registry row is copied. Without a row to copy the new project is converged like
	// an existing one at its first pass.
	CopyFrom string
	// CopyChoice is the decision to record for a seeded create when CopyFrom has none (the source is
	// deleted; a backup manifest remembers it). Empty records nothing.
	CopyChoice registry.DefaultPrivileges
}

// DataAPISetup is what DataAPIPlane.ApplyDataAPI runs in a project's database.
type DataAPISetup struct {
	// RevokeDefaultPrivileges runs RevokeDefaultPrivilegesSQL.
	RevokeDefaultPrivileges bool
	// APISchema runs APISchemaSQL.
	APISchema bool
	// MustExist lists schemas that have to exist once the statements ran: the exposed schemas of a new
	// project, which PostgREST refuses to start on when one is missing. Nothing is written for it; a
	// missing schema fails the call.
	MustExist []string
}

// Statements lists the SQL of the setup, in order.
func (s DataAPISetup) Statements() []string {
	var out []string
	if s.RevokeDefaultPrivileges {
		out = append(out, RevokeDefaultPrivilegesSQL...)
	}
	if s.APISchema {
		out = append(out, APISchemaSQL...)
	}
	return out
}

// ErrNotPrimary is returned by ApplyDataAPI for a cluster that is in recovery (a standby, or a
// restored clone that has not finished): the change is made on the primary and reaches replicas
// through replication, and a standby refuses the write anyway.
var ErrNotPrimary = errors.New("lifecycle: the cluster is in recovery, so it is not the primary")

// DataAPIPlane is the optional capability of a data plane that can run the Data API statements in a
// project's database; *PostgresPlane has it, and the plane router of internal/placement sends the
// call to the project's home node. An Engine whose plane lacks it skips the step.
type DataAPIPlane interface {
	// ApplyDataAPI runs s as supabase_admin in one transaction, on the primary: ErrNotPrimary
	// for a cluster in recovery. Every statement is safe to run twice.
	ApplyDataAPI(ctx context.Context, p *registry.Project, s DataAPISetup) error
}

// ApplyDataAPI implements DataAPIPlane.
func (pl *PostgresPlane) ApplyDataAPI(ctx context.Context, p *registry.Project, s DataAPISetup) error {
	stmts := s.Statements()
	if len(stmts) == 0 && len(s.MustExist) == 0 {
		return nil
	}
	c, err := connect(ctx, socketDSN(pl.paths(p), "postgres"))
	if err != nil {
		return fmt.Errorf("lifecycle: connect to %s: %w", p.Ref, err)
	}
	defer c.Close(context.WithoutCancel(ctx))
	var recovering bool
	if err := c.QueryRow(ctx, "select pg_is_in_recovery()").Scan(&recovering); err != nil {
		return fmt.Errorf("lifecycle: read the recovery state of %s: %w", p.Ref, err)
	}
	if recovering {
		return fmt.Errorf("%w: %s", ErrNotPrimary, p.Ref)
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("lifecycle: %s: %w", q, err)
		}
	}
	for _, name := range s.MustExist {
		var found bool
		if err := tx.QueryRow(ctx, "select exists (select 1 from pg_namespace where nspname = $1)", name).Scan(&found); err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("lifecycle: schema %q does not exist in %s, so the Data API cannot expose it (create it, then save it as an exposed schema)", name, p.Ref)
		}
	}
	return tx.Commit(ctx)
}

var _ DataAPIPlane = (*PostgresPlane)(nil)

// SettingsWriter is the part of *projectconfig.Manager the Engine needs to save a setting of a
// project it has just registered (the exposed schemas of a create).
type SettingsWriter interface {
	Patch(ctx context.Context, ref string, svc projectconfig.Service, patch map[string]any, cx projectconfig.CrossContext) (*projectconfig.Change, error)
}

// Events of the Data API step.
const (
	EventDataAPIRevoked = "project.data_api_defaults_revoked"
	EventDataAPIFailed  = "project.data_api_defaults_failed"
)

// dataAPIAtCreate runs after a project's cluster is up: it applies the choices of req.DataAPI and
// records the decision. A seeded create (restore as new, data branch) runs no SQL, because the
// seed is a copy of a database that has its own default privileges; it copies the row of
// CopyFrom so the copy is not converged again.
func (e *Engine) dataAPIAtCreate(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, req CreateRequest) error {
	store := registry.DataAPIOf(e.reg)
	o := req.DataAPI
	if req.Seed != nil {
		if store == nil {
			return nil
		}
		choice := o.CopyChoice
		if o.CopyFrom != "" {
			src, err := store.GetDataAPI(ctx, o.CopyFrom)
			switch {
			case err == nil:
				choice = src.DefaultPrivileges
			case !errors.Is(err, registry.ErrNotFound):
				return fmt.Errorf("read the choice of %s: %w", o.CopyFrom, err)
			}
		}
		if choice != registry.PrivilegesGranted && choice != registry.PrivilegesRevoked {
			return nil // nothing to copy: the first pass decides, as for a project from before
		}
		return store.PutDataAPI(ctx, registry.DataAPI{Ref: p.Ref, DefaultPrivileges: choice, Source: registry.DataAPICopy})
	}
	choice := registry.PrivilegesRevoked
	if o.ExposeNewTables {
		choice = registry.PrivilegesGranted
	}
	setup := DataAPISetup{RevokeDefaultPrivileges: choice == registry.PrivilegesRevoked, APISchema: o.APISchema}
	exposed := splitSchemas(o.ExposedSchemas)
	setup.MustExist = exposed
	if len(setup.Statements()) > 0 || len(setup.MustExist) > 0 {
		dp, ok := e.plane.(DataAPIPlane)
		if !ok {
			return nil // a plane that cannot run it: nothing is recorded, a later pass decides
		}
		if err := dp.ApplyDataAPI(ctx, p, setup); err != nil {
			return err
		}
	}
	// PostgREST started on the default schemas, because it will not start on one that does not exist yet
	// (the schema api, a schema of the caller's). Now that they do, it learns the list.
	if len(exposed) > 0 {
		if err := e.exposeSchemasAtCreate(ctx, p.Ref, o.ExposedSchemas); err != nil {
			return err
		}
		if cp, ok := e.plane.(ConfigPlane); ok {
			if err := cp.ReconfigureService(ctx, p, keys, config.SvcPostgREST); err != nil {
				return fmt.Errorf("restart PostgREST on the exposed schemas: %w", err)
			}
		}
	}
	if store == nil {
		return nil
	}
	return store.PutDataAPI(ctx, registry.DataAPI{Ref: p.Ref, DefaultPrivileges: choice, Source: registry.DataAPICreate})
}

func splitSchemas(list string) []string {
	var out []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// exposeSchemasAtCreate saves the exposed schemas of a new project as its PostgREST db_schema.
func (e *Engine) exposeSchemasAtCreate(ctx context.Context, ref, schemas string) error {
	if schemas == "" {
		return nil
	}
	w, ok := e.opts.Settings.(SettingsWriter)
	if !ok {
		return errors.New("this node cannot save the exposed schemas of a project (no settings store)")
	}
	_, err := w.Patch(ctx, ref, projectconfig.PostgREST, map[string]any{"db_schema": schemas}, projectconfig.CrossContext{})
	return err
}

// ConvergeDataAPI brings every running project that no release looked at before to hosted's default:
// the default privileges of "public" are revoked for anon, authenticated and service_role, once,
// and the registry records it (supavise.project_data_api, source "upgrade"). A project with a row
// is never touched again, so a user who rolled the revoke back with SQL keeps the rollback.
// Existing tables keep their grants. The leader's Engine drives every project, through the plane
// router for one homed on another node; the SQL runs on the primary only, and replicas get it by
// replication. A follower's Engine, whose registry is read-only, does nothing. The result holds the
// projects that could not be done now (the cluster does not answer, the home node is behind): they
// are tried again at the next call.
func (e *Engine) ConvergeDataAPI(ctx context.Context) map[string]error {
	failed := map[string]error{}
	if e.opts.ReadOnly || registry.DataAPIOf(e.reg) == nil {
		return failed
	}
	ps, err := e.reg.ListProjects(ctx)
	if err != nil {
		failed[""] = err
		return failed
	}
	for _, listed := range ps {
		if listed.Ref == config.SystemRef || !listed.Status.Running() || !e.drives(&listed) {
			continue
		}
		if err := e.convergeOne(ctx, listed.Ref); err != nil {
			failed[listed.Ref] = err
		}
	}
	return failed
}

// convergeTimeout bounds the step for one project, so that a project whose lock another operation
// holds, or whose cluster hangs, does not hold up the others; the next pass tries it again.
const convergeTimeout = 2 * time.Minute

func (e *Engine) convergeOne(ctx context.Context, ref string) error {
	ctx, cancel := context.WithTimeout(ctx, convergeTimeout)
	defer cancel()
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	// Decide on the row as it is now: a pause or a delete may have landed since the listing.
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return nil
		}
		return err
	}
	if !p.Status.Running() {
		return nil
	}
	return e.convergeLocked(ctx, p)
}

// convergeLocked is the step of ConvergeDataAPI for one project whose lock the caller holds and
// whose cluster answers. A failure is recorded as an event and returned; nothing is recorded as
// decided, so the next pass tries again.
func (e *Engine) convergeLocked(ctx context.Context, p *registry.Project) error {
	store := registry.DataAPIOf(e.reg)
	if store == nil || e.opts.ReadOnly || p.Ref == config.SystemRef {
		return nil
	}
	if _, err := store.GetDataAPI(ctx, p.Ref); err == nil {
		return nil
	} else if !errors.Is(err, registry.ErrNotFound) {
		return fmt.Errorf("read the Data API choice of %s: %w", p.Ref, err)
	}
	dp, ok := e.plane.(DataAPIPlane)
	if !ok {
		return nil
	}
	if err := dp.ApplyDataAPI(ctx, p, DataAPISetup{RevokeDefaultPrivileges: true}); err != nil {
		if errors.Is(err, ErrNotSupported) {
			return nil // applied on the home node, which is not this one
		}
		e.event(ctx, p.Ref, EventDataAPIFailed, map[string]string{"error": err.Error()})
		return fmt.Errorf("revoke the default privileges of %s: %w", p.Ref, err)
	}
	if err := store.PutDataAPI(ctx, registry.DataAPI{Ref: p.Ref, DefaultPrivileges: registry.PrivilegesRevoked, Source: registry.DataAPIUpgrade}); err != nil {
		return fmt.Errorf("record the Data API choice of %s: %w", p.Ref, err)
	}
	e.event(ctx, p.Ref, EventDataAPIRevoked, map[string]string{"source": registry.DataAPIUpgrade})
	return nil
}

// DataAPIChoice returns the recorded default privileges of ref: PrivilegesGranted or
// PrivilegesRevoked, and false when nothing is recorded (a project no pass has looked at, or a
// registry without the table).
func (e *Engine) DataAPIChoice(ctx context.Context, ref string) (registry.DefaultPrivileges, bool) {
	store := registry.DataAPIOf(e.reg)
	if store == nil {
		return "", false
	}
	d, err := store.GetDataAPI(ctx, ref)
	if err != nil {
		return "", false
	}
	return d.DefaultPrivileges, true
}

// ParseExposedSchemas turns the list of data_api_exposed_schemas into the db_schema setting. A nil
// list keeps the default; an empty list is the Data API off, which a node cannot do (see
// DisabledDataAPISchema); the names are checked as the PostgREST settings check them. apiSchema
// puts "api" first when the list does not name it.
func ParseExposedSchemas(list []string, given, apiSchema bool) (string, error) {
	if !given {
		if apiSchema {
			return "api,graphql_public", nil
		}
		return "", nil
	}
	var names []string
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			names = append(names, s)
		}
	}
	if len(names) == 0 {
		if apiSchema {
			return "api," + DisabledDataAPISchema, nil
		}
		return DisabledDataAPISchema, nil
	}
	if apiSchema {
		has := false
		for _, n := range names {
			has = has || n == "api"
		}
		if !has {
			names = append([]string{"api"}, names...)
		}
	}
	joined := strings.Join(names, ",")
	f, ok := projectconfig.PostgRESTSchema.Field("db_schema")
	if !ok {
		return "", errors.New("lifecycle: the PostgREST schema has no db_schema")
	}
	c, err := f.Coerce(joined)
	if err == nil && f.Check != nil {
		err = f.Check(c)
	}
	if err != nil {
		return "", fmt.Errorf("data_api_exposed_schemas: %w", err)
	}
	return joined, nil
}
