// Package schemadiff reads the schema of a Postgres database into a Snapshot and plans the DDL that
// takes one snapshot to another. It is the diff engine of branching (internal/branching): the
// baseline of a parent that has no migration history, the diff the merge page shows, the
// three-way base a branch is compared with, and the changes a push carries into a gitless branch.
//
// It reads the catalog itself instead of calling pg_dump or a Node tool, so a node needs nothing
// beyond its own Postgres, a snapshot is a small value the registry can store, and what the engine
// does not model is counted and named instead of silently dropped (Snapshot.Unmodeled).
//
// The shape follows Migra and pg-delta, the engines hosted Supabase uses: every object is a node
// with an identity, a canonical definition and the objects it needs; a plan drops what is gone in
// reverse dependency order, then creates and alters what is new in dependency order. Renames are
// not detected (a rename is a drop and a create, as in Migra).
package schemadiff

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// FormatVersion is the version of the snapshot format and of the object model. A snapshot of
// another version is not compared (the caller takes a new one).
const FormatVersion = 1

// Kinds of Object.
const (
	KindSchema     = "schema"
	KindExtension  = "extension"
	KindEnum       = "enum"
	KindComposite  = "composite"
	KindDomain     = "domain"
	KindSequence   = "sequence"
	KindTable      = "table"
	KindColumn     = "column"
	KindDefault    = "default"
	KindConstraint = "constraint"
	KindIndex      = "index"
	KindTrigger    = "trigger"
	KindPolicy     = "policy"
	KindView       = "view"
	KindMatView    = "matview"
	KindFunction   = "function"
	KindACL        = "acl"
	KindDefACL     = "defacl"
	KindComment    = "comment"
)

// Object is one node of a schema: a table, a column, a policy and so on.
type Object struct {
	Kind string `json:"k"`
	// Key identifies the object inside a snapshot (kind, schema, name and sub-name).
	Key string `json:"id"`
	// Name is the qualified, quoted name the statements use ("public"."todos").
	Name string `json:"n,omitempty"`
	// Parent is the key of the object this one belongs to (the table of a column, a policy, ...).
	Parent string `json:"p,omitempty"`
	// Def is the canonical definition. For most kinds it is the statement that creates the object.
	Def string `json:"d,omitempty"`
	// Attr carries what an in-place change needs (the type of a column, the options of a sequence).
	Attr map[string]string `json:"a,omitempty"`
	// List carries ordered values (enum labels, policy roles, grants).
	List []string `json:"l,omitempty"`
	// Deps are the keys of the objects that must exist before this one.
	Deps []string `json:"r,omitempty"`
}

// Snapshot is the schema of one database, as far as the engine models it.
type Snapshot struct {
	Version int `json:"v"`
	// PG is server_version_num of the database it was read from.
	PG int `json:"pg"`
	// Schemas are the schemas it covers (sorted).
	Schemas []string `json:"schemas"`
	Objects []Object `json:"objects"`
	// Unmodeled names the objects of kinds the engine does not model (aggregates, operators,
	// casts, rules, event triggers, publications, foreign tables, range types, collations,
	// extended statistics), "kind name" each, sorted. A difference here cannot be turned into DDL.
	Unmodeled []string `json:"unmodeled,omitempty"`
}

// Sort puts the objects in key order, which is the form Fingerprint and the encoding use.
func (s *Snapshot) Sort() {
	sort.Slice(s.Objects, func(i, j int) bool { return s.Objects[i].Key < s.Objects[j].Key })
	sort.Strings(s.Schemas)
	sort.Strings(s.Unmodeled)
}

// Marshal encodes the snapshot (sorted) as JSON.
func (s *Snapshot) Marshal() ([]byte, error) {
	s.Sort()
	return json.Marshal(s)
}

// Unmarshal decodes a snapshot written by Marshal. A snapshot of another FormatVersion is an error.
func Unmarshal(b []byte) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Version != FormatVersion {
		return nil, &VersionError{Have: s.Version}
	}
	return &s, nil
}

// VersionError is returned for a snapshot of a format this build does not read.
type VersionError struct{ Have int }

func (e *VersionError) Error() string {
	return "schemadiff: snapshot format " + strconv.Itoa(e.Have) + " is not " + strconv.Itoa(FormatVersion)
}

// Fingerprint is a hash of everything the snapshot holds, equal for equal schemas.
func (s *Snapshot) Fingerprint() string {
	s.Sort()
	h := sha256.New()
	w := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
		h.Write([]byte{1})
	}
	for _, o := range s.Objects {
		w(o.Key, o.Parent, o.Def)
		keys := make([]string, 0, len(o.Attr))
		for k := range o.Attr {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			w(k, o.Attr[k])
		}
		w(o.List...)
	}
	w(s.Unmodeled...)
	return hex.EncodeToString(h.Sum(nil))
}

// Empty reports whether the snapshot holds no object.
func (s *Snapshot) Empty() bool { return s == nil || (len(s.Objects) == 0 && len(s.Unmodeled) == 0) }

// index maps keys to objects.
func (s *Snapshot) index() map[string]*Object {
	m := make(map[string]*Object, len(s.Objects))
	for i := range s.Objects {
		m[s.Objects[i].Key] = &s.Objects[i]
	}
	return m
}

// Count returns the number of objects of a kind.
func (s *Snapshot) Count(kind string) int {
	n := 0
	for i := range s.Objects {
		if s.Objects[i].Kind == kind {
			n++
		}
	}
	return n
}

// ---- identifiers -----------------------------------------------------------------------

// QuoteIdent quotes a Postgres identifier.
func QuoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// QuoteLiteral quotes a string literal.
func QuoteLiteral(s string) string {
	if strings.Contains(s, `\`) {
		return "E'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func qname(schema, name string) string { return QuoteIdent(schema) + "." + QuoteIdent(name) }

func key(parts ...string) string { return strings.Join(parts, "|") }
