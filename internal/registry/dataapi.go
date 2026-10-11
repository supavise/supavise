package registry

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// DefaultPrivileges says which way the default privileges of a project's "public" schema point for
// the Data API roles (anon, authenticated, service_role): whether a table created from now on is
// reachable through PostgREST without a grant.
type DefaultPrivileges string

const (
	// PrivilegesGranted is the behavior before hosted Supabase's change: new tables and sequences
	// carry the grants.
	PrivilegesGranted DefaultPrivileges = "granted"
	// PrivilegesRevoked is hosted's default for new projects: a new table needs an explicit grant.
	PrivilegesRevoked DefaultPrivileges = "revoked"
)

// Sources of a DataAPI row.
const (
	// DataAPICreate: the project was created with this choice (or a schema-only branch took its
	// parent's).
	DataAPICreate = "create"
	// DataAPIUpgrade: the project existed before this release and got the revoke when the node first
	// ran it.
	DataAPIUpgrade = "upgrade"
	// DataAPICopy: a restore as new or a data branch took the row of its source, or an in-place restore
	// took the decision its backup recorded.
	DataAPICopy = "copy"
)

// DataAPI is the decision recorded for one project (migration 1400, supavise.project_data_api).
// It is what Supavise did or was told, never a reading of the database: a user who runs the grants
// or the revokes by hand changes the database and leaves the row alone.
type DataAPI struct {
	Ref               string
	DefaultPrivileges DefaultPrivileges
	Source            string
	DecidedAt         time.Time
}

// DataAPIStore keeps the Data API decision of projects. Both registries implement it; the Engine
// reaches it through the Registry it holds.
type DataAPIStore interface {
	// GetDataAPI returns the decision for ref, or ErrNotFound when none was recorded (a project no
	// release that knows the table has looked at, or an unknown ref).
	GetDataAPI(ctx context.Context, ref string) (*DataAPI, error)
	// PutDataAPI records d for d.Ref, replacing an earlier row (a reset branch is created again over
	// its ref). ErrNotFound for an unknown project.
	PutDataAPI(ctx context.Context, d DataAPI) error
	// DeleteDataAPI forgets the decision for ref, so that the project is looked at again by the next
	// pass of the Data API step (an in-place restore to a backup that recorded none). A ref with no
	// decision is not an error.
	DeleteDataAPI(ctx context.Context, ref string) error
}

// DataAPIOf returns reg's DataAPIStore, or nil when it has none.
func DataAPIOf(reg Registry) DataAPIStore {
	s, _ := reg.(DataAPIStore)
	return s
}

var _ DataAPIStore = (*Memory)(nil)
var _ DataAPIStore = (*Postgres)(nil)

func (m *Memory) GetDataAPI(_ context.Context, ref string) (*DataAPI, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.dataAPI[ref]
	if !ok {
		return nil, ErrNotFound
	}
	return &d, nil
}

func (m *Memory) PutDataAPI(_ context.Context, d DataAPI) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[d.Ref]; !ok {
		return ErrNotFound
	}
	if d.DecidedAt.IsZero() {
		d.DecidedAt = time.Now()
	}
	if m.dataAPI == nil {
		m.dataAPI = map[string]DataAPI{}
	}
	m.dataAPI[d.Ref] = d
	return nil
}

func (m *Memory) DeleteDataAPI(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.dataAPI, ref)
	return nil
}

func (r *Postgres) GetDataAPI(ctx context.Context, ref string) (*DataAPI, error) {
	var d DataAPI
	var dp string
	err := r.pool.QueryRow(ctx, `select ref, default_privileges, source, decided_at from supavise.project_data_api where ref = $1`, ref).
		Scan(&d.Ref, &dp, &d.Source, &d.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.DefaultPrivileges = DefaultPrivileges(dp)
	return &d, nil
}

func (r *Postgres) PutDataAPI(ctx context.Context, d DataAPI) error {
	if d.DecidedAt.IsZero() {
		d.DecidedAt = time.Now()
	}
	_, err := r.pool.Exec(ctx, `
		insert into supavise.project_data_api (ref, default_privileges, source, decided_at)
		values ($1, $2, $3, $4)
		on conflict (ref) do update set default_privileges = excluded.default_privileges,
		  source = excluded.source, decided_at = excluded.decided_at`,
		d.Ref, string(d.DefaultPrivileges), d.Source, d.DecidedAt)
	return mapWriteErr(err)
}

func (r *Postgres) DeleteDataAPI(ctx context.Context, ref string) error {
	_, err := r.pool.Exec(ctx, `delete from supavise.project_data_api where ref = $1`, ref)
	return mapWriteErr(err)
}
