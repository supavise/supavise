package main

import (
	"strings"
	"testing"
)

const commit = "94b8b06eb294cf6b217c68d30357566cc8f146d9"

// ts is the shape of lints.ts: other exports around getLintsSQL, a template with escaped
// backticks, JavaScript escapes, and ${literal(`...${docsUrl}...`)} for the strings that carry a
// link or a format string.
var ts = "import { literal, safeSql } from '../../../pg-format'\n" +
	"export const enrichLintsQuery = (q: string) => safeSql`do $$ begin ${q} end $$`\n" +
	"/** Replace docs url with docsUrl (${docsUrl}) */\n" +
	"export const getLintsSQL = ({ docsUrl }: { docsUrl: string }): SafeSqlFragment =>\n" +
	"  safeSql`set local search_path = '';\n" +
	"select 'x' as name,\n" +
	"  'Table \\`%s\\` is open' as detail,\n" +
	"  ${literal(`${docsUrl}/guides?lint=0001`)} as remediation,\n" +
	"  ${literal(`Table \\`%s.%s\\` it's here; see [docs](${docsUrl}/a)`)} as d2,\n" +
	"  ${literal(`a\\\\b`)} as d3,\n" +
	"  qual like '%current\\_setting(%)%',\n" +
	"  replace(q, E'\\n', ''),\n" +
	"  $re$\\\\A\\\\s*$re$\n" +
	"order by 1`\n" +
	"export const after = safeSql`select 1`\n"

func TestExtractCooksTheTemplateLikeSafeSql(t *testing.T) {
	got, err := ExtractLintsSQL(ts, "https://example.test/docs")
	if err != nil {
		t.Fatal(err)
	}
	want := "set local search_path = '';\n" +
		"select 'x' as name,\n" +
		"  'Table `%s` is open' as detail,\n" +
		"  'https://example.test/docs/guides?lint=0001' as remediation,\n" +
		"  'Table `%s.%s` it''s here; see [docs](https://example.test/docs/a)' as d2,\n" +
		"  E'a\\\\b' as d3,\n" +
		"  qual like '%current_setting(%)%',\n" +
		"  replace(q, E'\n', ''),\n" +
		"  $re$\\A\\s*$re$\n" +
		"order by 1"
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestGenerateRecordsTheSource(t *testing.T) {
	out, err := Generate(ts, commit, DefaultDocsURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"-- source: packages/pg-meta/src/sql/studio/advisor/lints.ts (getLintsSQL)\n",
		"-- commit: " + commit + "\n",
		"-- docs-url: https://supabase.com/docs\n",
		"Apache-2.0",
		"\nset local search_path = '';\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, "order by 1\n") {
		t.Errorf("output ends %q", out[len(out)-20:])
	}
	again, _ := Generate(ts, commit, DefaultDocsURL)
	if again != out {
		t.Error("the output is not deterministic")
	}
	if other, _ := Generate(ts+" ", commit, DefaultDocsURL); other == out {
		t.Error("the header does not change with the source")
	}
}

func TestExtractRefusesWhatItCannotEvaluate(t *testing.T) {
	for name, src := range map[string]string{
		"no function":        "export const x = 1",
		"no template":        "export const getLintsSQL = () => 1",
		"other expression":   "export const getLintsSQL = () => safeSql`select ${1 + 1}`",
		"other call":         "export const getLintsSQL = () => safeSql`select ${quote(`a`)}`",
		"other var in quote": "export const getLintsSQL = () => safeSql`select ${literal(`${other}/x`)}`",
		"unclosed literal":   "export const getLintsSQL = () => safeSql`select ${literal(`a`}`",
		"unterminated":       "export const getLintsSQL = () => safeSql`select 1",
		"octal":              "export const getLintsSQL = () => safeSql`select \\1`",
	} {
		if out, err := ExtractLintsSQL(src, DefaultDocsURL); err == nil {
			t.Errorf("%s: accepted, produced %q", name, out)
		}
	}
}

func TestEscapes(t *testing.T) {
	for in, want := range map[string]string{
		`\n`:        "\n",
		`\t`:        "\t",
		`\x41`:      "A",
		`\u0041`:    "A",
		`\u{1F600}`: "\U0001F600",
		`\$`:        "$",
		`\'`:        "'",
		"\\\nx":     "x", // a line continuation
	} {
		got, err := ExtractLintsSQL("export const getLintsSQL = () => safeSql`"+in+"`", DefaultDocsURL)
		if err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestRunNeedsAFullCommit(t *testing.T) {
	for _, c := range []string{"", "94b8b06", strings.ToUpper(commit), commit + "0"} {
		if err := run("x", c, "", DefaultDocsURL); err == nil {
			t.Errorf("commit %q accepted", c)
		}
	}
}

func TestPgLiteral(t *testing.T) {
	for in, want := range map[string]string{
		"plain":     `'plain'`,
		"it's":      `'it''s'`,
		`a\b`:       `E'a\\b'`,
		`a\b'c`:     `E'a\\b''c'`,
		"":          `''`,
		"two\nline": "'two\nline'",
	} {
		if got := pgLiteral(in); got != want {
			t.Errorf("pgLiteral(%q) = %s, want %s", in, got, want)
		}
	}
}
