package schemadiff

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Queryer is what Read needs from a connection or a transaction. The caller sets search_path to
// pg_catalog and opens a read-only repeatable-read transaction, so that every catalog function
// qualifies the names it prints and the snapshot is consistent.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ReadOptions select what Read covers.
type ReadOptions struct {
	// Schemas are the schemas to read. Empty reads every user schema: not a schema of a Supabase
	// service (ManagedSchemas), not owned by an extension, and with the policies and triggers users
	// created on tables of the managed schemas (a policy on storage.objects, a trigger on auth.users).
	Schemas []string
}

// ManagedSchemas are the schemas the platform's services own. Their tables are not read, but the
// policies and triggers a user created on them are (Read with no Schemas).
var ManagedSchemas = []string{
	"auth", "storage", "realtime", "extensions", "graphql", "graphql_public", "vault", "pgsodium",
	"pgsodium_masks", "net", "cron", "pgbouncer", "supabase_functions", "supabase_migrations",
	"_realtime", "_analytics", "supavisor", "pgtle", "topology", "tiger", "tiger_data", "repack",
}

// sharedTableSchemas are the managed schemas whose tables users add policies and triggers to.
var sharedTableSchemas = []string{"auth", "storage", "realtime", "vault", "extensions", "graphql", "graphql_public"}

// serviceFunctionSchemas hold the trigger functions of the services themselves: a trigger on a
// managed table that calls one of them is the service's own, not a user's.
var serviceFunctionSchemas = []string{
	"auth", "storage", "realtime", "extensions", "graphql", "graphql_public", "vault", "pgsodium",
	"pgsodium_masks", "net", "cron", "pgbouncer", "_realtime", "_analytics", "supavisor",
}

type ref struct {
	class string
	oid   int64
	sub   int32
}

type reader struct {
	ctx context.Context
	q   Queryer
	o   ReadOptions

	objs  []Object
	byKey map[string]int

	// oidKey maps a catalog entry to the key of the object it stands for; oids lists the entries'
	// oids for the pg_depend query.
	oidKey map[ref]string
	oids   []int64

	scope      []int64 // oids of the schemas read
	scopeNames map[string]bool
	shared     []int64 // oids of the managed schemas users add policies and triggers to
	sharedOn   bool

	tableOids []int64 // tables read (relkind r, p)
	viewOids  []int64
	typeOids  []int64 // pg_type oids of the relations and types read, for the array types
	unmodeled []string
	pg        int
}

func (r *reader) add(o Object) {
	if i, ok := r.byKey[o.Key]; ok {
		r.objs[i] = o
		return
	}
	r.byKey[o.Key] = len(r.objs)
	r.objs = append(r.objs, o)
}

func (r *reader) get(key string) *Object {
	if i, ok := r.byKey[key]; ok {
		return &r.objs[i]
	}
	return nil
}

func (r *reader) reg(class string, oid int64, sub int32, key string) {
	r.oidKey[ref{class, oid, sub}] = key
	r.oids = append(r.oids, oid)
}

func (r *reader) each(sql string, args []any, scan func(rows pgx.Rows) error) error {
	rows, err := r.q.Query(r.ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

const notInExtension = `not exists (select 1 from pg_depend dx where dx.classid = '%s'::regclass and dx.objid = %s.oid and dx.deptype = 'e')`

func notExt(class, alias string) string { return fmt.Sprintf(notInExtension, class, alias) }

// Read reads the schema of the database q is connected to.
func Read(ctx context.Context, q Queryer, o ReadOptions) (snap *Snapshot, err error) {
	defer func() {
		if p := recover(); p != nil {
			snap, err = nil, fmt.Errorf("schemadiff: reading the schema failed unexpectedly: %v", p)
		}
	}()
	r := &reader{ctx: ctx, q: q, o: o, byKey: map[string]int{}, oidKey: map[ref]string{}, scopeNames: map[string]bool{}}
	steps := []func() error{
		r.readVersion, r.readSchemas, r.readExtensions, r.readTypes, r.readTables, r.readColumns,
		r.readSequences, r.readConstraints, r.readViews, r.readIndexes, r.readFunctions, r.readTriggers,
		r.readPolicies, r.readACLs, r.readDefaultACLs, r.readUnmodeled, r.readDeps,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	s := &Snapshot{Version: FormatVersion, PG: r.pg, Objects: r.objs, Unmodeled: r.unmodeled}
	for n := range r.scopeNames {
		s.Schemas = append(s.Schemas, n)
	}
	s.Sort()
	return s, nil
}

func (r *reader) readVersion() error {
	return r.each(`select current_setting('server_version_num')::int`, nil, func(rows pgx.Rows) error { return rows.Scan(&r.pg) })
}

func (r *reader) readSchemas() error {
	want := map[string]bool{}
	for _, n := range r.o.Schemas {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = true
		}
	}
	managed := map[string]bool{}
	for _, n := range ManagedSchemas {
		managed[n] = true
	}
	sharedNames := map[string]bool{}
	for _, n := range sharedTableSchemas {
		sharedNames[n] = true
	}
	r.sharedOn = len(want) == 0
	return r.each(`select n.oid::int8, n.nspname, coalesce(obj_description(n.oid, 'pg_namespace'), ''),
		exists (select 1 from pg_depend d where d.classid = 'pg_namespace'::regclass and d.objid = n.oid and d.deptype = 'e')
		from pg_namespace n where n.nspname !~ '^pg_' and n.nspname <> 'information_schema' order by n.nspname`, nil, func(rows pgx.Rows) error {
		var oid int64
		var name, comment string
		var ext bool
		if err := rows.Scan(&oid, &name, &comment, &ext); err != nil {
			return err
		}
		if r.sharedOn && sharedNames[name] {
			r.shared = append(r.shared, oid)
		}
		in := false
		if len(want) > 0 {
			in = want[name]
		} else {
			in = !managed[name] && !ext
		}
		if !in {
			return nil
		}
		r.scope = append(r.scope, oid)
		r.scopeNames[name] = true
		k := key(KindSchema, name)
		r.add(Object{Kind: KindSchema, Key: k, Name: QuoteIdent(name), Def: "CREATE SCHEMA IF NOT EXISTS " + QuoteIdent(name)})
		r.reg("pg_namespace", oid, 0, k)
		r.comment(k, "SCHEMA "+QuoteIdent(name), comment)
		return nil
	})
}

// comment adds a COMMENT object for target (a statement prefix like `TABLE "s"."t"`).
func (r *reader) comment(parent, target, text string) {
	if text == "" {
		return
	}
	r.add(Object{Kind: KindComment, Key: key(KindComment, parent), Parent: parent, Def: text, Attr: map[string]string{"target": target}})
}

func (r *reader) readExtensions() error {
	return r.each(`select e.oid::int8, e.extname, n.nspname from pg_extension e join pg_namespace n on n.oid = e.extnamespace
		where e.extname <> 'plpgsql' order by e.extname`, nil, func(rows pgx.Rows) error {
		var oid int64
		var name, schema string
		if err := rows.Scan(&oid, &name, &schema); err != nil {
			return err
		}
		k := key(KindExtension, name)
		o := Object{Kind: KindExtension, Key: k, Name: QuoteIdent(name),
			Def:  "CREATE EXTENSION IF NOT EXISTS " + QuoteIdent(name) + " WITH SCHEMA " + QuoteIdent(schema),
			Attr: map[string]string{"schema": schema}}
		r.add(o)
		r.reg("pg_extension", oid, 0, k)
		return nil
	})
}

func (r *reader) readTypes() error {
	type tinfo struct {
		oid, relid     int64
		schema, name   string
		kind           string
		comment, base  string
		notnull        bool
		def            string
		labels, fields []string
		checks         []string
	}
	var ts []*tinfo
	byOid := map[int64]*tinfo{}
	byRel := map[int64]*tinfo{}
	err := r.each(`select t.oid::int8, t.typrelid::int8, n.nspname, t.typname, t.typtype::text, coalesce(obj_description(t.oid, 'pg_type'), ''),
		case t.typtype when 'd' then format_type(t.typbasetype, t.typtypmod) else '' end, t.typnotnull, coalesce(t.typdefault, '')
		from pg_type t join pg_namespace n on n.oid = t.typnamespace
		where t.typnamespace = any($1::int8[]::oid[]) and t.typtype in ('e', 'd', 'c', 'r')
		  and (t.typtype <> 'c' or exists (select 1 from pg_class c where c.oid = t.typrelid and c.relkind = 'c'))
		  and `+notExt("pg_type", "t")+` order by n.nspname, t.typname`, []any{r.scope}, func(rows pgx.Rows) error {
		t := &tinfo{}
		var kind string
		if err := rows.Scan(&t.oid, &t.relid, &t.schema, &t.name, &kind, &t.comment, &t.base, &t.notnull, &t.def); err != nil {
			return err
		}
		switch kind {
		case "e":
			t.kind = KindEnum
		case "d":
			t.kind = KindDomain
		case "c":
			t.kind = KindComposite
		default:
			r.unmodeled = append(r.unmodeled, "range type "+t.schema+"."+t.name)
			return nil
		}
		ts = append(ts, t)
		byOid[t.oid] = t
		if t.relid != 0 {
			byRel[t.relid] = t
		}
		return nil
	})
	if err != nil || len(ts) == 0 {
		return err
	}
	oids := make([]int64, 0, len(ts))
	rels := []int64{}
	for _, t := range ts {
		oids = append(oids, t.oid)
		if t.relid != 0 {
			rels = append(rels, t.relid)
		}
	}
	if err := r.each(`select enumtypid::int8, enumlabel from pg_enum where enumtypid = any($1::int8[]::oid[]) order by enumtypid, enumsortorder`, []any{oids}, func(rows pgx.Rows) error {
		var id int64
		var l string
		if err := rows.Scan(&id, &l); err != nil {
			return err
		}
		byOid[id].labels = append(byOid[id].labels, l)
		return nil
	}); err != nil {
		return err
	}
	if len(rels) > 0 {
		if err := r.each(`select attrelid::int8, quote_ident(attname) || ' ' || format_type(atttypid, atttypmod) from pg_attribute
			where attrelid = any($1::int8[]::oid[]) and attnum > 0 and not attisdropped order by attrelid, attnum`, []any{rels}, func(rows pgx.Rows) error {
			var id int64
			var f string
			if err := rows.Scan(&id, &f); err != nil {
				return err
			}
			byRel[id].fields = append(byRel[id].fields, f)
			return nil
		}); err != nil {
			return err
		}
	}
	if err := r.each(`select contypid::int8, 'CONSTRAINT ' || quote_ident(conname) || ' ' || pg_get_constraintdef(oid) from pg_constraint
		where contypid = any($1::int8[]::oid[]) order by contypid, conname`, []any{oids}, func(rows pgx.Rows) error {
		var id int64
		var c string
		if err := rows.Scan(&id, &c); err != nil {
			return err
		}
		byOid[id].checks = append(byOid[id].checks, c)
		return nil
	}); err != nil {
		return err
	}
	for _, t := range ts {
		k := key(t.kind, t.schema, t.name)
		name := qname(t.schema, t.name)
		o := Object{Kind: t.kind, Key: k, Name: name, Parent: key(KindSchema, t.schema)}
		var target string
		switch t.kind {
		case KindEnum:
			o.List = t.labels
			lits := make([]string, len(t.labels))
			for i, l := range t.labels {
				lits[i] = QuoteLiteral(l)
			}
			o.Def = "CREATE TYPE " + name + " AS ENUM (" + strings.Join(lits, ", ") + ")"
			target = "TYPE " + name
		case KindComposite:
			o.List = t.fields
			o.Def = "CREATE TYPE " + name + " AS (" + strings.Join(t.fields, ", ") + ")"
			target = "TYPE " + name
		case KindDomain:
			def := "CREATE DOMAIN " + name + " AS " + t.base
			if t.def != "" {
				def += " DEFAULT " + t.def
			}
			if t.notnull {
				def += " NOT NULL"
			}
			for _, c := range t.checks {
				def += " " + c
			}
			o.Def = def
			target = "DOMAIN " + name
		}
		r.add(o)
		r.reg("pg_type", t.oid, 0, k)
		r.typeOids = append(r.typeOids, t.oid)
		if t.relid != 0 {
			r.reg("pg_class", t.relid, 0, k)
		}
		r.comment(k, target, t.comment)
	}
	return nil
}

func (r *reader) readTables() error {
	type part struct {
		key    string
		parent int64
	}
	var parts []part
	err := r.each(`select c.oid::int8, c.reltype::int8, n.nspname, c.relname, c.relkind::text, c.relrowsecurity, c.relforcerowsecurity, c.relpersistence::text,
		c.relispartition, coalesce(pg_get_partkeydef(c.oid), ''), case when c.relispartition then pg_get_expr(c.relpartbound, c.oid) else '' end,
		coalesce(c.reloptions, '{}'::text[]), coalesce(obj_description(c.oid, 'pg_class'), ''),
		coalesce((select i.inhparent::int8 from pg_inherits i where i.inhrelid = c.oid limit 1), 0)
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where c.relnamespace = any($1::int8[]::oid[]) and c.relkind in ('r', 'p') and `+notExt("pg_class", "c")+` order by n.nspname, c.relname`, []any{r.scope}, func(rows pgx.Rows) error {
		var oid, rtype, parent int64
		var schema, name, kind, persist, pkey, bound, comment string
		var rls, force, ispart bool
		var opts []string
		if err := rows.Scan(&oid, &rtype, &schema, &name, &kind, &rls, &force, &persist, &ispart, &pkey, &bound, &opts, &comment, &parent); err != nil {
			return err
		}
		sort.Strings(opts) // in the order they were set otherwise
		if parent != 0 && !ispart {
			r.unmodeled = append(r.unmodeled, "inherited table "+schema+"."+name)
			return nil
		}
		k := key(KindTable, schema, name)
		o := Object{Kind: KindTable, Key: k, Name: qname(schema, name), Parent: key(KindSchema, schema), Attr: map[string]string{
			"rls": strconv.FormatBool(rls), "force": strconv.FormatBool(force), "persist": persist, "partkey": pkey, "bound": bound,
		}, List: opts}
		r.add(o)
		r.reg("pg_class", oid, 0, k)
		r.tableOids = append(r.tableOids, oid)
		if rtype != 0 {
			r.reg("pg_type", rtype, 0, k)
			r.typeOids = append(r.typeOids, rtype)
		}
		if ispart {
			parts = append(parts, part{k, parent})
		}
		r.comment(k, "TABLE "+qname(schema, name), comment)
		return nil
	})
	if err != nil {
		return err
	}
	for _, p := range parts {
		pk, ok := r.oidKey[ref{"pg_class", p.parent, 0}]
		if !ok {
			// A partition of a table outside the schemas read cannot be created on its own.
			r.unmodeled = append(r.unmodeled, "partition "+strings.TrimPrefix(p.key, KindTable+"|"))
			i := r.byKey[p.key]
			r.objs[i].Kind = "" // dropped below
			continue
		}
		o := r.get(p.key)
		o.Attr["partof"] = pk
		o.Deps = append(o.Deps, pk)
	}
	kept := r.objs[:0]
	for _, o := range r.objs {
		if o.Kind != "" {
			kept = append(kept, o)
		}
	}
	r.objs = kept
	r.byKey = make(map[string]int, len(r.objs))
	for i, o := range r.objs {
		r.byKey[o.Key] = i
	}
	return nil
}

func (r *reader) readColumns() error {
	if len(r.tableOids) == 0 {
		return nil
	}
	tables := map[int64]string{}
	for k, v := range r.oidKey {
		if k.class == "pg_class" && k.sub == 0 && strings.HasPrefix(v, KindTable+"|") {
			tables[k.oid] = v
		}
	}
	return r.each(`select a.attrelid::int8, a.attnum::int, a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull, a.attidentity::text, a.attgenerated::text,
		coalesce(pg_get_expr(d.adbin, d.adrelid), ''), coalesce(d.oid::int8, 0),
		coalesce((select quote_ident(cn.nspname) || '.' || quote_ident(co.collname) from pg_collation co join pg_namespace cn on cn.oid = co.collnamespace
		          where co.oid = a.attcollation and a.attcollation <> t.typcollation), ''),
		coalesce(col_description(a.attrelid, a.attnum), ''), c.relispartition
		from pg_attribute a join pg_type t on t.oid = a.atttypid join pg_class c on c.oid = a.attrelid
		left join pg_attrdef d on d.adrelid = a.attrelid and d.adnum = a.attnum
		where a.attrelid = any($1::int8[]::oid[]) and a.attnum > 0 and not a.attisdropped order by a.attrelid, a.attnum`, []any{r.tableOids}, func(rows pgx.Rows) error {
		var rel, def int64
		var num int
		var name, typ, ident, gen, expr, coll, comment string
		var notnull, ispart bool
		if err := rows.Scan(&rel, &num, &name, &typ, &notnull, &ident, &gen, &expr, &def, &coll, &comment, &ispart); err != nil {
			return err
		}
		tk, ok := tables[rel]
		if !ok || ispart {
			return nil // a partition's columns are its parent's
		}
		t := r.get(tk)
		schema, table := splitKey(tk)
		ck := key(KindColumn, schema, table, name)
		col := Object{Kind: KindColumn, Key: ck, Name: QuoteIdent(name), Parent: tk, Attr: map[string]string{
			"type": typ, "notnull": strconv.FormatBool(notnull), "ident": ident, "coll": coll, "pos": strconv.Itoa(num),
		}}
		if gen != "" {
			col.Attr["gen"] = gen
			col.Attr["expr"] = expr
			if def != 0 {
				r.reg("pg_attrdef", def, 0, ck) // what the expression needs, the column needs
			}
		}
		r.add(col)
		r.reg("pg_class", rel, int32(num), ck)
		if expr != "" && gen == "" && ident == "" {
			dk := key(KindDefault, schema, table, name)
			r.add(Object{Kind: KindDefault, Key: dk, Name: QuoteIdent(name), Parent: ck, Def: expr, Attr: map[string]string{"table": t.Name}})
			r.reg("pg_attrdef", def, 0, dk)
		}
		r.comment(ck, "COLUMN "+t.Name+"."+QuoteIdent(name), comment)
		return nil
	})
}

// splitKey returns the schema and name parts of a "kind|schema|name" key.
func splitKey(k string) (schema, name string) {
	p := strings.SplitN(k, "|", 4)
	if len(p) >= 3 {
		return p[1], p[2]
	}
	return "", ""
}

func (r *reader) readSequences() error {
	type seq struct {
		key        string
		ownerTable int64
		ownerCol   int32
	}
	var seqs []seq
	err := r.each(`select c.oid::int8, c.reltype::int8, n.nspname, c.relname, format_type(s.seqtypid, null), s.seqstart, s.seqincrement, s.seqmin, s.seqmax, s.seqcache, s.seqcycle,
		coalesce(obj_description(c.oid, 'pg_class'), ''),
		coalesce((select d.refobjid::int8 from pg_depend d where d.classid = 'pg_class'::regclass and d.objid = c.oid and d.refclassid = 'pg_class'::regclass and d.deptype = 'a' limit 1), 0),
		coalesce((select d.refobjsubid from pg_depend d where d.classid = 'pg_class'::regclass and d.objid = c.oid and d.refclassid = 'pg_class'::regclass and d.deptype = 'a' limit 1), 0)
		from pg_class c join pg_namespace n on n.oid = c.relnamespace join pg_sequence s on s.seqrelid = c.oid
		where c.relnamespace = any($1::int8[]::oid[]) and c.relkind = 'S' and `+notExt("pg_class", "c")+`
		  and not exists (select 1 from pg_depend d where d.classid = 'pg_class'::regclass and d.objid = c.oid and d.deptype = 'i')
		order by n.nspname, c.relname`, []any{r.scope}, func(rows pgx.Rows) error {
		var oid, rtype, start, incr, min, max, cache, owner int64
		var ownerSub int32
		var schema, name, typ, comment string
		var cycle bool
		if err := rows.Scan(&oid, &rtype, &schema, &name, &typ, &start, &incr, &min, &max, &cache, &cycle, &comment, &owner, &ownerSub); err != nil {
			return err
		}
		k := key(KindSequence, schema, name)
		fq := qname(schema, name)
		o := Object{Kind: KindSequence, Key: k, Name: fq, Parent: key(KindSchema, schema), Attr: map[string]string{
			"type": typ, "start": strconv.FormatInt(start, 10), "incr": strconv.FormatInt(incr, 10), "min": strconv.FormatInt(min, 10),
			"max": strconv.FormatInt(max, 10), "cache": strconv.FormatInt(cache, 10), "cycle": strconv.FormatBool(cycle),
		}}
		r.add(o)
		r.reg("pg_class", oid, 0, k)
		if rtype != 0 {
			r.reg("pg_type", rtype, 0, k)
		}
		seqs = append(seqs, seq{k, owner, ownerSub})
		r.comment(k, "SEQUENCE "+fq, comment)
		return nil
	})
	if err != nil {
		return err
	}
	// The owning column is known once the columns are read (they come before this step).
	for _, s := range seqs {
		o := r.get(s.key)
		if s.ownerTable != 0 {
			if ck, ok := r.oidKey[ref{"pg_class", s.ownerTable, s.ownerCol}]; ok {
				if c := r.get(ck); c != nil {
					tk := c.Parent
					o.Attr["owner"] = r.get(tk).Name + "." + c.Name
					o.Deps = append(o.Deps, tk)
				}
			}
		}
		o.Def = sequenceDDL(o)
	}
	return nil
}

func sequenceDDL(o *Object) string {
	a := o.Attr
	cycle := "NO CYCLE"
	if a["cycle"] == "true" {
		cycle = "CYCLE"
	}
	s := fmt.Sprintf("CREATE SEQUENCE %s AS %s INCREMENT BY %s MINVALUE %s MAXVALUE %s START WITH %s CACHE %s %s",
		o.Name, a["type"], a["incr"], a["min"], a["max"], a["start"], a["cache"], cycle)
	if a["owner"] != "" {
		s += " OWNED BY " + a["owner"]
	}
	return s
}

func (r *reader) readConstraints() error {
	if len(r.tableOids) == 0 {
		return nil
	}
	return r.each(`select c.oid::int8, c.conrelid::int8, n.nspname, rel.relname, c.conname, c.contype::text, pg_get_constraintdef(c.oid, true), c.conindid::int8, c.confrelid::int8,
		c.condeferrable, c.condeferred
		from pg_constraint c join pg_class rel on rel.oid = c.conrelid join pg_namespace n on n.oid = rel.relnamespace
		where c.conrelid = any($1::int8[]::oid[]) and c.contype in ('p', 'u', 'f', 'c', 'x') and c.conparentid = 0
		order by n.nspname, rel.relname, c.conname`, []any{r.tableOids}, func(rows pgx.Rows) error {
		var oid, rel, idx, frel int64
		var schema, table, name, typ, def string
		var deferrable, deferred bool
		if err := rows.Scan(&oid, &rel, &schema, &table, &name, &typ, &def, &idx, &frel, &deferrable, &deferred); err != nil {
			return err
		}
		tk := key(KindTable, schema, table)
		if r.get(tk) == nil {
			return nil
		}
		k := key(KindConstraint, schema, table, name)
		o := Object{Kind: KindConstraint, Key: k, Name: QuoteIdent(name), Parent: tk, Def: def, Attr: map[string]string{"type": typ, "table": r.get(tk).Name}}
		r.add(o)
		r.reg("pg_constraint", oid, 0, k)
		if typ == "p" || typ == "u" || typ == "x" {
			r.reg("pg_class", idx, 0, k) // the index that backs it
		}
		return nil
	})
}

func (r *reader) readIndexes() error {
	rels := append(append([]int64{}, r.tableOids...), r.matviewOids()...)
	if len(rels) == 0 {
		return nil
	}
	return r.each(`select i.indexrelid::int8, n.nspname, ic.relname, t.relname, pg_get_indexdef(i.indexrelid)
		from pg_index i join pg_class ic on ic.oid = i.indexrelid join pg_class t on t.oid = i.indrelid join pg_namespace n on n.oid = t.relnamespace
		where i.indrelid = any($1::int8[]::oid[]) and not ic.relispartition
		  and not exists (select 1 from pg_constraint k where k.conindid = i.indexrelid and k.contype in ('p', 'u', 'x'))
		order by n.nspname, ic.relname`, []any{rels}, func(rows pgx.Rows) error {
		var oid int64
		var schema, name, table, def string
		if err := rows.Scan(&oid, &schema, &name, &table, &def); err != nil {
			return err
		}
		pk := key(KindTable, schema, table)
		if r.get(pk) == nil {
			pk = key(KindMatView, schema, table)
		}
		if r.get(pk) == nil {
			return nil
		}
		k := key(KindIndex, schema, name)
		r.add(Object{Kind: KindIndex, Key: k, Name: qname(schema, name), Parent: pk, Def: def})
		r.reg("pg_class", oid, 0, k)
		return nil
	})
}

func (r *reader) matviewOids() []int64 {
	var out []int64
	for k, v := range r.oidKey {
		if k.class == "pg_class" && k.sub == 0 && strings.HasPrefix(v, KindMatView+"|") {
			out = append(out, k.oid)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (r *reader) readViews() error {
	type v struct {
		key  string
		cols []string
	}
	vs := map[int64]*v{}
	var oids []int64
	err := r.each(`select c.oid::int8, c.reltype::int8, n.nspname, c.relname, c.relkind::text, pg_get_viewdef(c.oid, true), coalesce(c.reloptions, '{}'::text[]),
		coalesce(obj_description(c.oid, 'pg_class'), '')
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where c.relnamespace = any($1::int8[]::oid[]) and c.relkind in ('v', 'm') and `+notExt("pg_class", "c")+` order by n.nspname, c.relname`, []any{r.scope}, func(rows pgx.Rows) error {
		var oid, rtype int64
		var schema, name, kind, def, comment string
		var opts []string
		if err := rows.Scan(&oid, &rtype, &schema, &name, &kind, &def, &opts, &comment); err != nil {
			return err
		}
		sort.Strings(opts)
		k, target := key(KindView, schema, name), "VIEW "
		ok := KindView
		if kind == "m" {
			k, target, ok = key(KindMatView, schema, name), "MATERIALIZED VIEW ", KindMatView
		}
		fq := qname(schema, name)
		r.add(Object{Kind: ok, Key: k, Name: fq, Parent: key(KindSchema, schema), Def: strings.TrimRight(strings.TrimSpace(def), ";"), List: opts})
		r.reg("pg_class", oid, 0, k)
		if rtype != 0 {
			r.reg("pg_type", rtype, 0, k)
			r.typeOids = append(r.typeOids, rtype)
		}
		vs[oid] = &v{key: k}
		oids = append(oids, oid)
		r.viewOids = append(r.viewOids, oid)
		r.comment(k, target+fq, comment)
		return nil
	})
	if err != nil || len(oids) == 0 {
		return err
	}
	if err := r.each(`select attrelid::int8, quote_ident(attname) || ' ' || format_type(atttypid, atttypmod) from pg_attribute
		where attrelid = any($1::int8[]::oid[]) and attnum > 0 and not attisdropped order by attrelid, attnum`, []any{oids}, func(rows pgx.Rows) error {
		var id int64
		var c string
		if err := rows.Scan(&id, &c); err != nil {
			return err
		}
		vs[id].cols = append(vs[id].cols, c)
		return nil
	}); err != nil {
		return err
	}
	for _, x := range vs {
		r.get(x.key).Attr = map[string]string{"cols": strings.Join(x.cols, ", ")}
	}
	// The rules that stand for a view's query: its dependencies hang on them.
	return r.each(`select oid::int8, ev_class::int8 from pg_rewrite where ev_class = any($1::int8[]::oid[]) and rulename = '_RETURN'`, []any{oids}, func(rows pgx.Rows) error {
		var rule, rel int64
		if err := rows.Scan(&rule, &rel); err != nil {
			return err
		}
		r.reg("pg_rewrite", rule, 0, vs[rel].key)
		return nil
	})
}

func (r *reader) readFunctions() error {
	return r.each(`select p.oid::int8, n.nspname, p.proname, pg_get_function_identity_arguments(p.oid), p.prokind::text, pg_get_functiondef(p.oid),
		pg_get_function_result(p.oid), coalesce(obj_description(p.oid, 'pg_proc'), '')
		from pg_proc p join pg_namespace n on n.oid = p.pronamespace
		where p.pronamespace = any($1::int8[]::oid[]) and p.prokind in ('f', 'p') and `+notExt("pg_proc", "p")+` order by n.nspname, p.proname, 4`, []any{r.scope}, func(rows pgx.Rows) error {
		var oid int64
		var schema, name, args, kind, def, result, comment string
		if err := rows.Scan(&oid, &schema, &name, &args, &kind, &def, &result, &comment); err != nil {
			return err
		}
		k := key(KindFunction, schema, name+"("+args+")")
		word := "FUNCTION"
		if kind == "p" {
			word = "PROCEDURE"
		}
		fq := qname(schema, name) + "(" + args + ")"
		r.add(Object{Kind: KindFunction, Key: k, Name: fq, Parent: key(KindSchema, schema), Def: strings.TrimRight(strings.TrimSpace(def), ";"),
			Attr: map[string]string{"kind": word, "result": result}})
		r.reg("pg_proc", oid, 0, k)
		r.comment(k, word+" "+fq, comment)
		return nil
	})
}

func (r *reader) readTriggers() error {
	return r.each(`select t.oid::int8, n.nspname, c.relname, t.tgname, pg_get_triggerdef(t.oid, true), t.tgenabled::text, c.relkind::text, c.relnamespace = any($1::int8[]::oid[])
		from pg_trigger t join pg_class c on c.oid = t.tgrelid join pg_namespace n on n.oid = c.relnamespace
		join pg_proc p on p.oid = t.tgfoid join pg_namespace pn on pn.oid = p.pronamespace
		where not t.tgisinternal and t.tgparentid = 0 and c.relkind in ('r', 'p', 'v')
		  and (c.relnamespace = any($1::int8[]::oid[]) or (c.relnamespace = any($2::int8[]::oid[]) and pn.nspname <> all($3::text[])))
		order by n.nspname, c.relname, t.tgname`, []any{r.scope, r.shared, serviceFunctionSchemas}, func(rows pgx.Rows) error {
		var oid int64
		var schema, table, name, def, enabled, kind string
		var inScope bool
		if err := rows.Scan(&oid, &schema, &table, &name, &def, &enabled, &kind, &inScope); err != nil {
			return err
		}
		tk := key(KindTable, schema, table)
		if kind == "v" {
			tk = key(KindView, schema, table)
		}
		k := key(KindTrigger, schema, table, name)
		fq := qname(schema, table)
		r.add(Object{Kind: KindTrigger, Key: k, Name: QuoteIdent(name), Parent: tk, Def: strings.TrimSpace(def), Attr: map[string]string{"table": fq, "enabled": enabled}})
		r.reg("pg_trigger", oid, 0, k)
		return nil
	})
}

func (r *reader) readPolicies() error {
	return r.each(`select p.oid::int8, n.nspname, c.relname, p.polname, p.polcmd::text, p.polpermissive,
		coalesce((select array_agg(case when x = 0 then 'public' else (select quote_ident(rolname) from pg_roles where oid = x) end order by x) from unnest(p.polroles) x), '{}'::text[]),
		coalesce(pg_get_expr(p.polqual, p.polrelid), ''), coalesce(pg_get_expr(p.polwithcheck, p.polrelid), '')
		from pg_policy p join pg_class c on c.oid = p.polrelid join pg_namespace n on n.oid = c.relnamespace
		where c.relnamespace = any($1::int8[]::oid[]) or c.relnamespace = any($2::int8[]::oid[]) order by n.nspname, c.relname, p.polname`, []any{r.scope, r.shared}, func(rows pgx.Rows) error {
		var oid int64
		var schema, table, name, cmd, using, check string
		var permissive bool
		var roles []string
		if err := rows.Scan(&oid, &schema, &table, &name, &cmd, &permissive, &roles, &using, &check); err != nil {
			return err
		}
		sort.Strings(roles)
		k := key(KindPolicy, schema, table, name)
		o := Object{Kind: KindPolicy, Key: k, Name: QuoteIdent(name), Parent: key(KindTable, schema, table), List: roles, Attr: map[string]string{
			"table": qname(schema, table), "cmd": cmd, "permissive": strconv.FormatBool(permissive), "using": using, "check": check,
		}}
		o.Def = policyDDL(&o)
		r.add(o)
		r.reg("pg_policy", oid, 0, k)
		return nil
	})
}

var policyCmds = map[string]string{"r": "SELECT", "a": "INSERT", "w": "UPDATE", "d": "DELETE", "*": "ALL"}

func policyDDL(o *Object) string {
	a := o.Attr
	kind := "PERMISSIVE"
	if a["permissive"] != "true" {
		kind = "RESTRICTIVE"
	}
	s := fmt.Sprintf("CREATE POLICY %s ON %s AS %s FOR %s TO %s", o.Name, a["table"], kind, policyCmds[a["cmd"]], strings.Join(o.List, ", "))
	if a["using"] != "" {
		s += " USING (" + a["using"] + ")"
	}
	if a["check"] != "" {
		s += " WITH CHECK (" + a["check"] + ")"
	}
	return s
}

// ---- grants ----------------------------------------------------------------------------

type aclRow struct {
	obj, target, typ, grantee, priv string
	grantable                       bool
}

func (r *reader) readACLs() error {
	var rows []aclRow
	collect := func(sql, typ string, args ...any) error {
		return r.each(sql, args, func(x pgx.Rows) error {
			var kind, schema, name string
			var a aclRow
			if err := x.Scan(&kind, &schema, &name, &a.grantee, &a.priv, &a.grantable); err != nil {
				return err
			}
			a.typ = typ
			if typ == "TABLE" && kind == "S" {
				a.typ = "SEQUENCE"
			}
			switch typ {
			case "SCHEMA":
				a.obj, a.target = key(KindSchema, schema), QuoteIdent(schema)
			case "FUNCTION":
				a.obj, a.target = key(KindFunction, schema, name), qname(schema, strings.SplitN(name, "(", 2)[0])+"("+strings.TrimSuffix(strings.SplitN(name, "(", 2)[1], ")")+")"
			default:
				switch kind {
				case "v":
					a.obj = key(KindView, schema, name)
				case "m":
					a.obj = key(KindMatView, schema, name)
				case "S":
					a.obj = key(KindSequence, schema, name)
				default:
					a.obj = key(KindTable, schema, name)
				}
				a.target = qname(schema, name)
			}
			rows = append(rows, a)
			return nil
		})
	}
	if err := collect(`select c.relkind::text, n.nspname, c.relname, case a.grantee when 0 then 'public' else quote_ident(pg_get_userbyid(a.grantee)) end, a.privilege_type, a.is_grantable
		from pg_class c join pg_namespace n on n.oid = c.relnamespace, aclexplode(c.relacl) a
		where c.relnamespace = any($1::int8[]::oid[]) and c.relkind in ('r', 'p', 'v', 'm', 'S') and c.relacl is not null and a.grantee <> c.relowner
		order by 1, 2, 3, 4, 5`, "TABLE", r.scope); err != nil {
		return err
	}
	if err := collect(`select 'n', n.nspname, '', case a.grantee when 0 then 'public' else quote_ident(pg_get_userbyid(a.grantee)) end, a.privilege_type, a.is_grantable
		from pg_namespace n, aclexplode(n.nspacl) a where n.oid = any($1::int8[]::oid[]) and n.nspacl is not null and a.grantee <> n.nspowner
		order by 2, 4, 5`, "SCHEMA", r.scope); err != nil {
		return err
	}
	// A function's ACL is NULL while PUBLIC may execute it. Once it is set, a missing PUBLIC entry
	// means PUBLIC lost the privilege: that shows as an entry with no privileges.
	publicSeen := map[string]bool{}
	fnACL := map[string]bool{}
	if err := collect(`select p.prokind::text, n.nspname, p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')',
		case a.grantee when 0 then 'public' else quote_ident(pg_get_userbyid(a.grantee)) end, a.privilege_type, a.is_grantable
		from pg_proc p join pg_namespace n on n.oid = p.pronamespace, aclexplode(p.proacl) a
		where p.pronamespace = any($1::int8[]::oid[]) and p.prokind in ('f', 'p') and p.proacl is not null and a.grantee <> p.proowner
		order by 2, 3, 4, 5`, "FUNCTION", r.scope); err != nil {
		return err
	}
	// Functions whose ACL holds only the owner's entry (PUBLIC revoked, nobody else granted).
	var bare []aclRow
	if err := r.each(`select p.prokind::text, n.nspname, p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')'
		from pg_proc p join pg_namespace n on n.oid = p.pronamespace
		where p.pronamespace = any($1::int8[]::oid[]) and p.prokind in ('f', 'p') and p.proacl is not null`, []any{r.scope}, func(x pgx.Rows) error {
		var kind, schema, name string
		if err := x.Scan(&kind, &schema, &name); err != nil {
			return err
		}
		fnACL[key(KindFunction, schema, name)] = true
		return nil
	}); err != nil {
		return err
	}
	for _, a := range rows {
		if a.typ == "FUNCTION" && a.grantee == "public" {
			publicSeen[a.obj] = true
		}
	}
	for obj := range fnACL {
		if !publicSeen[obj] {
			parts := strings.SplitN(obj, "|", 3)
			name := parts[2]
			bare = append(bare, aclRow{obj: obj, typ: "FUNCTION", grantee: "public", priv: "",
				target: qname(parts[1], strings.SplitN(name, "(", 2)[0]) + "(" + strings.TrimSuffix(strings.SplitN(name, "(", 2)[1], ")") + ")"})
		}
	}
	rows = append(rows, bare...)

	type gk struct{ obj, grantee string }
	group := map[gk]*Object{}
	var order []gk
	for _, a := range rows {
		if r.get(a.obj) == nil {
			continue
		}
		g := gk{a.obj, a.grantee}
		o, ok := group[g]
		if !ok {
			o = &Object{Kind: KindACL, Key: key(KindACL, a.obj, a.grantee), Name: a.grantee, Parent: a.obj,
				Attr: map[string]string{"objtype": a.typ, "target": a.target}}
			group[g] = o
			order = append(order, g)
		}
		if a.priv != "" {
			p := a.priv
			if a.grantable {
				p += "*"
			}
			o.List = append(o.List, p)
		}
	}
	for _, g := range order {
		o := group[g]
		sort.Strings(o.List)
		o.Def = strings.Join(o.List, ",")
		r.add(*o)
	}
	return nil
}

func (r *reader) readDefaultACLs() error {
	type dk struct{ schema, typ, grantee string }
	group := map[dk]*Object{}
	var order []dk
	err := r.each(`select coalesce(n.nspname, ''), d.defaclobjtype::text,
		case a.grantee when 0 then 'public' else quote_ident(pg_get_userbyid(a.grantee)) end, a.privilege_type, a.is_grantable
		from pg_default_acl d left join pg_namespace n on n.oid = d.defaclnamespace, aclexplode(d.defaclacl) a
		where pg_get_userbyid(d.defaclrole) = 'postgres' and (d.defaclnamespace = 0 or d.defaclnamespace = any($1::int8[]::oid[]))
		order by 1, 2, 3, 4`, []any{r.scope}, func(x pgx.Rows) error {
		var schema, typ, grantee, priv string
		var grantable bool
		if err := x.Scan(&schema, &typ, &grantee, &priv, &grantable); err != nil {
			return err
		}
		g := dk{schema, typ, grantee}
		o, ok := group[g]
		if !ok {
			o = &Object{Kind: KindDefACL, Key: key(KindDefACL, "postgres", schema, typ, grantee), Name: grantee,
				Attr: map[string]string{"role": "postgres", "schema": schema, "objtype": typ}}
			if schema != "" {
				o.Parent = key(KindSchema, schema)
			}
			group[g] = o
			order = append(order, g)
		}
		if grantable {
			priv += "*"
		}
		o.List = append(o.List, priv)
		return nil
	})
	if err != nil {
		return err
	}
	for _, g := range order {
		o := group[g]
		sort.Strings(o.List)
		o.Def = strings.Join(o.List, ",")
		r.add(*o)
	}
	return nil
}

// ---- what the engine does not model -------------------------------------------------------

func (r *reader) readUnmodeled() error {
	q := func(kind, sql string, args ...any) error {
		return r.each(sql, args, func(x pgx.Rows) error {
			var n string
			if err := x.Scan(&n); err != nil {
				return err
			}
			r.unmodeled = append(r.unmodeled, kind+" "+n)
			return nil
		})
	}
	sc := []any{r.scope}
	queries := []struct {
		kind, sql string
		args      []any
	}{
		{"aggregate", `select n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')' from pg_proc p join pg_namespace n on n.oid = p.pronamespace
			where p.pronamespace = any($1::int8[]::oid[]) and p.prokind in ('a', 'w') and ` + notExt("pg_proc", "p"), sc},
		{"operator", `select n.nspname || '.' || o.oprname from pg_operator o join pg_namespace n on n.oid = o.oprnamespace
			where o.oprnamespace = any($1::int8[]::oid[]) and ` + notExt("pg_operator", "o"), sc},
		{"cast", `select format_type(castsource, null) || ' as ' || format_type(casttarget, null) from pg_cast c where c.oid >= 16384 and ` + notExt("pg_cast", "c"), nil},
		{"rule", `select n.nspname || '.' || c.relname || '.' || rw.rulename from pg_rewrite rw join pg_class c on c.oid = rw.ev_class join pg_namespace n on n.oid = c.relnamespace
			where c.relnamespace = any($1::int8[]::oid[]) and c.relkind in ('r', 'p') and rw.rulename <> '_RETURN'`, sc},
		{"event trigger", `select e.evtname from pg_event_trigger e where ` + notExt("pg_event_trigger", "e"), nil},
		{"publication", `select p.pubname from pg_publication p where ` + notExt("pg_publication", "p"), nil},
		{"foreign table", `select n.nspname || '.' || c.relname from pg_class c join pg_namespace n on n.oid = c.relnamespace where c.relnamespace = any($1::int8[]::oid[]) and c.relkind = 'f'`, sc},
		{"foreign server", `select s.srvname from pg_foreign_server s where ` + notExt("pg_foreign_server", "s"), nil},
		{"collation", `select n.nspname || '.' || c.collname from pg_collation c join pg_namespace n on n.oid = c.collnamespace where c.collnamespace = any($1::int8[]::oid[]) and ` + notExt("pg_collation", "c"), sc},
		{"statistics object", `select n.nspname || '.' || s.stxname from pg_statistic_ext s join pg_namespace n on n.oid = s.stxnamespace where s.stxnamespace = any($1::int8[]::oid[])`, sc},
		{"column privilege", `select n.nspname || '.' || c.relname || '.' || a.attname from pg_attribute a join pg_class c on c.oid = a.attrelid join pg_namespace n on n.oid = c.relnamespace
			where c.relnamespace = any($1::int8[]::oid[]) and c.relkind in ('r', 'p', 'v', 'm') and a.attacl is not null and not a.attisdropped`, sc},
	}
	for _, x := range queries {
		if err := q(x.kind, x.sql, x.args...); err != nil {
			return err
		}
	}
	return nil
}

// ---- dependencies ----------------------------------------------------------------------

func (r *reader) readDeps() error {
	// Array types of the types and relations read stand for their element.
	if len(r.typeOids) > 0 {
		var arrays [][2]int64
		if err := r.each(`select oid::int8, typelem::int8 from pg_type where typelem = any($1::int8[]::oid[]) and typcategory = 'A'`, []any{r.typeOids}, func(x pgx.Rows) error {
			var a, e int64
			if err := x.Scan(&a, &e); err != nil {
				return err
			}
			arrays = append(arrays, [2]int64{a, e})
			return nil
		}); err != nil {
			return err
		}
		for _, a := range arrays {
			if k, ok := r.oidKey[ref{"pg_type", a[1], 0}]; ok {
				r.oidKey[ref{"pg_type", a[0], 0}] = k
			}
		}
	}
	if len(r.oids) == 0 {
		return nil
	}
	type edge struct {
		from string
		to   ref
	}
	var edges []edge
	unresolved := map[int64]bool{}
	if err := r.each(`select d.classid::regclass::text, d.objid::int8, d.objsubid, d.refclassid::regclass::text, d.refobjid::int8, d.refobjsubid
		from pg_depend d
		where d.objid = any($1::int8[]::oid[]) and d.deptype in ('n', 'a', 'i')
		  and d.classid in ('pg_class'::regclass, 'pg_proc'::regclass, 'pg_type'::regclass, 'pg_attrdef'::regclass, 'pg_rewrite'::regclass, 'pg_constraint'::regclass, 'pg_trigger'::regclass, 'pg_policy'::regclass)
		  and d.refclassid in ('pg_class'::regclass, 'pg_proc'::regclass, 'pg_type'::regclass)`, []any{r.oids}, func(x pgx.Rows) error {
		var cls, rcls string
		var oid, roid int64
		var sub, rsub int32
		if err := x.Scan(&cls, &oid, &sub, &rcls, &roid, &rsub); err != nil {
			return err
		}
		from, ok := r.oidKey[ref{cls, oid, sub}]
		if !ok {
			from, ok = r.oidKey[ref{cls, oid, 0}]
		}
		if !ok {
			return nil
		}
		to := ref{rcls, roid, rsub}
		if _, ok := r.oidKey[to]; !ok {
			if _, ok := r.oidKey[ref{rcls, roid, 0}]; !ok {
				unresolved[roid] = true
			}
		}
		edges = append(edges, edge{from, to})
		return nil
	}); err != nil {
		return err
	}
	// Entries that belong to an extension (a type, a function) hang on the extension.
	extOf := map[int64]string{}
	if len(unresolved) > 0 {
		list := make([]int64, 0, len(unresolved))
		for o := range unresolved {
			list = append(list, o)
		}
		if err := r.each(`select d.objid::int8, e.extname from pg_depend d join pg_extension e on e.oid = d.refobjid
			where d.deptype = 'e' and d.refclassid = 'pg_extension'::regclass and d.objid = any($1::int8[]::oid[])`, []any{list}, func(x pgx.Rows) error {
			var oid int64
			var name string
			if err := x.Scan(&oid, &name); err != nil {
				return err
			}
			extOf[oid] = key(KindExtension, name)
			return nil
		}); err != nil {
			return err
		}
	}
	deps := map[string]map[string]bool{}
	for _, e := range edges {
		to, ok := r.oidKey[e.to]
		if !ok {
			to, ok = r.oidKey[ref{e.to.class, e.to.oid, 0}]
		}
		if !ok {
			to, ok = extOf[e.to.oid]
		}
		if !ok || to == e.from {
			continue
		}
		if r.get(to) == nil {
			continue
		}
		if deps[e.from] == nil {
			deps[e.from] = map[string]bool{}
		}
		deps[e.from][to] = true
	}
	for i := range r.objs {
		o := &r.objs[i]
		set := map[string]bool{}
		for _, d := range o.Deps {
			set[d] = true
		}
		for d := range deps[o.Key] {
			set[d] = true
		}
		if o.Parent != "" && r.get(o.Parent) != nil {
			set[o.Parent] = true
		}
		// An object of a schema this snapshot creates needs it first.
		if o.Kind != KindSchema && o.Kind != KindExtension {
			if s, _ := splitKey(o.Key); s != "" && o.Kind != KindACL && o.Kind != KindDefACL && o.Kind != KindComment {
				if sk := key(KindSchema, s); r.get(sk) != nil {
					set[sk] = true
				}
			}
		}
		delete(set, o.Key)
		o.Deps = o.Deps[:0]
		for d := range set {
			o.Deps = append(o.Deps, d)
		}
		sort.Strings(o.Deps)
	}
	// A table is created with its columns, so it needs what they need (their types), not the
	// defaults, which come after it.
	for i := range r.objs {
		o := &r.objs[i]
		if o.Kind != KindColumn {
			continue
		}
		t := r.get(o.Parent)
		if t == nil {
			continue
		}
		for _, d := range o.Deps {
			if d != t.Key && d != o.Parent && !strings.HasPrefix(d, KindColumn+"|") {
				t.Deps = appendUnique(t.Deps, d)
			}
		}
	}
	for i := range r.objs {
		sort.Strings(r.objs[i].Deps)
	}
	return nil
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
