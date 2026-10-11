package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/lifecycle"
)

// Target is where a project's Postgres listens on its private unix socket.
type Target struct {
	Ref  string
	Sock string // the socket directory (unix_socket_directories)
	Port int
}

// Probe is what one short SQL session reads from a project's cluster.
type Probe struct {
	// Backends counts the client backends other than the probe's own; the next fields split them by
	// role, those that are executing (Active) and those Supavisor opened (Supavisor). Other is what
	// the five named roles leave.
	Backends, Postgres, Authenticator, Admin, AuthAdmin, StorageAdmin, Other, Active, Supavisor int64
	// MaxConnections is the cluster's max_connections.
	MaxConnections int64
	// DBSize is the sum of pg_database_size over every database, WALSize the size of pg_wal.
	DBSize, WALSize int64
	// IO holds the cumulative block counters; nil when the cluster has no pg_stat_io.
	IO *IOCounters
}

// IOCounters are cumulative since the cluster's statistics were last reset (a crash or a restart
// of the cluster resets them, which the collector reads as a counter that went backward).
type IOCounters struct {
	Reads, Writes, ReadBytes, WriteBytes, WALBytes float64
}

// Prober reads a Probe from a cluster.
type Prober interface {
	Probe(ctx context.Context, t Target) (Probe, error)
}

// ProbeTimeout bounds one probe: the connection and both statements.
const ProbeTimeout = 5 * time.Second

// SQLProber probes over the unix socket as supabase_admin, which pg_hba trusts there. It opens a
// connection for each probe and closes it, so a node with many projects holds no connection per
// project between two samples.
type SQLProber struct{}

// connectionsSQL reads everything but the I/O counters in one statement.
const connectionsSQL = `
with a as (
  select usename, application_name, state
  from pg_stat_activity
  where backend_type = 'client backend' and pid <> pg_backend_pid()
)
select
  count(*),
  count(*) filter (where usename = 'postgres'),
  count(*) filter (where usename = 'authenticator'),
  count(*) filter (where usename = 'supabase_admin'),
  count(*) filter (where usename = 'supabase_auth_admin'),
  count(*) filter (where usename = 'supabase_storage_admin'),
  count(*) filter (where state = 'active'),
  count(*) filter (where application_name ilike '%supavisor%'),
  current_setting('max_connections')::bigint,
  (select coalesce(sum(pg_database_size(oid)), 0)::bigint from pg_database),
  (select coalesce(sum(size), 0)::bigint from pg_ls_waldir())
from a`

// ioSQL reads the cumulative block counters. Reads and writes are the block operations Postgres
// asked the operating system for (extends are writes of new blocks); WAL bytes are added to the
// bytes written by the caller.
const ioSQL = `
select
  coalesce(sum(reads), 0)::float8,
  coalesce(sum(coalesce(writes, 0) + coalesce(extends, 0)), 0)::float8,
  coalesce(sum(reads * op_bytes), 0)::float8,
  coalesce(sum((coalesce(writes, 0) + coalesce(extends, 0)) * op_bytes), 0)::float8,
  (select coalesce(wal_bytes, 0)::float8 from pg_stat_wal)
from pg_stat_io
where object = 'relation'`

// Probe implements Prober.
func (SQLProber) Probe(ctx context.Context, t Target) (Probe, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	cc, err := pgx.ParseConfig(lifecycle.AdminSocketDSN(t.Sock, t.Port, "postgres", "supavise-metrics"))
	if err != nil {
		return Probe{}, err
	}
	// A statement that hangs on a lock must not hold the sample past its minute.
	cc.RuntimeParams["statement_timeout"] = "4000"
	c, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return Probe{}, fmt.Errorf("connect: %w", err)
	}
	defer c.Close(context.Background())
	return probeConn(ctx, c)
}

// probeConn runs the probe's statements on an open connection.
func probeConn(ctx context.Context, c *pgx.Conn) (Probe, error) {
	var p Probe
	if err := c.QueryRow(ctx, connectionsSQL).Scan(&p.Backends, &p.Postgres, &p.Authenticator, &p.Admin, &p.AuthAdmin,
		&p.StorageAdmin, &p.Active, &p.Supavisor, &p.MaxConnections, &p.DBSize, &p.WALSize); err != nil {
		return Probe{}, err
	}
	p.Other = max(p.Backends-p.Postgres-p.Authenticator-p.Admin-p.AuthAdmin-p.StorageAdmin, 0)
	// The counters are an extra: a cluster without pg_stat_io (or a role that cannot read it)
	// still reports the rest.
	var io IOCounters
	if err := c.QueryRow(ctx, ioSQL).Scan(&io.Reads, &io.Writes, &io.ReadBytes, &io.WriteBytes, &io.WALBytes); err == nil {
		p.IO = &io
	}
	return p, nil
}
