package github

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func snapshotOf(t *testing.T, h *harness, sha string, files map[string]string) *Snapshot {
	t.Helper()
	h.gh.setCommit(sha, files)
	sn, err := h.svc.loadSnapshot(context.Background(), "acme/app", h.gh.instID, "", sha)
	if err != nil {
		t.Fatal(err)
	}
	return sn
}

func TestSnapshotReadsOnlyTheSupabaseDirectory(t *testing.T) {
	h := newHarness(t)
	sn := snapshotOf(t, h, "s1", repoFiles(nil))
	if sn.Dir != "supabase" || !sn.Has("config.toml") || !sn.Has("migrations/20240101000000_init.sql") || sn.Has("README.md") {
		t.Fatalf("entries = %v", sn.Files(""))
	}
	b, err := sn.Read(context.Background(), "seed.sql", 1<<20)
	if err != nil || !strings.Contains(string(b), "insert into") {
		t.Fatalf("seed = %q, %v", b, err)
	}
	if _, err := sn.Read(context.Background(), "seed.sql", 5); err == nil {
		t.Fatal("a file over the limit was read")
	}
	if _, err := sn.Read(context.Background(), "missing.sql", 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing file: %v", err)
	}
	// With a working directory it is under it; without a supabase directory there is nothing.
	h.gh.setCommit("s2", map[string]string{"apps/web/supabase/config.toml": "x", "supabase/other.toml": "y"})
	sn2, err := h.svc.loadSnapshot(context.Background(), "acme/app", h.gh.instID, "apps/web", "s2")
	if err != nil || sn2.Dir != "apps/web/supabase" || !sn2.Has("config.toml") || sn2.Has("other.toml") {
		t.Fatalf("with a working directory: %v, %v", sn2, err)
	}
	h.gh.setCommit("s3", map[string]string{"README.md": "x"})
	if _, err := h.svc.loadSnapshot(context.Background(), "acme/app", h.gh.instID, "", "s3"); !errors.Is(err, ErrNoSupabaseDir) {
		t.Fatalf("no supabase directory: %v", err)
	}
	if _, err := h.svc.loadSnapshot(context.Background(), "acme/app", h.gh.instID, "", "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown commit: %v", err)
	}
}

func TestSnapshotOfARepositoryTooBigForOneTreeDescendsToTheDirectory(t *testing.T) {
	h := newHarness(t)
	h.gh.truncated = true
	files := repoFiles(map[string]string{"apps/web/index.ts": "x"})
	h.gh.setCommit("big", files)
	sn, err := h.svc.loadSnapshot(context.Background(), "acme/app", h.gh.instID, "", "big")
	if err != nil {
		t.Fatal(err)
	}
	if !sn.Has("migrations/20240101000000_init.sql") || !sn.Has("functions/hello/index.ts") || sn.Has("apps/web/index.ts") {
		t.Fatalf("entries = %v", sn.Files(""))
	}
	// And under a working directory.
	h.gh.setCommit("big2", map[string]string{"apps/web/supabase/config.toml": "x", "apps/web/supabase/migrations/1_a.sql": "select 1", "other/file": "y"})
	sn2, err := h.svc.loadSnapshot(context.Background(), "acme/app", h.gh.instID, "apps/web", "big2")
	if err != nil || !sn2.Has("migrations/1_a.sql") {
		t.Fatalf("under a working directory: %v, %v", sn2, err)
	}
	h.gh.setCommit("big3", map[string]string{"README.md": "x"})
	if _, err := h.svc.loadSnapshot(context.Background(), "acme/app", h.gh.instID, "", "big3"); !errors.Is(err, ErrNoSupabaseDir) {
		t.Fatalf("no supabase directory in a big repository: %v", err)
	}
}

func TestMigrationsAreReadInVersionOrderAsTheCLINamesThem(t *testing.T) {
	h := newHarness(t)
	sn := snapshotOf(t, h, "s1", map[string]string{
		"supabase/migrations/20240201000000_b.sql":     "select 2;",
		"supabase/migrations/20240101000000_a.sql":     "\ufeffselect 1;",
		"supabase/migrations/9_short.sql":              "select 0;",
		"supabase/migrations/notes.md":                 "# notes",
		"supabase/migrations/README.sql":               "select 'readme';",
		"supabase/migrations/20240301000000_empty.sql": "   \n",
		"supabase/migrations/sub/20240401000000_x.sql": "select 'nested';",
	})
	ms, skipped, err := sn.Migrations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var versions []string
	for _, m := range ms {
		versions = append(versions, m.Version)
	}
	if !slices.Equal(versions, []string{"9", "20240101000000", "20240201000000"}) {
		t.Fatalf("versions = %v", versions)
	}
	if ms[1].Name != "a" || ms[1].Statements[0] != "select 1;" {
		t.Fatalf("migration = %+v (a byte order mark must go)", ms[1])
	}
	if !slices.Contains(skipped, "README.sql") || !slices.Contains(skipped, "20240301000000_empty.sql (empty)") || slices.Contains(skipped, "notes.md") {
		t.Fatalf("skipped = %v", skipped)
	}
	// Two files with one version are an error: the CLI refuses them too.
	dup := snapshotOf(t, h, "s2", map[string]string{"supabase/migrations/20240101000000_a.sql": "x", "supabase/migrations/20240101000000_b.sql": "y"})
	if _, _, err := dup.Migrations(context.Background()); err == nil || !strings.Contains(err.Error(), "same version") {
		t.Fatalf("duplicate versions: %v", err)
	}
}

func TestFunctionsShareUnderscoreDirectoriesAndTheImportMap(t *testing.T) {
	h := newHarness(t)
	sn := snapshotOf(t, h, "s1", map[string]string{
		"supabase/functions/hello/index.ts":     "hello",
		"supabase/functions/hello/deno.json":    "{}",
		"supabase/functions/world/index.ts":     "world",
		"supabase/functions/world/lib/util.ts":  "util",
		"supabase/functions/_shared/cors.ts":    "cors",
		"supabase/functions/import_map.json":    "{}",
		"supabase/functions/readme/README.md":   "not a function",
		"supabase/functions/Bad Name/index.ts":  "x",
		"supabase/functions/off/index.ts":       "off",
		"supabase/functions/static/index.ts":    "static",
		"supabase/functions/static/data/a.json": "{}",
		"supabase/functions/custom/main.ts":     "custom",
	})
	off := false
	fns, notes, err := sn.Functions(context.Background(), map[string]FunctionConfig{
		"off":    {Enabled: &off},
		"custom": {Entrypoint: "./functions/custom/main.ts"},
		"world":  {ImportMap: "./functions/import_map.json"},
		"ghost":  {},
		"static": {Static: []string{"./functions/static/data/*.json"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]RepoFunction{}
	for _, f := range fns {
		by[f.Slug] = f
	}
	for _, slug := range []string{"hello", "world", "custom", "static"} {
		if _, ok := by[slug]; !ok {
			t.Errorf("function %s is not deployed (notes: %v)", slug, notes)
		}
	}
	for _, slug := range []string{"off", "readme", "ghost", "Bad Name"} {
		if _, ok := by[slug]; ok {
			t.Errorf("%s was deployed", slug)
		}
	}
	paths := func(f RepoFunction) []string {
		var p []string
		for _, fl := range f.Files {
			p = append(p, fl.Path)
		}
		return p
	}
	hello := by["hello"]
	if hello.Entrypoint != "functions/hello/index.ts" || hello.ImportMap != "functions/hello/deno.json" || !hello.VerifyJWT {
		t.Fatalf("hello = %+v", hello.FunctionDeploy)
	}
	if got := paths(hello); !slices.Contains(got, "functions/_shared/cors.ts") || !slices.Contains(got, "functions/import_map.json") || slices.Contains(got, "functions/world/index.ts") {
		t.Fatalf("files of hello = %v", got)
	}
	if world := by["world"]; world.ImportMap != "functions/import_map.json" || !slices.Contains(paths(world), "functions/world/lib/util.ts") {
		t.Fatalf("world = %+v", world.FunctionDeploy)
	}
	if c := by["custom"]; c.Entrypoint != "functions/custom/main.ts" {
		t.Fatalf("custom = %+v", c.FunctionDeploy)
	}
	if s := by["static"]; !slices.Contains(paths(s), "functions/static/data/a.json") || len(s.StaticPatterns) != 1 {
		t.Fatalf("static = %+v", s.FunctionDeploy)
	}
	notesText := strings.Join(notes, "\n")
	for _, want := range []string{"off is disabled", "functions/readme has no index.ts", "ghost", "Bad Name"} {
		if !strings.Contains(notesText, want) {
			t.Errorf("no note about %q:\n%s", want, notesText)
		}
	}
	// A function whose hash is what was deployed before is described but not read.
	again, _, _ := sn.Functions(context.Background(), map[string]FunctionConfig{}, map[string]string{"hello": hello.Hash})
	for _, f := range again {
		if f.Slug == "hello" && len(f.Files) != 0 {
			t.Fatal("an unchanged function was read again")
		}
		if f.Slug == "world" && len(f.Files) == 0 {
			t.Fatal("a changed function was not read")
		}
	}
	// The hash follows the content, the entrypoint and the options.
	sn2 := snapshotOf(t, h, "s2", map[string]string{"supabase/functions/hello/index.ts": "HELLO", "supabase/functions/_shared/cors.ts": "cors", "supabase/functions/hello/deno.json": "{}", "supabase/functions/import_map.json": "{}"})
	changed, _, _ := sn2.Functions(context.Background(), nil, nil)
	if len(changed) != 1 || changed[0].Hash == hello.Hash {
		t.Fatalf("hash after a change = %+v", changed)
	}
	f := false
	nojwt, _, _ := sn.Functions(context.Background(), map[string]FunctionConfig{"hello": {VerifyJWT: &f}}, nil)
	for _, fn := range nojwt {
		if fn.Slug == "hello" && (fn.Hash == hello.Hash || fn.VerifyJWT) {
			t.Fatal("verify_jwt = false did not reach the function or its hash")
		}
	}
}

func TestMatchGlobsOfConfigToml(t *testing.T) {
	h := newHarness(t)
	sn := snapshotOf(t, h, "s1", map[string]string{"supabase/seed.sql": "a", "supabase/seeds/b.sql": "b", "supabase/seeds/a.sql": "a", "supabase/seeds/deep/c.sql": "c"})
	for pat, want := range map[string][]string{
		"./seed.sql":       {"seed.sql"},
		"seed.sql":         {"seed.sql"},
		"./seeds/*.sql":    {"seeds/a.sql", "seeds/b.sql"},
		"./seeds/**/*.sql": {"seeds/a.sql", "seeds/b.sql", "seeds/deep/c.sql"},
		"./nothing/*.sql":  nil,
	} {
		got, err := sn.Match(pat)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("Match(%q) = %v, %v; want %v", pat, got, err, want)
		}
	}
}
