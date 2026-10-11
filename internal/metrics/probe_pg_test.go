package metrics

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The probe's statements run on the Postgres of the CI job (SUPAVISE_TEST_DATABASE_URL, a superuser,
// as supabase_admin is). It checks that they parse and return what the collector reads from
// them, on the version the project pins.
func TestProbeStatementsRunOnPostgres(t *testing.T) {
	url := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)

	p, err := probeConn(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	// This session is not counted; max_connections, the databases and the WAL are there.
	if p.MaxConnections < 1 || p.DBSize <= 0 || p.WALSize <= 0 || p.Backends < 0 || p.Other < 0 {
		t.Fatalf("%+v", p)
	}
	if p.Postgres+p.Authenticator+p.Admin+p.AuthAdmin+p.StorageAdmin+p.Other != p.Backends {
		t.Fatalf("the roles do not add up to the backends: %+v", p)
	}
	if p.IO == nil {
		t.Fatal("pg_stat_io or pg_stat_wal could not be read (Postgres 16 or later has both)")
	}
	if p.IO.Reads < 0 || p.IO.Writes < 0 || p.IO.WALBytes <= 0 {
		t.Fatalf("%+v", p.IO)
	}

	// A second session is a client backend (other tests open and close sessions on the same server, so
	// the count cannot be compared with the first), and the counters never go backward.
	c2, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close(ctx)
	q, err := probeConn(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if q.Backends < 1 {
		t.Fatalf("%d backends with another session open", q.Backends)
	}
	if q.IO.WALBytes < p.IO.WALBytes || q.IO.Reads < p.IO.Reads {
		t.Fatalf("counters went backward: %+v then %+v", p.IO, q.IO)
	}
}
