package schemadiff

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The tests below read and change real databases on the Postgres of SUPAVISE_TEST_DATABASE_URL (a
// superuser). Each test creates its databases and drops them when it ends.

var dbSeq atomic.Int64

func adminURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if u == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL is not set")
	}
	return u
}

// managed stands in for what the services create in a Supabase database: the API roles, the auth
// and storage tables users hang policies and triggers on, and the extensions schema.
const managed = `
create schema if not exists extensions;
create schema auth;
create schema storage;
create table auth.users (id uuid primary key default gen_random_uuid(), email text);
create table storage.buckets (id text primary key, name text);
create table storage.objects (id uuid primary key default gen_random_uuid(), bucket_id text references storage.buckets (id), name text);
alter table storage.objects enable row level security;
create function auth.uid() returns uuid language sql stable as $$ select null::uuid $$;
grant usage on schema public, auth, storage to anon, authenticated, service_role;
alter default privileges for role postgres in schema public grant all on tables to anon, authenticated, service_role;
alter default privileges for role postgres in schema public grant all on sequences to anon, authenticated, service_role;
alter default privileges for role postgres in schema public grant all on functions to anon, authenticated, service_role;
`

func newDB(t *testing.T, setup ...string) (*pgx.Conn, string) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	for _, r := range []string{"anon", "authenticated", "service_role"} {
		_, _ = admin.Exec(ctx, "create role "+r+" nologin")
	}
	name := fmt.Sprintf("sd_%d_%d", time.Now().UnixNano()%1_000_000_000, dbSeq.Add(1))
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := pgx.Connect(ctx, adminURL(t))
		if err != nil {
			return
		}
		defer a.Close(ctx)
		_, _ = a.Exec(ctx, "drop database if exists "+name+" with (force)")
	})
	cfg, err := pgx.ParseConfig(adminURL(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(ctx) })
	for _, s := range append([]string{managed}, setup...) {
		if _, err := c.Exec(ctx, s); err != nil {
			t.Fatalf("setup: %v\n%s", err, s)
		}
	}
	return c, name
}

func snapshot(t *testing.T, c *pgx.Conn, o ReadOptions) *Snapshot {
	t.Helper()
	ctx := context.Background()
	tx, err := c.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local search_path = pg_catalog"); err != nil {
		t.Fatal(err)
	}
	s, err := Read(ctx, tx, o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func apply(t *testing.T, c *pgx.Conn, r *Result) {
	t.Helper()
	if HasUnsupported(r.SQL()) {
		t.Fatalf("unsupported:\n%s", r.SQL())
	}
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range r.Statements {
		if _, err := tx.Exec(ctx, s); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("applying %q: %v\nfull plan:\n%s", s, err, r.SQL())
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// converge applies the diff of two schemas to a database that has the first and checks that it
// then reads like the second.
func converge(t *testing.T, a, b []string) (*Result, *Snapshot) {
	t.Helper()
	ca, _ := newDB(t, a...)
	cb, _ := newDB(t, b...)
	sa, sb := snapshot(t, ca, ReadOptions{}), snapshot(t, cb, ReadOptions{})
	r := Diff(sa, sb, Options{})
	if r.Empty() {
		t.Fatalf("the schemas differ but the diff is empty")
	}
	apply(t, ca, r)
	sa2 := snapshot(t, ca, ReadOptions{})
	if again := Diff(sa2, sb, Options{}); !again.Empty() {
		t.Fatalf("after applying:\n%s\nthe databases still differ:\n%s", r.SQL(), again.SQL())
	}
	return r, sb
}

const baseSchema = `
create extension if not exists pgcrypto with schema extensions;
create table public.todos (id bigserial primary key, title text not null, owner uuid default gen_random_uuid());
alter table public.todos enable row level security;
create policy todos_read on public.todos for select to authenticated using (true);
create function public.touch() returns trigger language plpgsql as $$ begin return new; end $$;
create trigger todos_touch before update on public.todos for each row execute function public.touch();
create index todos_title on public.todos (title);
create policy "bucket read" on storage.objects for select to anon using (bucket_id = 'public');
create function public.on_signup() returns trigger language plpgsql security definer as $$ begin return new; end $$;
create trigger on_auth_user_created after insert on auth.users for each row execute function public.on_signup();
`

func TestEmptyDatabaseReadsEmptyUserSchema(t *testing.T) {
	c, _ := newDB(t)
	s := snapshot(t, c, ReadOptions{})
	for _, o := range s.Objects {
		switch o.Kind {
		case KindSchema, KindDefACL, KindACL, KindExtension, KindComment:
		default:
			t.Errorf("a database with only the managed schemas reads object %s", o.Key)
		}
	}
}

func TestSnapshotsOfEqualSchemasAreEqual(t *testing.T) {
	a, _ := newDB(t, baseSchema)
	b, _ := newDB(t, baseSchema)
	sa, sb := snapshot(t, a, ReadOptions{}), snapshot(t, b, ReadOptions{})
	if sa.Fingerprint() != sb.Fingerprint() {
		t.Fatalf("same schema, different snapshots:\n%s", Diff(sa, sb, Options{}).SQL())
	}
	if r := Diff(sa, sb, Options{}); !r.Empty() {
		t.Fatalf("diff of equal schemas: %s", r.SQL())
	}
	// A policy on storage.objects and a trigger on auth.users are part of it.
	for _, k := range []string{"policy|storage|objects|bucket read", "trigger|auth|users|on_auth_user_created", "table|public|todos", "extension|pgcrypto"} {
		if sa.index()[k] == nil {
			t.Errorf("snapshot lacks %s", k)
		}
	}
	if sa.index()["table|storage|objects"] != nil || sa.index()["table|auth|users"] != nil {
		t.Errorf("the tables of the managed schemas are part of the snapshot")
	}
	if b, err := sa.Marshal(); err != nil {
		t.Fatal(err)
	} else if back, err := Unmarshal(b); err != nil || back.Fingerprint() != sa.Fingerprint() {
		t.Fatalf("round trip: %v", err)
	}
}

// The services of a full node put triggers of their own on their tables (Storage's on storage.objects), and
// a user puts triggers on the same tables (on auth.users, on storage.objects). The snapshot holds the user's
// and not the services', so a baseline made from it carries what the user built and does not try to create
// what the branch's own Storage creates when it starts.
func TestServiceTriggersOnManagedTablesAreLeftOutOfTheSnapshot(t *testing.T) {
	c, _ := newDB(t, `
		create function storage.update_updated_at_column() returns trigger language plpgsql as $$ begin return new; end $$;
		create trigger update_objects_updated_at before update on storage.objects for each row execute function storage.update_updated_at_column();
		create function public.audit() returns trigger language plpgsql security definer as $$ begin return new; end $$;
		create trigger objects_audit after insert on storage.objects for each row execute function public.audit();
		create trigger on_auth_user_created after insert on auth.users for each row execute function public.audit();`)
	idx := snapshot(t, c, ReadOptions{}).index()
	if idx["trigger|storage|objects|update_objects_updated_at"] != nil {
		t.Error("the snapshot holds the trigger of Storage")
	}
	for _, k := range []string{"trigger|storage|objects|objects_audit", "trigger|auth|users|on_auth_user_created"} {
		if idx[k] == nil {
			t.Errorf("the snapshot lacks the user's %s", k)
		}
	}
}

func TestBaselineBuildsAnEmptyDatabase(t *testing.T) {
	src, _ := newDB(t, baseSchema, `
		create type public.mood as enum ('sad', 'ok', 'happy');
		create domain public.posint as integer check (value > 0);
		create table public.profiles (id uuid primary key, mood public.mood not null default 'ok', score public.posint, created_at timestamptz not null default now(),
			full_name text generated always as (id::text) stored);
		create table public.notes (id bigint generated always as identity primary key, body text, author uuid references public.profiles (id) on delete cascade, unique (body));
		comment on table public.notes is 'notes of profiles';
		comment on column public.notes.body is 'the text';
		create view public.v_notes as select n.id, n.body from public.notes n;
		create materialized view public.mv_notes as select id from public.notes;
		create index mv_notes_id on public.mv_notes (id);
		create sequence public.standalone start 100 increment 5;
		create function public.f(a int) returns setof public.notes language sql stable as $$ select * from public.notes where id = a $$;
		create schema extra;
		create table extra.t (a int);
		grant select on public.notes to anon;
		revoke all on public.todos from authenticated;
		revoke execute on function public.touch() from public;
	`)
	dst, _ := newDB(t)
	ss := snapshot(t, src, ReadOptions{})
	r := Diff(nil, ss, Options{})
	if HasUnsupported(r.SQL()) {
		t.Fatalf("unsupported: %s", r.SQL())
	}
	apply(t, dst, r)
	sd := snapshot(t, dst, ReadOptions{})
	if again := Diff(sd, ss, Options{}); !again.Empty() {
		t.Fatalf("the baseline left differences:\n%s\n--- baseline:\n%s", again.SQL(), r.SQL())
	}
}

func TestConvergeTables(t *testing.T) {
	r, _ := converge(t, []string{baseSchema}, []string{baseSchema, `
		create table public.notes (id int primary key, body text not null default 'x');
		alter table public.todos add column done boolean not null default false;
		alter table public.todos alter column title drop not null;
		drop policy todos_read on public.todos;
		create policy todos_write on public.todos for all to authenticated using (owner is not null) with check (true);
		drop index public.todos_title;
		create index todos_done on public.todos (done);
		create policy "extra" on storage.objects for insert to authenticated with check (bucket_id = 'x');
		create trigger t2 after update on auth.users for each row execute function public.on_signup();
	`})
	sql := r.SQL()
	for _, want := range []string{`CREATE TABLE "public"."notes"`, `ADD COLUMN "done" boolean DEFAULT false NOT NULL`, `DROP POLICY "todos_read"`, `CREATE POLICY "extra"`, `CREATE TRIGGER t2`, `DROP NOT NULL`} {
		if !strings.Contains(sql, want) {
			t.Errorf("the diff lacks %q:\n%s", want, sql)
		}
	}
}

func TestConvergeDropsAndAlters(t *testing.T) {
	converge(t, []string{baseSchema, `
		create table public.a (id int primary key, n int);
		create table public.b (id int primary key, a_id int references public.a (id));
		create view public.va as select id from public.a;
		create type public.color as enum ('red', 'blue');
		create sequence public.s1;
	`}, []string{baseSchema, `
		create table public.a (id bigint primary key, n int not null);
		create type public.color as enum ('red', 'green', 'blue');
		create sequence public.s1 increment 2 cycle;
	`})
}

func TestConvergeFunctionsViewsAndDefaults(t *testing.T) {
	converge(t, []string{baseSchema, `
		create function public.next_code() returns text language sql as $$ select 'a' $$;
		create table public.codes (id int primary key, code text default public.next_code());
		create view public.vcodes as select id from public.codes;
	`}, []string{baseSchema, `
		create function public.next_code() returns text language sql as $$ select 'b' $$;
		create function public.other() returns text language sql as $$ select 'c' $$;
		create table public.codes (id int primary key, code text default public.other(), extra text default public.other());
		create view public.vcodes as select id, code from public.codes;
		create table public.uses_fn (id int primary key, c text not null default public.next_code());
	`})
}

func TestConvergeGrantsAndComments(t *testing.T) {
	converge(t, []string{baseSchema, `create table public.g (id int primary key);`}, []string{baseSchema, `
		create table public.g (id int primary key);
		revoke all on public.g from anon;
		grant select on public.g to authenticated with grant option;
		comment on table public.g is 'a table';
		create table public.h (id int);
		revoke insert, update on public.h from authenticated;
		create function public.locked() returns int language sql as $$ select 1 $$;
		revoke execute on function public.locked() from public, anon;
	`})
}

func TestDiffIgnoresUnmanagedDifferencesInManagedSchemas(t *testing.T) {
	a, _ := newDB(t, baseSchema)
	b, _ := newDB(t, baseSchema, `alter table storage.buckets add column extra int; create table auth.more (id int); create function auth.helper() returns int language sql as $$ select 1 $$;`)
	sa, sb := snapshot(t, a, ReadOptions{}), snapshot(t, b, ReadOptions{})
	if r := Diff(sa, sb, Options{}); !r.Empty() {
		t.Fatalf("changes to the tables of managed schemas are part of the diff:\n%s", r.SQL())
	}
}

func TestIncludedSchemasAreHonored(t *testing.T) {
	a, _ := newDB(t, baseSchema)
	b, _ := newDB(t, baseSchema, `create table public.only_b (id int); create schema other; create table other.t (id int);`)
	sa, sb := snapshot(t, a, ReadOptions{Schemas: []string{"other"}}), snapshot(t, b, ReadOptions{Schemas: []string{"other"}})
	r := Diff(sa, sb, Options{})
	if !strings.Contains(r.SQL(), `"other"."t"`) || strings.Contains(r.SQL(), "only_b") {
		t.Fatalf("included_schemas=other:\n%s", r.SQL())
	}
}

func TestUnmodeledObjectsAreNamed(t *testing.T) {
	a, _ := newDB(t, baseSchema)
	b, _ := newDB(t, baseSchema, `create aggregate public.agg(int) (sfunc = int4pl, stype = int4);`)
	r := Diff(snapshot(t, a, ReadOptions{}), snapshot(t, b, ReadOptions{}), Options{})
	if len(r.Unsupported) != 1 || !strings.Contains(r.SQL(), UnsupportedMarker) {
		t.Fatalf("an aggregate is not named:\n%s", r.SQL())
	}
}

func TestBaselineLeavesOutRolesTheTargetLacks(t *testing.T) {
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL(t))
	if err != nil {
		t.Fatal(err)
	}
	role := fmt.Sprintf("reporting_%d", time.Now().UnixNano()%1_000_000)
	if _, err := admin.Exec(ctx, "create role "+role+" nologin"); err != nil {
		t.Fatal(err)
	}
	// the databases are dropped first (cleanups run last in, first out), then the role
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "drop role if exists "+role)
		admin.Close(ctx)
	})
	src, _ := newDB(t, `create table public.rep (id int); grant select on public.rep to `+role+`;
		create policy p on public.rep for select to `+role+` using (true); alter table public.rep enable row level security;`)
	ss := snapshot(t, src, ReadOptions{})
	r := Diff(nil, ss, Options{RoleExists: func(n string) bool { return n != role }})
	sql := strings.Join(r.Statements, "\n")
	if strings.Contains(sql, role) || len(r.Notes) == 0 || !strings.Contains(r.SQL(), "-- note:") {
		t.Fatalf("a role the target lacks is in the baseline (or not mentioned in the notes %v):\n%s", r.Notes, sql)
	}
	dst, _ := newDB(t)
	apply(t, dst, r)
}

func TestConvergeDropsOfTablesViewsAndTypes(t *testing.T) {
	converge(t, []string{baseSchema, `
		create type public.color as enum ('red', 'blue');
		create table public.a (id int primary key, c public.color);
		create table public.b (id int primary key, a_id int references public.a (id));
		create view public.va as select id from public.a;
		create view public.vb as select b.id from public.b join public.va on va.id = b.a_id;
		create materialized view public.mva as select id from public.a;
		create index mva_i on public.mva (id);
		create function public.fa() returns int language sql as $$ select 1 $$;
		create schema gone;
		create table gone.t (id int);
		grant select on public.a to anon;
		comment on table public.a is 'tbl a';
	`}, []string{baseSchema, `
		create type public.color as enum ('red', 'green', 'blue');
		create table public.keep (id int primary key, c public.color);
	`})
}

func TestConvergeMatViewAndOptions(t *testing.T) {
	converge(t, []string{baseSchema, `
		create table public.src (id int primary key, v text);
		create materialized view public.mv as select id from public.src;
		create index mv_i on public.mv (id);
		create view public.vv as select id from public.src;
		create unlogged table public.u (id int);
	`}, []string{baseSchema, `
		create table public.src (id int primary key, v text) with (fillfactor = 70);
		create materialized view public.mv as select id, v from public.src;
		create index mv_i2 on public.mv (v);
		create view public.vv with (security_invoker = true) as select id, v from public.src;
		create table public.u (id int);
	`})
}

func TestConvergePartitionsAndGenerated(t *testing.T) {
	converge(t, []string{baseSchema}, []string{baseSchema, `
		create table public.events (id bigint not null, at date not null, body text, doubled bigint generated always as (id * 2) stored) partition by range (at);
		create table public.events_2026 partition of public.events for values from ('2026-01-01') to ('2027-01-01');
		create table public.events_2027 partition of public.events for values from ('2027-01-01') to ('2028-01-01');
		create index events_at on public.events (at);
	`})
}

func TestPlanOfAdditionsLeavesRemovalsAlone(t *testing.T) {
	a, _ := newDB(t, baseSchema, `create table public.only_a (id int)`)
	b, _ := newDB(t, baseSchema, `create table public.only_b (id int)`)
	r := Diff(snapshot(t, a, ReadOptions{}), snapshot(t, b, ReadOptions{}), Options{KeepRemoved: true})
	if strings.Contains(r.SQL(), "DROP") || !strings.Contains(r.SQL(), "only_b") {
		t.Fatalf("KeepRemoved:\n%s", r.SQL())
	}
}

func TestConvergeColumnTypeChangeWithADefault(t *testing.T) {
	converge(t, []string{baseSchema, `create table public.c (id int primary key, n integer default 5, s text default 'x')`},
		[]string{baseSchema, `create table public.c (id int primary key, n bigint default 5, s varchar(20) default 'x')`})
}

func TestConvergeIdentityAndNotNull(t *testing.T) {
	converge(t, []string{baseSchema, `create table public.i (id int primary key, a int, b int not null)`},
		[]string{baseSchema, `create table public.i (id int generated by default as identity primary key, a int not null, b int)`})
}

// A schema like the ones people build on Supabase: foreign keys into auth.users, policies that call
// auth.uid(), a trigger on auth.users that fills a profile, enums, arrays, partial and expression
// indexes, a view, defaults from extensions.
const typicalApp = `
create extension if not exists "uuid-ossp" with schema extensions;
create type public.role as enum ('member', 'admin');
create table public.profiles (
  id uuid primary key references auth.users (id) on delete cascade,
  username text unique not null check (length(username) >= 3),
  role public.role not null default 'member',
  tags text[] not null default '{}',
  settings jsonb not null default '{}'::jsonb,
  created_at timestamptz not null default now()
);
comment on table public.profiles is 'one row per user';
alter table public.profiles enable row level security;
create policy "own profile" on public.profiles for select to authenticated using (auth.uid() = id);
create policy "update own" on public.profiles for update to authenticated using (auth.uid() = id) with check (auth.uid() = id);
create table public.posts (
  id uuid primary key default extensions.uuid_generate_v4(),
  author uuid not null references public.profiles (id) on delete cascade,
  body text,
  published boolean not null default false,
  score numeric(10, 2) default 0
);
create index posts_author_idx on public.posts (author) where published;
create index posts_body_lower on public.posts (lower(body));
create index posts_settings on public.profiles using gin (settings);
alter table public.posts enable row level security;
alter table public.posts force row level security;
create policy "read published" on public.posts for select using (published or author = auth.uid());
create function public.handle_new_user() returns trigger language plpgsql security definer set search_path = '' as $$
begin insert into public.profiles (id, username) values (new.id, split_part(new.email, '@', 1)); return new; end $$;
create trigger on_auth_user_created after insert on auth.users for each row execute function public.handle_new_user();
create view public.published_posts with (security_invoker = true) as select id, author, body from public.posts where published;
create function public.post_count(p uuid) returns bigint language sql stable as $$ select count(*) from public.posts where author = p $$;
grant select on public.published_posts to anon;
revoke all on public.posts from anon;
`

func TestTypicalAppBaselineAndEvolution(t *testing.T) {
	src, _ := newDB(t, typicalApp)
	dst, _ := newDB(t)
	ss := snapshot(t, src, ReadOptions{})
	apply(t, dst, Diff(nil, ss, Options{}))
	if again := Diff(snapshot(t, dst, ReadOptions{}), ss, Options{}); !again.Empty() {
		t.Fatalf("the baseline of a typical app left differences:\n%s", again.SQL())
	}
	// Then it evolves: a column, a policy, an enum value, a new index, a dropped view.
	evolved, _ := newDB(t, typicalApp, `
		alter type public.role add value 'owner';
		alter table public.profiles add column bio text;
		drop policy "update own" on public.profiles;
		create policy "update own" on public.profiles for update to authenticated using (auth.uid() = id and role <> 'admin') with check (auth.uid() = id);
		create index posts_score on public.posts (score desc nulls last);
		drop view public.published_posts;
		create function public.touch_profile() returns trigger language plpgsql as $$ begin return new; end $$;
		create trigger t_touch before update on public.profiles for each row execute function public.touch_profile();
	`)
	se := snapshot(t, evolved, ReadOptions{})
	r := Diff(snapshot(t, dst, ReadOptions{}), se, Options{})
	apply(t, dst, r)
	if again := Diff(snapshot(t, dst, ReadOptions{}), se, Options{}); !again.Empty() {
		t.Fatalf("after:\n%s\nstill differs:\n%s", r.SQL(), again.SQL())
	}
}
