package lifecycle

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/projectconfig"
)

// SettingsReader is the part of *projectconfig.Manager the Engine uses to read the saved settings of
// a project (the exposed schemas of a branch's parent).
type SettingsReader interface {
	Get(ctx context.Context, ref string, svc projectconfig.Service) (*projectconfig.State, error)
}

// defaultExposedSchemas is the db_schema PostgREST runs on when none is saved (projectconfig's
// default).
const defaultExposedSchemas = "public,graphql_public"

// schemaProbeTimeout bounds the question of which schemas a database has.
const schemaProbeTimeout = 10 * time.Second

// MissingSchemas returns, in the order given, the names that are not schemas of the database the dsn
// opens. PostgREST does not load its schema cache when a schema it is told to expose is missing, so
// a list is checked with it before it is saved.
func MissingSchemas(ctx context.Context, dsn string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	rows, err := conn.Query(ctx, `select n from unnest($1::text[]) with ordinality as t(n, i)
		where not exists (select 1 from pg_namespace where nspname = n) order by i`, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var missing []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		missing = append(missing, n)
	}
	return missing, rows.Err()
}

// SavedExposedSchemas returns the db_schema saved for the PostgREST of ref, "" when it keeps the
// default (or the Engine has no settings store).
func (e *Engine) SavedExposedSchemas(ctx context.Context, ref string) (string, error) {
	r, ok := e.opts.Settings.(SettingsReader)
	if !ok {
		return "", nil
	}
	st, err := r.Get(ctx, ref, projectconfig.PostgREST)
	if err != nil {
		return "", err
	}
	return st.Set.Str(projectconfig.PostgRESTExposedSchemas), nil
}

// SetExposedSchemas saves list as the db_schema of the PostgREST of ref, and an empty list returns
// it to the default. It does not restart PostgREST: the next start reads it. A branch whose schemas
// are not built yet starts on the default and keeps the list aside this way (branching).
func (e *Engine) SetExposedSchemas(ctx context.Context, ref, list string) error {
	w, ok := e.opts.Settings.(SettingsWriter)
	if !ok {
		return nil
	}
	var v any
	if list = strings.Join(splitSchemas(list), ","); list != "" {
		v = list
	}
	_, err := w.Patch(ctx, ref, projectconfig.PostgREST, map[string]any{projectconfig.PostgRESTExposedSchemas: v}, projectconfig.CrossContext{})
	return err
}

// ExposeSchemasThatExist saves the names of list that are schemas of ref's database as its PostgREST
// db_schema, in the order of the list, and restarts PostgREST on them. The names that are not
// schemas (yet) are returned in missing and left out: a list a parent exposes is a request for a
// branch whose schema may not have everything the parent has. Nothing is saved or restarted when
// none of the names exists or when the list is what PostgREST already runs on.
func (e *Engine) ExposeSchemasThatExist(ctx context.Context, ref, list string) (exposed []string, missing []string, err error) {
	names := splitSchemas(list)
	if len(names) == 0 {
		return nil, nil, nil
	}
	if missing, err = e.probeSchemas(ctx, ref, names); err != nil {
		return nil, nil, fmt.Errorf("lifecycle: look for the exposed schemas in %s: %w", ref, err)
	}
	gone := map[string]bool{}
	for _, n := range missing {
		gone[n] = true
	}
	for _, n := range names {
		if !gone[n] {
			exposed = append(exposed, n)
		}
	}
	if len(exposed) == 0 {
		return nil, missing, nil
	}
	want := strings.Join(exposed, ",")
	cur, err := e.SavedExposedSchemas(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	if cur == "" {
		cur = defaultExposedSchemas
	}
	if cur == want {
		return exposed, missing, nil
	}
	if _, ok := e.opts.Settings.(SettingsWriter); !ok {
		return nil, nil, fmt.Errorf("lifecycle: this node cannot save the exposed schemas of %s (no settings store)", ref)
	}
	if err := e.SetExposedSchemas(ctx, ref, want); err != nil {
		return nil, nil, err
	}
	if _, err := e.ApplyConfig(ctx, ref, projectconfig.PostgREST, ApplyOptions{}); err != nil {
		return nil, nil, fmt.Errorf("lifecycle: restart PostgREST of %s on the exposed schemas: %w", ref, err)
	}
	return exposed, missing, nil
}

// probeSchemas asks ref's database which of names are not schemas.
func (e *Engine) probeSchemas(ctx context.Context, ref string, names []string) ([]string, error) {
	if e.schemaProbe != nil {
		return e.schemaProbe(ctx, ref, names)
	}
	dsn, err := e.ConnString(ctx, ref, RolePostgres)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, schemaProbeTimeout)
	defer cancel()
	return MissingSchemas(cctx, dsn, names)
}
