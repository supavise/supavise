package branching

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitSQLKeepsQuotedAndDollarQuotedSemicolons(t *testing.T) {
	src := `-- a comment; with a semicolon
create table t (id int, note text default 'a;b');
insert into t values ('it''s; fine'), (E'x\'; y');
/* block; /* nested; */ still comment */
create function f() returns int language plpgsql as $body$ begin perform 1; return 2; end $body$;
select "we;ird";`
	got, ok := splitSQL(src)
	if !ok || len(got) != 4 {
		t.Fatalf("split = %d statements, ok %v: %+v", len(got), ok, got)
	}
	if !strings.HasPrefix(got[1].text, "insert into t") || !strings.HasSuffix(got[1].text, `y')`) {
		t.Fatalf("insert statement = %q", got[1].text)
	}
	if !strings.Contains(got[2].text, "return 2; end") {
		t.Fatalf("function statement = %q", got[2].text)
	}
	if want := []string{"CREATE", "TABLE", "T"}; !reflect.DeepEqual(got[0].words, want) {
		t.Fatalf("words = %v, want %v", got[0].words, want)
	}
}

func TestSplitSQLReadsABeginAtomicBodyAsOneStatement(t *testing.T) {
	src := `create function f(a int) returns int language sql begin atomic
  select case when a > 1 then 1 else 0 end;
  select 2;
end;
insert into t values (1);`
	got, ok := splitSQL(src)
	if !ok || len(got) != 2 {
		t.Fatalf("split = %d, ok %v: %+v", len(got), ok, got)
	}
	if classify(got[0]) != classHeld || classify(got[1]) != classCarried {
		t.Fatalf("classes = %v, %v", classify(got[0]), classify(got[1]))
	}
}

func TestSplitSQLRefusesAScriptThatEndsInsideAQuote(t *testing.T) {
	for _, src := range []string{`select 'open`, `select "open`, `select $x$ open`, `/* open`} {
		if _, ok := splitSQL(src); ok {
			t.Errorf("%q was split", src)
		}
	}
	if st, ok := splitSQL("  ;; -- nothing\n"); !ok || len(st) != 0 {
		t.Errorf("an empty script = %v, %v", st, ok)
	}
}

func TestCarriedStatementsAreWhatADiffDoesNotHold(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{"ddl only", `create table public.t (id int primary key, a int generated always as (id + 1) stored);
alter table public.t enable row level security;
create index on public.t (a);
create policy p on public.t for select using (true);
create or replace function public.f() returns int language sql as $$ select 1 $$;
create trigger tr before insert on public.t for each row execute function public.f();
grant select on public.t to anon;
grant usage on schema public to anon;
revoke all on all tables in schema public from anon;
alter default privileges in schema public grant all on tables to anon;
comment on table public.t is 'x';
comment on column public.t.id is 'y';
create extension if not exists pgcrypto;
create type public.mood as enum ('a', 'b');
drop view if exists public.v;
create materialized view public.mv as select 1;`, nil},
		{"data", `create table public.roles (name text);
insert into public.roles values ('admin');`, []string{`insert into public.roles values ('admin')`}},
		{"bucket and cron", `insert into storage.buckets (id, name) values ('avatars', 'avatars') on conflict do nothing;
select cron.schedule('nightly', '0 3 * * *', $$ delete from public.old $$);`, []string{
			`insert into storage.buckets (id, name) values ('avatars', 'avatars') on conflict do nothing`,
			`select cron.schedule('nightly', '0 3 * * *', $$ delete from public.old $$)`}},
		{"publication and role", `alter publication supabase_realtime add table public.t;
create role reporting nologin;
grant reporting to postgres;
grant select on public.t to reporting;`, []string{`alter publication supabase_realtime add table public.t`, `create role reporting nologin`, `grant reporting to postgres`}},
		{"create table as", `create table public.snap as select * from public.t;`, []string{`create table public.snap as select * from public.t`}},
		{"temporary table", `create temp table scratch (id int);
insert into scratch values (1);`, []string{`create temp table scratch (id int)`, `insert into scratch values (1)`}},
		{"settings come along", `set search_path = app, public;
create table t (id int);
update t set id = 1;
reset search_path;`, []string{`set search_path = app, public`, `update t set id = 1`}},
		{"transaction control is dropped", `begin;
create table t (id int);
do $$ begin perform 1; end $$;
commit;`, []string{`do $$ begin perform 1; end $$`}},
		{"unmodeled objects", `create aggregate public.agg(int) (sfunc = int4pl, stype = int);
create event trigger e on ddl_command_end execute function public.f();
create server s foreign data wrapper w;
alter table public.t add column c int;`, []string{
			`create aggregate public.agg(int) (sfunc = int4pl, stype = int)`,
			`create event trigger e on ddl_command_end execute function public.f()`,
			`create server s foreign data wrapper w`}},
		{"objects of the services' schemas", `alter table auth.users add column plan text;
create table storage.custom (id int);
alter table storage.objects enable row level security;
create index on storage.objects (name);
create function auth.f() returns int language sql as $$ select 1 $$;
create schema if not exists extensions;
grant usage on schema auth to anon;
comment on table auth.users is 'x';
drop table if exists "quoted"."t";
create schema app;`, []string{
			`alter table auth.users add column plan text`,
			`create table storage.custom (id int)`,
			`alter table storage.objects enable row level security`,
			`create index on storage.objects (name)`,
			`create function auth.f() returns int language sql as $$ select 1 $$`,
			`create schema if not exists extensions`,
			`grant usage on schema auth to anon`,
			`comment on table auth.users is 'x'`}},
		{"what the engine reads on the services' tables", `create policy p on storage.objects for select using (true);
create trigger t after insert on auth.users for each row execute function public.f();
drop policy p on storage.objects;
create view public.v as select * from auth.users;
create table public.profiles (id uuid references auth.users (id));`, nil},
		{"grant on a database", `grant connect on database postgres to anon;`, []string{`grant connect on database postgres to anon`}},
	}
	for _, c := range cases {
		got := carriedStatements([]string{c.sql})
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: carried\n%q\nwant\n%q", c.name, got, c.want)
		}
	}
}

func TestCarriedStatementsReadOneElementPerStatementToo(t *testing.T) {
	// The CLI records one element per statement, the Management API one for the whole script.
	m := Migration{Statements: []string{"create table t (id int)", "insert into t values (1)", "alter table t add column c int"}}
	if got := m.carried(); !reflect.DeepEqual(got, []string{"insert into t values (1)"}) {
		t.Fatalf("carried = %q", got)
	}
	// A script that cannot be read is carried as it is, so that it fails loudly instead of being skipped.
	if got := carriedStatements([]string{"create table t (id int); select 'open"}); len(got) != 1 {
		t.Fatalf("an unreadable script = %q", got)
	}
}
