package schemadiff

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Options tune Diff.
type Options struct {
	// RoleExists, when set, says whether a role exists on the database the statements will run on.
	// Grants and default privileges to a role that does not exist are left out, a policy loses the
	// roles that do not exist (and is left out when none is left), and each is named in Result.Notes.
	// The baseline of a branch uses it: custom roles are not copied to a new branch.
	RoleExists func(name string) bool
	// TableExists, when set, says whether a table or view that the snapshot being created does not hold
	// (a table of a managed schema such as storage.objects, where a user added a policy or a trigger)
	// exists on the target; key is the object key ("table|storage|objects"). A policy or trigger on one
	// that does not is left out and named in Result.Notes: Storage, for one, may not have set up a new
	// branch yet. The baseline of a branch uses it.
	TableExists func(key string) bool
	// KeepRemoved leaves out what the target has and the source lacks (no DROP, and no replacement
	// that needs one). Without a stored base a branch cannot tell what it removed from what its
	// parent added later, so the diff of such a branch carries additions and changes only.
	KeepRemoved bool
}

// Result is a plan.
type Result struct {
	Statements []string
	// Unsupported names changes the engine cannot express as DDL. The statements leave them out,
	// and SQL() puts a marker comment for each in front, so that a reader sees them and a merge
	// can refuse.
	Unsupported []string
	// Notes are things left out on purpose (a grant to a role the target lacks).
	Notes []string
}

// UnsupportedMarker begins the comment line SQL() writes for each unsupported change.
const UnsupportedMarker = "-- supavise:unsupported "

// Empty reports whether there is nothing to apply and nothing to say.
func (r *Result) Empty() bool { return len(r.Statements) == 0 && len(r.Unsupported) == 0 }

// SQL renders the plan as a script: the unsupported markers, then each statement terminated by a
// semicolon and separated by a blank line. It is empty for an empty plan.
func (r *Result) SQL() string {
	if r.Empty() {
		return ""
	}
	var b strings.Builder
	for _, u := range r.Unsupported {
		b.WriteString(UnsupportedMarker + strings.ReplaceAll(u, "\n", " ") + "\n")
	}
	for _, n := range r.Notes {
		b.WriteString("-- note: " + strings.ReplaceAll(n, "\n", " ") + "\n")
	}
	if len(r.Unsupported)+len(r.Notes) > 0 && len(r.Statements) > 0 {
		b.WriteString("\n")
	}
	for i, s := range r.Statements {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
		b.WriteString(";\n")
	}
	return b.String()
}

// HasUnsupported reports whether a script (the SQL of a Result) carries an unsupported marker.
func HasUnsupported(script string) bool { return strings.Contains(script, UnsupportedMarker) }

// step is the statements of one object in one pass.
type step struct {
	key   string
	kind  string
	deps  []string
	stmts []string
}

// rank orders steps that nothing else orders.
func rank(kind string) int {
	switch kind {
	case KindSchema:
		return 0
	case KindExtension:
		return 1
	case KindDefACL:
		return 2
	case KindEnum, KindComposite, KindDomain:
		return 3
	case KindSequence:
		return 4
	case KindTable:
		return 5
	case KindColumn:
		return 6
	case KindFunction:
		return 7
	case KindDefault:
		return 8
	case KindConstraint:
		return 9
	case KindIndex:
		return 10
	case KindView:
		return 11
	case KindMatView:
		return 12
	case KindTrigger:
		return 13
	case KindPolicy:
		return 14
	case KindACL:
		return 15
	case KindComment:
		return 16
	}
	return 17
}

type differ struct {
	from, to *Snapshot
	fi, ti   map[string]*Object
	opt      Options
	res      *Result

	removed map[string]bool // in from, not in to
	forced  map[string]bool // in both, but must be created again (the parent was replaced)
	replace map[string]bool // dropped and created again
	handled map[string]bool // created by another statement (a default folded into ADD COLUMN)

	drops   []*step
	creates []*step
}

// Diff plans the statements that take a database like from to one like to. A nil snapshot is an
// empty database.
func Diff(from, to *Snapshot, opt Options) *Result {
	if from == nil {
		from = &Snapshot{Version: FormatVersion}
	}
	if to == nil {
		to = &Snapshot{Version: FormatVersion}
	}
	d := &differ{from: from, to: to, fi: from.index(), ti: to.index(), opt: opt, res: &Result{},
		removed: map[string]bool{}, forced: map[string]bool{}, replace: map[string]bool{}, handled: map[string]bool{}}
	d.plan()
	return d.res
}

func (d *differ) plan() {
	// What is gone, what is new, what changed and how.
	for k := range d.fi {
		if _, ok := d.ti[k]; !ok {
			d.removed[k] = true
		}
	}
	d.classify()
	if d.opt.KeepRemoved {
		for k := range d.removed {
			delete(d.removed, k)
		}
		for k := range d.replace {
			d.res.Notes = append(d.res.Notes, fmt.Sprintf("%s was changed in a way that needs a drop and a create; it is left as it is", describe(d.fi[k])))
			delete(d.replace, k)
		}
	}
	d.dropPass()
	d.createPass()

	// The diff of what the engine does not model.
	fu, tu := setOf(d.from.Unmodeled), setOf(d.to.Unmodeled)
	for _, u := range d.to.Unmodeled {
		if !fu[u] {
			d.res.Unsupported = append(d.res.Unsupported, u+" exists only in the source; create it by hand")
		}
	}
	if !d.opt.KeepRemoved {
		for _, u := range d.from.Unmodeled {
			if !tu[u] {
				d.res.Unsupported = append(d.res.Unsupported, u+" exists only in the target; drop it by hand")
			}
		}
	}
	sort.Strings(d.res.Unsupported)

	var out []string
	for _, s := range d.drops {
		out = append(out, s.stmts...)
	}
	for _, s := range d.creates {
		out = append(out, s.stmts...)
	}
	for _, s := range out {
		if strings.Contains(s, "CREATE OR REPLACE FUNCTION") || strings.Contains(s, "CREATE OR REPLACE PROCEDURE") ||
			strings.HasPrefix(s, "CREATE FUNCTION") || strings.HasPrefix(s, "CREATE PROCEDURE") {
			// A SQL function is checked when it is created and may use a table that comes later.
			d.res.Statements = append(d.res.Statements, "SET check_function_bodies = off")
			break
		}
	}
	d.res.Statements = append(d.res.Statements, out...)
}

func setOf(s []string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, x := range s {
		m[x] = true
	}
	return m
}

func describe(o *Object) string {
	name := o.Name
	if o.Kind == KindColumn {
		name = d2(o)
	}
	return o.Kind + " " + name
}

func d2(o *Object) string {
	s, n := splitKey(o.Key)
	p := strings.Split(o.Key, "|")
	if len(p) >= 4 {
		return s + "." + n + "." + p[3]
	}
	return s + "." + n
}

// ---- classification ------------------------------------------------------------------------

// classify decides, for each object both snapshots have, whether it is unchanged, altered in
// place (handled when its step is built) or replaced, and propagates a replacement to the
// objects that belong to the replaced one.
func (d *differ) classify() {
	for k, f := range d.fi {
		t, ok := d.ti[k]
		if !ok {
			continue
		}
		if d.mustReplace(f, t) {
			d.replace[k] = true
		}
	}
	// A replaced object takes what hangs on it along: its indexes, triggers, grants, comments.
	changed := true
	for changed {
		changed = false
		for k, t := range d.ti {
			if _, in := d.fi[k]; !in || d.replace[k] || d.forced[k] {
				continue
			}
			if t.Parent != "" && (d.replace[t.Parent] || d.forced[t.Parent]) {
				d.forced[k] = true
				changed = true
			}
		}
	}
}

// mustReplace reports whether going from f to t needs the object dropped and created again.
func (d *differ) mustReplace(f, t *Object) bool {
	switch t.Kind {
	case KindComposite, KindDomain, KindConstraint, KindIndex, KindMatView:
		return f.Def != t.Def
	case KindTrigger:
		return f.Def != t.Def
	case KindPolicy:
		return f.Attr["cmd"] != t.Attr["cmd"] || f.Attr["permissive"] != t.Attr["permissive"]
	case KindFunction:
		return f.Def != t.Def && (f.Attr["result"] != t.Attr["result"] || f.Attr["kind"] != t.Attr["kind"])
	case KindView:
		if f.Def == t.Def && equalStrings(f.List, t.List) {
			return false
		}
		return !viewReplaceable(f, t)
	case KindTable:
		return f.Attr["partkey"] != t.Attr["partkey"] || f.Attr["partof"] != t.Attr["partof"] || f.Attr["bound"] != t.Attr["bound"]
	}
	return false
}

// viewReplaceable reports whether CREATE OR REPLACE VIEW can take f to t: the old columns stay,
// the same, in front, and no option is dropped.
func viewReplaceable(f, t *Object) bool {
	fc, tc := strings.Split(f.Attr["cols"], ", "), strings.Split(t.Attr["cols"], ", ")
	if f.Attr["cols"] == "" {
		return false
	}
	if len(tc) < len(fc) {
		return false
	}
	for i := range fc {
		if fc[i] != tc[i] {
			return false
		}
	}
	return equalStrings(f.List, t.List)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- the drop pass -------------------------------------------------------------------------

// goneWithAncestor reports whether the object disappears with an ancestor that is dropped or
// replaced, so that dropping it by itself is redundant (and, for a column of a dropped table, an
// error). A schema or an extension takes nothing with it: DROP SCHEMA without CASCADE refuses a
// schema that still holds objects.
func (d *differ) goneWithAncestor(o *Object) bool {
	for i, p := 0, o.Parent; p != "" && i < 8; i++ {
		po := d.fi[p]
		if po == nil || po.Kind == KindSchema || po.Kind == KindExtension {
			return false
		}
		if d.removed[p] || d.replace[p] {
			return true
		}
		p = po.Parent
	}
	return false
}

func (d *differ) dropPass() {
	steps := map[string]*step{}
	var keys []string
	add := func(k string, o *Object) {
		if sql := d.dropSQL(o); sql != "" {
			steps[k] = &step{key: k, kind: o.Kind, stmts: []string{sql}}
			keys = append(keys, k)
		}
	}
	for k := range d.removed {
		o := d.fi[k]
		if d.goneWithAncestor(o) || o.Kind == KindACL || o.Kind == KindComment {
			continue
		}
		add(k, o)
	}
	for k := range d.replace {
		add(k, d.fi[k])
	}
	// An object is dropped before what it depends on. What a dropped table takes with it (the
	// foreign keys it holds) still counts: it makes the table wait for the tables it references.
	stepOf := func(o *Object) string {
		for i := 0; o != nil && i < 8; i++ {
			if _, ok := steps[o.Key]; ok {
				return o.Key
			}
			o = d.fi[o.Parent]
		}
		return ""
	}
	for _, o := range d.fi {
		if !d.removed[o.Key] && !d.replace[o.Key] && !d.goneWithAncestor(o) {
			continue
		}
		mine := stepOf(o)
		if mine == "" {
			continue
		}
		for _, dep := range o.Deps {
			if theirs := stepOf(d.fi[dep]); theirs != "" && theirs != mine {
				steps[theirs].deps = append(steps[theirs].deps, mine)
			}
		}
	}
	d.drops = order(steps, true)
}

func (d *differ) dropSQL(o *Object) string {
	switch o.Kind {
	case KindSchema:
		if strings.HasSuffix(o.Key, "|public") {
			return ""
		}
		return "DROP SCHEMA " + o.Name
	case KindExtension:
		return "DROP EXTENSION " + o.Name
	case KindEnum, KindComposite:
		return "DROP TYPE " + o.Name
	case KindDomain:
		return "DROP DOMAIN " + o.Name
	case KindSequence:
		return "DROP SEQUENCE " + o.Name
	case KindTable:
		return "DROP TABLE " + o.Name
	case KindColumn:
		return "ALTER TABLE " + d.fi[o.Parent].Name + " DROP COLUMN " + o.Name
	case KindDefault:
		return "ALTER TABLE " + o.Attr["table"] + " ALTER COLUMN " + o.Name + " DROP DEFAULT"
	case KindConstraint:
		return "ALTER TABLE " + o.Attr["table"] + " DROP CONSTRAINT " + o.Name
	case KindIndex:
		return "DROP INDEX " + o.Name
	case KindTrigger:
		return "DROP TRIGGER " + o.Name + " ON " + o.Attr["table"]
	case KindPolicy:
		return "DROP POLICY " + o.Name + " ON " + o.Attr["table"]
	case KindView:
		return "DROP VIEW " + o.Name
	case KindMatView:
		return "DROP MATERIALIZED VIEW " + o.Name
	case KindFunction:
		return "DROP " + o.Attr["kind"] + " " + o.Name
	}
	return ""
}

// ---- the create pass -----------------------------------------------------------------------

func (d *differ) createPass() {
	steps := map[string]*step{}
	add := func(k string, kind string, deps []string, stmts ...string) {
		if len(stmts) == 0 {
			return
		}
		s := steps[k]
		if s == nil {
			s = &step{key: k, kind: kind}
			steps[k] = s
		}
		s.deps = append(s.deps, deps...)
		s.stmts = append(s.stmts, stmts...)
	}
	isNew := func(k string) bool { _, in := d.fi[k]; return !in || d.replace[k] || d.forced[k] }

	keys := make([]string, 0, len(d.ti))
	for k := range d.ti {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Defaults that ride along with an ADD COLUMN.
	for _, k := range keys {
		o := d.ti[k]
		if o.Kind != KindColumn || !isNew(k) {
			continue
		}
		tk := o.Parent
		if isNew(tk) {
			continue // the column is part of CREATE TABLE
		}
		dk := key(KindDefault, strings.Join(strings.Split(k, "|")[1:], "|"))
		if def := d.ti[dk]; def != nil && isNew(dk) {
			d.handled[dk] = true
		}
	}

	for _, k := range keys {
		o := d.ti[k]
		if d.handled[k] {
			continue
		}
		var f *Object
		if !d.replace[k] && !d.forced[k] {
			f = d.fi[k]
		}
		switch o.Kind {
		case KindACL, KindDefACL:
			continue // below
		case KindComment:
			if f == nil || f.Def != o.Def {
				add(k, o.Kind, o.Deps, "COMMENT ON "+o.Attr["target"]+" IS "+QuoteLiteral(o.Def))
			}
			continue
		}
		if f == nil {
			stmts := d.createSQL(o)
			if o.Kind == KindColumn && !isNew(o.Parent) {
				// ADD COLUMN, with the default that goes with it.
				dk := key(KindDefault, strings.Join(strings.Split(k, "|")[1:], "|"))
				deps := append([]string{}, o.Deps...)
				if def := d.ti[dk]; def != nil && d.handled[dk] {
					// the default rides along, so the column also needs what the default needs
					deps = append(deps, def.Deps...)
					if sql := d.addColumnWith(o, def); sql != "" {
						stmts = []string{sql}
					}
				}
				add(k, o.Kind, deps, stmts...)
				continue
			}
			if o.Kind == KindColumn && isNew(o.Parent) {
				continue // inside CREATE TABLE
			}
			add(k, o.Kind, o.Deps, stmts...)
			continue
		}
		if stmts := d.alterSQL(f, o); len(stmts) > 0 {
			add(k, o.Kind, o.Deps, stmts...)
		}
	}
	d.aclSteps(steps, isNew, add)
	// A table's rows of columns, defaults and checks need the table; the order below makes it so.
	d.creates = order(steps, false)
}

// addColumnWith renders ADD COLUMN for a column and its default in one statement.
func (d *differ) addColumnWith(col, def *Object) string {
	tbl := d.ti[col.Parent]
	if tbl == nil {
		return ""
	}
	return "ALTER TABLE " + tbl.Name + " ADD COLUMN " + columnClause(col, def.Def)
}

// columnClause is the column definition shared by CREATE TABLE and ADD COLUMN.
func columnClause(c *Object, dflt string) string {
	a := c.Attr
	s := c.Name + " " + a["type"]
	if a["coll"] != "" {
		s += " COLLATE " + a["coll"]
	}
	switch {
	case a["gen"] != "":
		s += " GENERATED ALWAYS AS (" + a["expr"] + ") STORED"
	case a["ident"] == "a":
		s += " GENERATED ALWAYS AS IDENTITY"
	case a["ident"] == "d":
		s += " GENERATED BY DEFAULT AS IDENTITY"
	case dflt != "":
		s += " DEFAULT " + dflt
	}
	if a["notnull"] == "true" {
		s += " NOT NULL"
	}
	return s
}

func (d *differ) createSQL(o *Object) []string {
	switch o.Kind {
	case KindTable:
		return d.createTable(o)
	case KindColumn:
		tbl := d.ti[o.Parent]
		if tbl == nil {
			return nil
		}
		return []string{"ALTER TABLE " + tbl.Name + " ADD COLUMN " + columnClause(o, "")}
	case KindDefault:
		return []string{"ALTER TABLE " + o.Attr["table"] + " ALTER COLUMN " + o.Name + " SET DEFAULT " + o.Def}
	case KindConstraint:
		return []string{"ALTER TABLE " + o.Attr["table"] + " ADD CONSTRAINT " + o.Name + " " + o.Def}
	case KindView:
		opts := ""
		if len(o.List) > 0 {
			opts = " WITH (" + strings.Join(o.List, ", ") + ")"
		}
		return []string{"CREATE VIEW " + o.Name + opts + " AS " + o.Def}
	case KindMatView:
		opts := ""
		if len(o.List) > 0 {
			opts = " WITH (" + strings.Join(o.List, ", ") + ")"
		}
		return []string{"CREATE MATERIALIZED VIEW " + o.Name + opts + " AS " + o.Def + " WITH DATA"}
	case KindPolicy:
		if d.missingTable(o) {
			return nil
		}
		roles := d.policyRoles(o)
		if roles == nil {
			return nil
		}
		c := *o
		c.List = roles
		return []string{policyDDL(&c)}
	case KindTrigger:
		if d.missingTable(o) {
			return nil
		}
		out := []string{o.Def}
		if o.Attr["enabled"] == "D" {
			out = append(out, "ALTER TABLE "+o.Attr["table"]+" DISABLE TRIGGER "+o.Name)
		}
		return out
	case KindSchema:
		if strings.HasSuffix(o.Key, "|public") {
			return nil // always there
		}
		return []string{o.Def}
	case KindExtension, KindEnum, KindComposite, KindDomain, KindSequence, KindIndex, KindFunction:
		return []string{o.Def}
	}
	return nil
}

// missingTable reports whether o hangs on a table the target lacks (and says so in the notes).
func (d *differ) missingTable(o *Object) bool {
	if d.opt.TableExists == nil || d.ti[o.Parent] != nil || d.opt.TableExists(o.Parent) {
		return false
	}
	d.res.Notes = append(d.res.Notes, fmt.Sprintf("%s %s on %s was left out: the table does not exist here", o.Kind, o.Name, o.Attr["table"]))
	return true
}

// policyRoles returns the roles a policy is created for, without those the target lacks. A policy
// that named roles and has none left is not created: it would otherwise widen to PUBLIC.
func (d *differ) policyRoles(o *Object) []string {
	if d.opt.RoleExists == nil {
		return o.List
	}
	var keep, lost []string
	for _, r := range o.List {
		if r == "public" || d.opt.RoleExists(unquote(r)) {
			keep = append(keep, r)
		} else {
			lost = append(lost, unquote(r))
		}
	}
	if len(lost) > 0 {
		if len(keep) == 0 {
			d.res.Notes = append(d.res.Notes, fmt.Sprintf("policy %s on %s was left out: its roles (%s) do not exist here", o.Name, o.Attr["table"], strings.Join(lost, ", ")))
			return nil
		}
		d.res.Notes = append(d.res.Notes, fmt.Sprintf("policy %s on %s lost the roles that do not exist here (%s)", o.Name, o.Attr["table"], strings.Join(lost, ", ")))
	}
	return keep
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}

func (d *differ) createTable(t *Object) []string {
	var cols []*Object
	for _, o := range d.to.Objects {
		if o.Kind == KindColumn && o.Parent == t.Key {
			o := o
			cols = append(cols, &o)
		}
	}
	sort.Slice(cols, func(i, j int) bool {
		a, _ := strconv.Atoi(cols[i].Attr["pos"])
		b, _ := strconv.Atoi(cols[j].Attr["pos"])
		return a < b
	})
	kw := "CREATE TABLE "
	if t.Attr["persist"] == "u" {
		kw = "CREATE UNLOGGED TABLE "
	}
	var s string
	if parent := t.Attr["partof"]; parent != "" {
		s = kw + t.Name + " PARTITION OF " + d.ti[parent].Name + " " + t.Attr["bound"]
	} else {
		defs := make([]string, len(cols))
		for i, c := range cols {
			defs[i] = "    " + columnClause(c, "")
		}
		s = kw + t.Name + " (\n" + strings.Join(defs, ",\n") + "\n)"
	}
	if t.Attr["partkey"] != "" {
		s += " PARTITION BY " + t.Attr["partkey"]
	}
	if len(t.List) > 0 {
		s += " WITH (" + strings.Join(t.List, ", ") + ")"
	}
	out := []string{s}
	if t.Attr["rls"] == "true" {
		out = append(out, "ALTER TABLE "+t.Name+" ENABLE ROW LEVEL SECURITY")
	}
	if t.Attr["force"] == "true" {
		out = append(out, "ALTER TABLE "+t.Name+" FORCE ROW LEVEL SECURITY")
	}
	return out
}

// alterSQL returns the statements that take an object from f to t in place.
func (d *differ) alterSQL(f, t *Object) []string {
	var out []string
	switch t.Kind {
	case KindExtension:
		if f.Attr["schema"] != t.Attr["schema"] {
			out = append(out, "ALTER EXTENSION "+t.Name+" SET SCHEMA "+QuoteIdent(t.Attr["schema"]))
		}
	case KindEnum:
		if !equalStrings(f.List, t.List) {
			stmts, ok := enumAdds(t, f.List, t.List)
			if !ok {
				d.res.Unsupported = append(d.res.Unsupported, "enum "+t.Name+": values were removed or reordered, which Postgres cannot do in place")
			}
			out = append(out, stmts...)
		}
	case KindSequence:
		out = append(out, sequenceAlters(f, t)...)
	case KindTable:
		out = append(out, tableAlters(f, t)...)
	case KindColumn:
		out = append(out, d.columnAlters(f, t)...)
	case KindDefault:
		if f.Def != t.Def {
			out = append(out, "ALTER TABLE "+t.Attr["table"]+" ALTER COLUMN "+t.Name+" SET DEFAULT "+t.Def)
		}
	case KindPolicy:
		if f.Def != t.Def {
			roles := d.policyRoles(t)
			if roles != nil {
				sql := "ALTER POLICY " + t.Name + " ON " + t.Attr["table"] + " TO " + strings.Join(roles, ", ")
				if t.Attr["using"] != "" {
					sql += " USING (" + t.Attr["using"] + ")"
				}
				if t.Attr["check"] != "" {
					sql += " WITH CHECK (" + t.Attr["check"] + ")"
				}
				out = append(out, sql)
			}
		}
	case KindTrigger:
		if f.Attr["enabled"] != t.Attr["enabled"] {
			out = append(out, "ALTER TABLE "+t.Attr["table"]+" "+enabledClause(t.Attr["enabled"])+" TRIGGER "+t.Name)
		}
	case KindView:
		if f.Def != t.Def || f.Attr["cols"] != t.Attr["cols"] {
			out = append(out, "CREATE OR REPLACE VIEW "+t.Name+" AS "+t.Def)
		}
	case KindFunction:
		if f.Def != t.Def {
			out = append(out, t.Def)
		}
	}
	return out
}

func enabledClause(s string) string {
	switch s {
	case "D":
		return "DISABLE"
	case "R":
		return "ENABLE REPLICA"
	case "A":
		return "ENABLE ALWAYS"
	}
	return "ENABLE"
}

// enumAdds lists ADD VALUE statements that take the labels from to to, when only additions are
// needed.
func enumAdds(t *Object, from, to []string) ([]string, bool) {
	have := map[string]bool{}
	for _, l := range from {
		have[l] = true
	}
	// from must be a subsequence of to
	j := 0
	for _, l := range to {
		if j < len(from) && l == from[j] {
			j++
		}
	}
	if j != len(from) {
		return nil, false
	}
	var out []string
	for i, l := range to {
		if have[l] {
			continue
		}
		clause := ""
		if i == 0 {
			if len(to) > 1 {
				clause = " BEFORE " + QuoteLiteral(to[1])
			}
		} else {
			clause = " AFTER " + QuoteLiteral(to[i-1])
		}
		out = append(out, "ALTER TYPE "+t.Name+" ADD VALUE IF NOT EXISTS "+QuoteLiteral(l)+clause)
		have[l] = true
	}
	return out, true
}

func sequenceAlters(f, t *Object) []string {
	var parts []string
	a, b := f.Attr, t.Attr
	if a["type"] != b["type"] {
		parts = append(parts, "AS "+b["type"])
	}
	if a["incr"] != b["incr"] {
		parts = append(parts, "INCREMENT BY "+b["incr"])
	}
	if a["min"] != b["min"] {
		parts = append(parts, "MINVALUE "+b["min"])
	}
	if a["max"] != b["max"] {
		parts = append(parts, "MAXVALUE "+b["max"])
	}
	if a["start"] != b["start"] {
		parts = append(parts, "START WITH "+b["start"])
	}
	if a["cache"] != b["cache"] {
		parts = append(parts, "CACHE "+b["cache"])
	}
	if a["cycle"] != b["cycle"] {
		if b["cycle"] == "true" {
			parts = append(parts, "CYCLE")
		} else {
			parts = append(parts, "NO CYCLE")
		}
	}
	var out []string
	if len(parts) > 0 {
		out = append(out, "ALTER SEQUENCE "+t.Name+" "+strings.Join(parts, " "))
	}
	if a["owner"] != b["owner"] {
		owner := b["owner"]
		if owner == "" {
			owner = "NONE"
		}
		out = append(out, "ALTER SEQUENCE "+t.Name+" OWNED BY "+owner)
	}
	return out
}

func tableAlters(f, t *Object) []string {
	var out []string
	if f.Attr["rls"] != t.Attr["rls"] {
		if t.Attr["rls"] == "true" {
			out = append(out, "ALTER TABLE "+t.Name+" ENABLE ROW LEVEL SECURITY")
		} else {
			out = append(out, "ALTER TABLE "+t.Name+" DISABLE ROW LEVEL SECURITY")
		}
	}
	if f.Attr["force"] != t.Attr["force"] {
		if t.Attr["force"] == "true" {
			out = append(out, "ALTER TABLE "+t.Name+" FORCE ROW LEVEL SECURITY")
		} else {
			out = append(out, "ALTER TABLE "+t.Name+" NO FORCE ROW LEVEL SECURITY")
		}
	}
	if f.Attr["persist"] != t.Attr["persist"] {
		if t.Attr["persist"] == "u" {
			out = append(out, "ALTER TABLE "+t.Name+" SET UNLOGGED")
		} else {
			out = append(out, "ALTER TABLE "+t.Name+" SET LOGGED")
		}
	}
	if !equalStrings(f.List, t.List) {
		have := map[string]bool{}
		for _, o := range t.List {
			have[strings.SplitN(o, "=", 2)[0]] = true
		}
		var reset []string
		for _, o := range f.List {
			if n := strings.SplitN(o, "=", 2)[0]; !have[n] {
				reset = append(reset, n)
			}
		}
		if len(t.List) > 0 {
			out = append(out, "ALTER TABLE "+t.Name+" SET ("+strings.Join(t.List, ", ")+")")
		}
		if len(reset) > 0 {
			out = append(out, "ALTER TABLE "+t.Name+" RESET ("+strings.Join(reset, ", ")+")")
		}
	}
	return out
}

func (d *differ) columnAlters(f, t *Object) []string {
	tbl := d.ti[t.Parent]
	if tbl == nil {
		return nil
	}
	pre := "ALTER TABLE " + tbl.Name + " ALTER COLUMN " + t.Name + " "
	a, b := f.Attr, t.Attr
	var out []string
	if a["type"] != b["type"] || a["coll"] != b["coll"] {
		s := pre + "TYPE " + b["type"]
		if b["coll"] != "" {
			s += " COLLATE " + b["coll"]
		}
		// A default that cannot be cast to the new type makes the change fail: drop it for the
		// change and set it again after.
		dk := key(KindDefault, strings.Join(strings.Split(t.Key, "|")[1:], "|"))
		def := d.ti[dk]
		if def != nil && d.fi[dk] != nil {
			out = append(out, pre+"DROP DEFAULT")
		}
		out = append(out, s+" USING "+t.Name+"::"+b["type"])
		if def != nil && d.fi[dk] != nil {
			out = append(out, pre+"SET DEFAULT "+def.Def)
		}
	}
	if a["gen"] != b["gen"] || a["expr"] != b["expr"] {
		switch {
		case a["gen"] != "" && b["gen"] == "":
			out = append(out, pre+"DROP EXPRESSION")
		case b["gen"] != "" && d.to.PG >= 170000:
			out = append(out, pre+"SET EXPRESSION AS ("+b["expr"]+")")
		default:
			d.res.Unsupported = append(d.res.Unsupported, "column "+d2(t)+": a generated expression cannot be changed in place here")
		}
	}
	if a["notnull"] != b["notnull"] {
		if b["notnull"] == "true" {
			out = append(out, pre+"SET NOT NULL")
		} else {
			out = append(out, pre+"DROP NOT NULL")
		}
	}
	if a["ident"] != b["ident"] {
		switch {
		case b["ident"] == "":
			out = append(out, pre+"DROP IDENTITY")
		case a["ident"] == "":
			out = append(out, pre+"ADD GENERATED "+identWord(b["ident"])+" AS IDENTITY")
		default:
			out = append(out, pre+"SET GENERATED "+identWord(b["ident"]))
		}
	}
	return out
}

func identWord(s string) string {
	if s == "a" {
		return "ALWAYS"
	}
	return "BY DEFAULT"
}

// ---- grants ---------------------------------------------------------------------------------

type aclSet = map[string][]string // grantee -> privileges (a trailing * marks grant option)

func (d *differ) aclSteps(steps map[string]*step, isNew func(string) bool, add func(string, string, []string, ...string)) {
	// Group the grants of both sides by object.
	group := func(s *Snapshot) map[string]aclSet {
		m := map[string]aclSet{}
		for i := range s.Objects {
			o := &s.Objects[i]
			if o.Kind != KindACL {
				continue
			}
			if m[o.Parent] == nil {
				m[o.Parent] = aclSet{}
			}
			m[o.Parent][o.Name] = o.List
		}
		return m
	}
	fromACL, toACL := group(d.from), group(d.to)
	objs := map[string]bool{}
	for k := range toACL {
		objs[k] = true
	}
	for k := range d.ti {
		o := d.ti[k]
		switch o.Kind {
		case KindTable, KindView, KindMatView, KindSequence, KindFunction, KindSchema:
			if isNew(k) {
				objs[k] = true
			}
		}
	}
	names := make([]string, 0, len(objs))
	for k := range objs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		o := d.ti[k]
		if o == nil {
			continue
		}
		objType, target := d.aclTarget(o)
		if objType == "" {
			continue
		}
		var stmts []string
		to := toACL[k]
		if isNew(k) {
			// The target gives a new object what its default privileges say (and PUBLIC may execute
			// a function), so statements are needed only where the object differs from that.
			def := d.defaultPrivsFor(o)
			for _, g := range sortedKeys(unionPrivs(def, to)) {
				dp, tp := def[g], to[g]
				if equalStrings(dp, tp) {
					continue
				}
				if len(dp) > 0 {
					stmts = append(stmts, d.revokeAll(objType, target, g)...)
				}
				stmts = append(stmts, d.grant(objType, target, g, tp)...)
			}
		} else {
			from := fromACL[k]
			if o.Kind == KindFunction {
				from = withImplicitPublic(from)
				to = withImplicitPublic(to)
			}
			for _, g := range sortedKeys(unionKeys(from, to)) {
				fp, tp := from[g], to[g]
				_, inTo := to[g]
				switch {
				case equalStrings(fp, tp) && (inTo || len(fp) == 0):
				case !inTo || len(tp) == 0:
					stmts = append(stmts, d.revokeAll(objType, target, g)...)
				default:
					stmts = append(stmts, d.revokeAll(objType, target, g)...)
					stmts = append(stmts, d.grant(objType, target, g, tp)...)
				}
			}
		}
		if len(stmts) > 0 {
			add("acl:"+k, KindACL, []string{k}, stmts...)
		}
	}
	// Default privileges themselves.
	fd, td := d.defaultACLs(d.from), d.defaultACLs(d.to)
	for _, k := range sortedKeys(unionKeys2(fd, td)) {
		f, t := fd[k], td[k]
		if equalStrings(listOf(f), listOf(t)) {
			continue
		}
		var o *Object
		if t != nil {
			o = t
		} else {
			o = f
		}
		if s := d.defaultACLStatements(o, listOf(f), listOf(t)); len(s) > 0 {
			var deps []string
			if o.Parent != "" {
				deps = []string{o.Parent}
			}
			add(k, KindDefACL, deps, s...)
		}
	}
}

func listOf(o *Object) []string {
	if o == nil {
		return nil
	}
	return o.List
}

func (d *differ) defaultACLs(s *Snapshot) map[string]*Object {
	m := map[string]*Object{}
	for i := range s.Objects {
		if s.Objects[i].Kind == KindDefACL {
			m[s.Objects[i].Key] = &s.Objects[i]
		}
	}
	return m
}

var defaultTypes = map[string]string{"r": "TABLES", "S": "SEQUENCES", "f": "FUNCTIONS", "T": "TYPES", "n": "SCHEMAS"}

func (d *differ) defaultACLStatements(o *Object, from, to []string) []string {
	g := unquote(o.Name)
	if g == "postgres" {
		return nil
	}
	if d.opt.RoleExists != nil && g != "public" && !d.opt.RoleExists(g) {
		d.res.Notes = append(d.res.Notes, fmt.Sprintf("default privileges for role %s were left out: the role does not exist here", g))
		return nil
	}
	pre := "ALTER DEFAULT PRIVILEGES FOR ROLE " + QuoteIdent(o.Attr["role"])
	if s := o.Attr["schema"]; s != "" {
		pre += " IN SCHEMA " + QuoteIdent(s)
	}
	typ := defaultTypes[o.Attr["objtype"]]
	to2 := granteeName(o.Name)
	var out []string
	if len(from) > 0 {
		out = append(out, pre+" REVOKE ALL ON "+typ+" FROM "+to2)
	}
	out = append(out, grantLists(pre+" GRANT %s ON "+typ+" TO "+to2, to)...)
	return out
}

func granteeName(g string) string {
	if g == "public" {
		return "PUBLIC"
	}
	return g
}

// grantLists renders a GRANT per option class (the privileges that carry the grant option and
// those that do not). format has one %s for the privilege list; WITH GRANT OPTION is appended.
func grantLists(format string, privs []string) []string {
	var plain, grantable []string
	for _, p := range privs {
		if strings.HasSuffix(p, "*") {
			grantable = append(grantable, strings.TrimSuffix(p, "*"))
		} else {
			plain = append(plain, p)
		}
	}
	var out []string
	if len(plain) > 0 {
		out = append(out, fmt.Sprintf(format, strings.Join(plain, ", ")))
	}
	if len(grantable) > 0 {
		out = append(out, fmt.Sprintf(format, strings.Join(grantable, ", "))+" WITH GRANT OPTION")
	}
	return out
}

func (d *differ) aclTarget(o *Object) (objType, target string) {
	switch o.Kind {
	case KindTable, KindView, KindMatView:
		return "TABLE", o.Name
	case KindSequence:
		return "SEQUENCE", o.Name
	case KindFunction:
		return o.Attr["kind"], o.Name
	case KindSchema:
		return "SCHEMA", o.Name
	}
	return "", ""
}

var defaultTypeOf = map[string]string{
	KindTable: "r", KindView: "r", KindMatView: "r", KindSequence: "S", KindFunction: "f", KindSchema: "n",
}

// defaultPrivsFor returns the privileges, by grantee, that the target's default privileges (those of
// the snapshot being applied: the create pass applies them first) give a new object of o's kind, and
// PUBLIC's EXECUTE on a function unless a default revoked it.
func (d *differ) defaultPrivsFor(o *Object) map[string][]string {
	typ := defaultTypeOf[o.Kind]
	schema, _ := splitKey(o.Key)
	set := map[string]map[string]bool{}
	globalF, globalPublic := false, false
	for i := range d.to.Objects {
		x := &d.to.Objects[i]
		if x.Kind != KindDefACL || x.Attr["objtype"] != typ {
			continue
		}
		if s := x.Attr["schema"]; s != "" && s != schema {
			continue
		}
		if x.Attr["schema"] == "" && typ == "f" {
			globalF = true
			if x.Name == "public" {
				globalPublic = true
			}
		}
		if unquote(x.Name) == "postgres" {
			continue
		}
		if set[x.Name] == nil {
			set[x.Name] = map[string]bool{}
		}
		for _, p := range x.List {
			set[x.Name][p] = true
		}
	}
	if typ == "f" && (!globalF || globalPublic) {
		if set["public"] == nil {
			set["public"] = map[string]bool{}
		}
		set["public"]["EXECUTE"] = true
	}
	out := map[string][]string{}
	for g, ps := range set {
		out[g] = sortedKeys(ps)
	}
	return out
}

func unionPrivs(a, b map[string][]string) map[string]struct{} {
	m := map[string]struct{}{}
	for k := range a {
		m[k] = struct{}{}
	}
	for k := range b {
		m[k] = struct{}{}
	}
	return m
}

func (d *differ) revokeAll(objType, target, grantee string) []string {
	if d.skipGrantee(grantee) {
		return nil
	}
	return []string{fmt.Sprintf("REVOKE ALL ON %s %s FROM %s", objType, target, granteeName(grantee))}
}

func (d *differ) grant(objType, target, grantee string, privs []string) []string {
	if len(privs) == 0 || d.skipGrantee(grantee) {
		return nil
	}
	return grantLists("GRANT %s ON "+objType+" "+target+" TO "+granteeName(grantee), privs)
}

// skipGrantee reports whether grants to the role are left out because the target lacks it.
func (d *differ) skipGrantee(g string) bool {
	if g == "public" || d.opt.RoleExists == nil {
		return false
	}
	name := unquote(g)
	if d.opt.RoleExists(name) {
		return false
	}
	note := fmt.Sprintf("grants to role %s were left out: the role does not exist here (custom roles are not copied)", name)
	for _, n := range d.res.Notes {
		if n == note {
			return true
		}
	}
	d.res.Notes = append(d.res.Notes, note)
	return true
}

func withImplicitPublic(s aclSet) aclSet {
	if _, ok := s["public"]; ok {
		return s
	}
	c := aclSet{}
	for k, v := range s {
		c[k] = v
	}
	if len(s) == 0 {
		// no ACL at all: PUBLIC executes
		c["public"] = []string{"EXECUTE"}
	}
	return c
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func unionKeys(a, b aclSet) map[string]struct{} {
	m := map[string]struct{}{}
	for k := range a {
		m[k] = struct{}{}
	}
	for k := range b {
		m[k] = struct{}{}
	}
	return m
}

func unionKeys2(a, b map[string]*Object) map[string]struct{} {
	m := map[string]struct{}{}
	for k := range a {
		m[k] = struct{}{}
	}
	for k := range b {
		m[k] = struct{}{}
	}
	return m
}

// ---- ordering ------------------------------------------------------------------------------

// order sorts steps so that each comes after the steps it depends on (reverse for a drop pass,
// where deps lists the steps that must run first), breaking ties by kind and key. A cycle is
// broken at the lowest-ranked step.
func order(steps map[string]*step, drop bool) []*step {
	left := map[string]*step{}
	for k, s := range steps {
		left[k] = s
	}
	indeg := map[string]int{}
	users := map[string][]string{}
	for k, s := range steps {
		seen := map[string]bool{}
		for _, dep := range s.deps {
			if dep == k || seen[dep] {
				continue
			}
			seen[dep] = true
			if _, ok := steps[dep]; !ok {
				continue
			}
			indeg[k]++
			users[dep] = append(users[dep], k)
		}
	}
	less := func(a, b *step) bool {
		ra, rb := rank(a.kind), rank(b.kind)
		if drop {
			ra, rb = -ra, -rb
		}
		if ra != rb {
			return ra < rb
		}
		return a.key < b.key
	}
	var out []*step
	for len(left) > 0 {
		var ready []*step
		for k, s := range left {
			if indeg[k] == 0 {
				ready = append(ready, s)
			}
		}
		if len(ready) == 0 {
			// a cycle: take the lowest-ranked step
			for _, s := range left {
				ready = append(ready, s)
			}
		}
		sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
		s := ready[0]
		out = append(out, s)
		delete(left, s.key)
		for _, u := range users[s.key] {
			indeg[u]--
		}
	}
	return out
}
