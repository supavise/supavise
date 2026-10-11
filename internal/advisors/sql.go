package advisors

import (
	_ "embed"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// lintsSQL is the vendored lint query (see the README of this package and gen/main.go).
//
//go:embed lints.sql
var lintsSQL string

// Source says where the vendored SQL came from, as its header records it.
type Source struct {
	Path, Commit, SourceSHA256, DocsURL string
}

// Vendored returns the header of lints.sql.
func Vendored() Source {
	var s Source
	for _, line := range strings.Split(lintsSQL, "\n") {
		rest, ok := strings.CutPrefix(line, "-- ")
		if !ok {
			break
		}
		k, v, ok := strings.Cut(rest, ": ")
		if !ok {
			continue
		}
		switch k {
		case "source":
			s.Path, _, _ = strings.Cut(v, " ")
		case "commit":
			s.Commit = v
		case "source-sha256":
			s.SourceSHA256 = v
		case "docs-url":
			s.DocsURL = v
		}
	}
	return s
}

// CheckStudioTag returns an error when the SQL was not generated at the commit that studio.tag of
// internal/versions/versions.yaml names (<date>-sha-<short commit>).
func (s Source) CheckStudioTag(tag string) error {
	_, short, ok := strings.Cut(tag, "-sha-")
	if !ok || len(short) < 7 {
		return fmt.Errorf("studio.tag %q does not end in -sha-<commit>", tag)
	}
	if len(s.Commit) != 40 || !strings.HasPrefix(s.Commit, short) {
		return fmt.Errorf("lints.sql was generated at commit %q but studio.tag is %q (commit %s...): run `go run ./internal/advisors/gen` again, as internal/advisors/README.md says",
			s.Commit, tag, short)
	}
	return nil
}

// StatementTimeout is how long the lint query may run in the database.
const StatementTimeout = 30 * time.Second

// DefaultExposedSchemas is what Studio uses when PostgREST's schema list is unknown, and
// PostgREST's own default in a Supabase project.
const DefaultExposedSchemas = "public,graphql_public"

// Query returns the SQL to send for a project whose PostgREST exposes the comma-separated
// exposedSchemas: Studio's enrichLintsQuery around the vendored lints.
//
//   - pg_stat_statements.track is switched off for the transaction so the lints do not fill the
//     statistics Query Performance shows; where that is not allowed (no superuser, no extension)
//     the guard skips silently, as in Studio. It is "set local" here where Studio sets the session,
//     so a pooled connection does not stay untracked.
//   - pgrst.db_schemas is set for the transaction because the lints that ask what the Data API
//     exposes read it with current_setting, as PostgREST's schema cache does.
//   - the "source" and "user" comments are Studio's, which Query Performance uses to leave its own
//     queries out.
//   - statement_timeout bounds the run.
func Query(exposedSchemas string, now time.Time) string {
	var b strings.Builder
	b.WriteString("do $$\nbegin\n  set local pg_stat_statements.track = none;\nexception when others then\n  -- not a superuser or extension not installed, skip silently\nend\n$$;\n")
	b.WriteString("set local statement_timeout = '" + strconv.Itoa(int(StatementTimeout/time.Second)) + "s';\n")
	if exposedSchemas != "" {
		b.WriteString("set local pgrst.db_schemas = " + sqlString(exposedSchemas) + ";\n")
	}
	b.WriteString("-- source: dashboard\n-- user: self host\n-- date: " + now.UTC().Format("2006-01-02T15:04:05.000Z") + "\n\n")
	b.WriteString(lintsSQL)
	return b.String()
}

// sqlString quotes s as a SQL string, as pg-format's literal does.
func sqlString(s string) string {
	q := "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", "''") + "'"
	if strings.Contains(s, `\`) {
		return "E" + q
	}
	return q
}

var sqlNameRe = regexp.MustCompile(`(?m)^\s*'([a-z][a-z0-9_]*)' as name,\s*$`)

var sqlNames = func() map[string]bool {
	m := map[string]bool{}
	for _, g := range sqlNameRe.FindAllStringSubmatch(lintsSQL, -1) {
		m[g[1]] = true
	}
	return m
}()

// IsSQLLint reports whether name is a lint of the vendored SQL.
func IsSQLLint(name string) bool { return sqlNames[name] }

// SQLLintNames returns the names of the lints of the vendored SQL, in the order of the file.
func SQLLintNames() []string {
	var out []string
	for _, g := range sqlNameRe.FindAllStringSubmatch(lintsSQL, -1) {
		out = append(out, g[1])
	}
	return out
}
