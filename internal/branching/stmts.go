package branching

import (
	"strings"

	"github.com/supavise/supavise/internal/branching/schemadiff"
)

// This file tells, for the statements of a migration, which ones a recorded schema diff holds and which
// ones it does not. Studio's merge page records the branch's schema diff as one migration, and the
// branch's other migrations are kept in the history as covered by it instead of being run again. The
// diff holds the DDL of the objects the schema engine models (tables, columns, constraints, indexes,
// views, functions, triggers, policies, types, sequences, extensions, schemas, grants, default
// privileges and comments). It does not hold what a migration does besides that: INSERT, UPDATE and
// privileges and comments). It does not hold what a migration does besides that: INSERT, UPDATE and
// DELETE of data (a bucket, a lookup table), cron.schedule, ALTER PUBLICATION, roles, the objects the
// engine does not model, and DDL on the objects of the services' own schemas (a column added to
// auth.users, a table in storage), of which the engine reads only the policies and triggers users add.
// Those statements are carried: they run after the diff, in the same transaction.

// sqlStmt is one statement of a script.
type sqlStmt struct {
	text string
	// words are the keywords and names outside parentheses, upper-cased; a quoted name, a string or a
	// dollar-quoted body is "?".
	words []string
}

type stmtClass int

const (
	// classHeld is DDL of an object kind the engine models: a diff that was made after it ran holds it.
	classHeld stmtClass = iota
	// classSession is SET or RESET: it changes how the statements after it run, not the database.
	classSession
	// classTx is transaction control, which has no place inside the transaction a merge runs in.
	classTx
	// classCarried is everything else.
	classCarried
)

// carriedStatements returns the statements of a migration that a recorded schema diff does not hold, in
// order, with the session settings that precede the last of them. It is nil when the diff holds the
// whole migration. A script that cannot be read (an unterminated quote) is carried as it is.
func carriedStatements(elements []string) []string {
	var all []sqlStmt
	for _, e := range elements {
		sts, ok := splitSQL(e)
		if !ok {
			if strings.TrimSpace(e) != "" {
				all = append(all, sqlStmt{text: e, words: []string{"?"}})
			}
			continue
		}
		all = append(all, sts...)
	}
	last := -1
	for i, st := range all {
		if classify(st) == classCarried {
			last = i
		}
	}
	if last < 0 {
		return nil
	}
	var out []string
	for i := 0; i <= last; i++ {
		switch classify(all[i]) {
		case classCarried, classSession:
			out = append(out, all[i].text)
		}
	}
	return out
}

// carried returns the statements of the migration that a diff covering it does not hold.
func (m Migration) carried() []string { return carriedStatements(m.Statements) }

// heldKinds are the object kinds whose DDL a schema diff holds.
var heldKinds = map[string]bool{
	"SCHEMA": true, "EXTENSION": true, "TABLE": true, "INDEX": true, "SEQUENCE": true, "VIEW": true, "MATERIALIZED": true,
	"FUNCTION": true, "PROCEDURE": true, "TRIGGER": true, "POLICY": true, "TYPE": true, "DOMAIN": true,
}

// createSkipped are the words that may stand between CREATE and the kind of the object.
var createSkipped = map[string]bool{
	"OR": true, "REPLACE": true, "UNIQUE": true, "UNLOGGED": true, "GLOBAL": true, "LOCAL": true, "RECURSIVE": true, "CONSTRAINT": true,
}

// managedSchemas are the schemas of the services, in lower case.
var managedSchemas = func() map[string]bool {
	m := make(map[string]bool, len(schemadiff.ManagedSchemas))
	for _, n := range schemadiff.ManagedSchemas {
		m[n] = true
	}
	return m
}()

// fillers may stand between the kind of an object and its name.
var fillers = map[string]bool{"IF": true, "NOT": true, "EXISTS": true, "ONLY": true, "CONCURRENTLY": true, "VIEW": true}

// inManaged reports whether the (possibly schema-qualified) name that starts at w[i], after the words
// that fill, is in a schema of the services. A quoted name cannot be told and is not.
func inManaged(w []string, i int) bool {
	for i < len(w) && fillers[w[i]] {
		i++
	}
	if i+1 < len(w) && w[i+1] == "." {
		return managedSchemas[strings.ToLower(w[i])]
	}
	return false
}

// classify says what a diff does with a statement.
func classify(st sqlStmt) stmtClass {
	w := st.words
	if len(w) == 0 {
		return classCarried
	}
	switch w[0] {
	case "SET", "RESET":
		return classSession
	case "BEGIN", "START", "COMMIT", "END", "ROLLBACK", "ABORT", "SAVEPOINT", "RELEASE":
		return classTx
	case "CREATE":
		i := 1
		for i < len(w) && createSkipped[w[i]] {
			i++
		}
		if i >= len(w) || !heldKinds[w[i]] {
			return classCarried // a temporary table is no object of the schema, either
		}
		return objectDDL(w, i)
	case "ALTER", "DROP":
		if len(w) > 1 && heldKinds[w[1]] {
			return objectDDL(w, 1)
		}
		if w[0] == "ALTER" && len(w) > 2 && w[1] == "DEFAULT" && w[2] == "PRIVILEGES" {
			return classHeld
		}
	case "COMMENT":
		if len(w) > 2 && w[1] == "ON" && (heldKinds[w[2]] || w[2] == "COLUMN" || w[2] == "CONSTRAINT") {
			if w[2] == "COLUMN" || w[2] == "CONSTRAINT" {
				if inManaged(w, 3) {
					return classCarried
				}
				return classHeld
			}
			return objectDDL(w, 2)
		}
	case "GRANT", "REVOKE":
		for i, x := range w {
			if x != "ON" {
				continue
			}
			j := i + 1
			if j < len(w) && w[j] == "ALL" {
				// ON ALL TABLES|SEQUENCES|FUNCTIONS IN SCHEMA name
				for k := j; k+1 < len(w); k++ {
					if w[k] == "SCHEMA" {
						if managedSchemas[strings.ToLower(w[k+1])] {
							return classCarried
						}
						break
					}
				}
				return classHeld
			}
			if j < len(w) {
				switch w[j] {
				case "DATABASE", "LANGUAGE", "FOREIGN", "TABLESPACE", "LARGE", "PARAMETER":
					return classCarried
				case "SCHEMA":
					if j+1 < len(w) && managedSchemas[strings.ToLower(w[j+1])] {
						return classCarried
					}
					return classHeld
				case "TABLE", "SEQUENCE", "FUNCTION", "PROCEDURE", "ROUTINE", "TYPE", "DOMAIN":
					j++
				}
			}
			if inManaged(w, j) {
				return classCarried
			}
			return classHeld
		}
		// no ON: GRANT role TO role, which a diff does not hold
	}
	return classCarried
}

// objectDDL classifies CREATE, ALTER, DROP and COMMENT ON of an object of a kind the engine models; kind
// is the index of the kind word.
func objectDDL(w []string, kind int) stmtClass {
	switch w[kind] {
	case "POLICY", "TRIGGER":
		// the engine reads the policies and triggers users add to the tables of the services, too
		return classHeld
	case "TABLE":
		if w[0] == "CREATE" && hasWord(w[kind+1:], "AS") {
			return classCarried // CREATE TABLE ... AS brings rows the diff does not
		}
	case "SCHEMA":
		if i := kind + 1; i < len(w) {
			for i < len(w) && fillers[w[i]] {
				i++
			}
			if i < len(w) && managedSchemas[strings.ToLower(w[i])] {
				return classCarried
			}
		}
		return classHeld
	case "EXTENSION":
		return classHeld
	case "INDEX":
		// CREATE INDEX [name] ON table: the table decides; ALTER and DROP INDEX name the index itself
		if w[0] == "CREATE" {
			for i := kind + 1; i < len(w); i++ {
				if w[i] == "ON" {
					if inManaged(w, i+1) {
						return classCarried
					}
					return classHeld
				}
			}
			return classHeld
		}
	}
	if inManaged(w, kind+1) {
		return classCarried
	}
	return classHeld
}

func hasWord(ws []string, w string) bool {
	for _, x := range ws {
		if x == w {
			return true
		}
	}
	return false
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// skipQuoted returns the index after the quote that closes the one at i, or -1 when it is not closed.
// A doubled quote stays inside; with esc a backslash escapes the next byte (an E'...' string).
func skipQuoted(src string, i int, q byte, esc bool) int {
	for j := i + 1; j < len(src); j++ {
		switch {
		case esc && src[j] == '\\':
			j++
		case src[j] == q:
			if j+1 < len(src) && src[j+1] == q {
				j++
				continue
			}
			return j + 1
		}
	}
	return -1
}

// dollarQuote returns the index after the dollar-quoted body that opens at i, or -1 when src[i] does not
// open one or it is not closed (reported by closed).
func dollarQuote(src string, i int) (end int, opens, closed bool) {
	j := i + 1
	for j < len(src) && (src[j] == '_' || src[j] >= 0x80 || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= 'A' && src[j] <= 'Z') || (j > i+1 && src[j] >= '0' && src[j] <= '9')) {
		j++
	}
	if j >= len(src) || src[j] != '$' {
		return 0, false, false
	}
	tag := src[i : j+1]
	k := strings.Index(src[j+1:], tag)
	if k < 0 {
		return 0, true, false
	}
	return j + 1 + k + len(tag), true, true
}

// splitSQL splits a script into statements at the semicolons outside quotes, comments, dollar-quoted
// bodies and a BEGIN ATOMIC ... END function body. ok is false when the script ends inside a quote or a
// comment.
func splitSQL(src string) (out []sqlStmt, ok bool) {
	var cur sqlStmt
	start, depth, caseDepth := 0, 0, 0
	atomic, seen := false, false
	flush := func(end int) {
		if seen {
			cur.text = strings.TrimSpace(src[start:end])
			out = append(out, cur)
		}
		cur, seen, atomic, caseDepth, depth = sqlStmt{}, false, false, 0, 0
		start = end + 1
	}
	word := func(w string) {
		if depth == 0 {
			cur.words = append(cur.words, w)
		}
		seen = true
	}
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case isSpace(c):
			i++
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			level := 1
			i += 2
			for i < len(src) && level > 0 {
				switch {
				case src[i] == '/' && i+1 < len(src) && src[i+1] == '*':
					level++
					i += 2
				case src[i] == '*' && i+1 < len(src) && src[i+1] == '/':
					level--
					i += 2
				default:
					i++
				}
			}
			if level > 0 {
				return nil, false
			}
		case c == '\'':
			esc := i > 0 && (src[i-1] == 'E' || src[i-1] == 'e') && (i < 2 || !isWordByte(src[i-2]))
			if i = skipQuoted(src, i, '\'', esc); i < 0 {
				return nil, false
			}
			word("?")
		case c == '"':
			if i = skipQuoted(src, i, '"', false); i < 0 {
				return nil, false
			}
			word("?")
		case c == '$':
			end, opens, closed := dollarQuote(src, i)
			switch {
			case opens && !closed:
				return nil, false
			case opens:
				i = end
				word("?")
			default:
				i++ // a parameter, $1
				seen = true
			}
		case c == ';':
			if atomic {
				i++
				continue
			}
			flush(i)
			i++
		case c == '.':
			word(".")
			i++
		case c == '(':
			depth++
			seen = true
			i++
		case c == ')':
			if depth > 0 {
				depth--
			}
			seen = true
			i++
		case isWordByte(c):
			j := i
			for j < len(src) && isWordByte(src[j]) {
				j++
			}
			w := strings.ToUpper(src[i:j])
			i = j
			if len(w) == 1 && i < len(src) && src[i] == '\'' {
				seen = true // the prefix of a string literal: E'...', B'...', X'...', N'...'
				continue
			}
			if depth == 0 {
				switch {
				case atomic && w == "CASE":
					caseDepth++
				case atomic && w == "END":
					if caseDepth > 0 {
						caseDepth--
					} else {
						atomic = false
					}
				case w == "ATOMIC" && len(cur.words) > 0 && cur.words[len(cur.words)-1] == "BEGIN" && cur.words[0] == "CREATE":
					atomic = true
				}
			}
			word(w)
		default:
			i++
			seen = true
		}
	}
	if seen {
		flush(len(src))
	}
	return out, true
}
