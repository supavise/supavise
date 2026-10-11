package advisors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/supavise/supavise/internal/versions"
)

// The vendored SQL records the supabase/supabase commit it was generated at. studio.tag of
// versions.yaml names the Studio build the dashboard runs, <date>-sha-<short commit>; when it moves,
// lints.sql must be generated again (gen/main.go) so that the advisors keep the lints of the
// Studio that shows them.
func TestVendoredSQLMatchesTheStudioPin(t *testing.T) {
	var v struct {
		Studio struct {
			Tag string `yaml:"tag"`
		} `yaml:"studio"`
	}
	if err := yaml.Unmarshal(versions.VersionsYAML, &v); err != nil {
		t.Fatal(err)
	}
	src := Vendored()
	if err := src.CheckStudioTag(v.Studio.Tag); err != nil {
		t.Fatal(err)
	}
	if src.Path != "packages/pg-meta/src/sql/studio/advisor/lints.ts" {
		t.Errorf("source path %q", src.Path)
	}
	if len(src.SourceSHA256) != 64 || src.DocsURL != DocsURL {
		t.Errorf("header %+v: want a source hash and docs-url %s", src, DocsURL)
	}
}

func TestVendoredSQLIsGeneratedOutput(t *testing.T) {
	for _, bad := range []string{"${", "\\`", "safeSql", "docsUrl"} {
		if strings.Contains(lintsSQL, bad) {
			t.Errorf("lints.sql still holds TypeScript (%q)", bad)
		}
	}
	if !strings.Contains(lintsSQL, "set local search_path = '';") {
		t.Error("lints.sql lost the search_path statement")
	}
	names := SQLLintNames()
	for _, want := range []string{"rls_disabled_in_public", "rls_enabled_no_policy", "security_definer_view", "function_search_path_mutable",
		"unindexed_foreign_keys", "unused_index", "duplicate_index", "auth_users_exposed", "auth_rls_initplan", "no_primary_key", "multiple_permissive_policies"} {
		if !slices.Contains(names, want) {
			t.Errorf("lint %s is not in lints.sql (found %d: %v)", want, len(names), names)
		}
	}
	if len(names) < 25 {
		t.Errorf("only %d lints found", len(names))
	}
	// Every lint carries the references its remediation link and docs need.
	if n := strings.Count(lintsSQL, "https://supabase.com/docs/guides/database/database-linter?lint="); n < 20 {
		t.Errorf("only %d remediation links", n)
	}
}

func TestQueryWrapsTheLintsLikeStudio(t *testing.T) {
	now := time.Date(2026, 10, 11, 1, 2, 3, 456_000_000, time.UTC)
	q := Query("public,graphql_public", now)
	for _, want := range []string{
		"set local pg_stat_statements.track = none;",
		"exception when others then",
		"set local statement_timeout = '30s';",
		"set local pgrst.db_schemas = 'public,graphql_public';",
		"-- source: dashboard\n-- user: self host\n-- date: 2026-10-11T01:02:03.456Z\n",
		"set local search_path = '';",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q", want)
		}
	}
	// The guard comes first, the lints last.
	if strings.Index(q, "pg_stat_statements") > strings.Index(q, "set local search_path") {
		t.Error("the pg_stat_statements guard must run before the lints")
	}
	// Quotes and backslashes in a schema list stay inside the literal.
	q = Query(`a'b\c`, now)
	if !strings.Contains(q, `set local pgrst.db_schemas = E'a''b\\c';`) {
		t.Errorf("schema list not quoted: %s", q[:200])
	}
	if strings.Contains(Query("", now), "set local pgrst.db_schemas") {
		t.Error("an empty schema list must not set pgrst.db_schemas")
	}
}

func TestParseRowsKeepsTheRowAsTheDatabaseSentIt(t *testing.T) {
	rows := `[{"name":"public_bucket_allows_listing","title":"t","level":"WARN","facing":"EXTERNAL","categories":["SECURITY"],"description":"d","detail":"x","remediation":"r","metadata":{"bucket_id":"avatars","name":"avatars","type":"storage"},"cache_key":"k1"},
	 {"name":"unused_index","title":"u","level":"INFO","facing":"EXTERNAL","categories":["PERFORMANCE"],"description":"d","detail":"x","remediation":"r","cache_key":"k2"}]`
	lints, err := ParseRows([]byte(rows))
	if err != nil {
		t.Fatal(err)
	}
	if len(lints) != 2 || lints[0].Name != "public_bucket_allows_listing" || lints[0].Metadata["bucket_id"] != "avatars" {
		t.Fatalf("parsed %+v", lints)
	}
	b, err := json.Marshal(lints)
	if err != nil {
		t.Fatal(err)
	}
	var back []map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if md := back[0]["metadata"].(map[string]any); md["bucket_id"] != "avatars" {
		t.Errorf("metadata.bucket_id, which Studio links on, was dropped: %v", md)
	}
	if _, has := back[1]["metadata"]; has {
		t.Error("a lint without metadata gained one")
	}
	if got := Names(lints); got != "public_bucket_allows_listing,unused_index" {
		t.Errorf("Names = %s", got)
	}
}

func TestParseRowsRefusesWhatIsNotLints(t *testing.T) {
	for name, in := range map[string]string{
		"object":        `{"error":"x"}`,
		"row not lint":  `[{"?column?":1}]`,
		"no categories": `[{"name":"a","categories":[]}]`,
		"not json":      `<html>`,
	} {
		if _, err := ParseRows([]byte(in)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if l, err := ParseRows([]byte(`[]`)); err != nil || len(l) != 0 || l == nil {
		t.Errorf("empty rows: %v %v", l, err)
	}
}

// The v1 routes and Studio split the lints by category; a lint in two categories is in both.
func TestFilterSplitsByCategory(t *testing.T) {
	rows := `[
	 {"name":"a","categories":["SECURITY"],"cache_key":"a"},
	 {"name":"b","categories":["PERFORMANCE"],"cache_key":"b"},
	 {"name":"c","categories":["SECURITY","PERFORMANCE"],"cache_key":"c"},
	 {"name":"d","categories":["HEALTH"],"cache_key":"d"}]`
	lints, err := ParseRows([]byte(rows))
	if err != nil {
		t.Fatal(err)
	}
	if got := Names(Filter(lints, CategorySecurity)); got != "a,c" {
		t.Errorf("security = %s", got)
	}
	if got := Names(Filter(lints, CategoryPerformance)); got != "b,c" {
		t.Errorf("performance = %s", got)
	}
	if got := Names(Filter(lints, CategoryHealth)); got != "d" {
		t.Errorf("health = %s", got)
	}
	if got := Filter(lints, "NOPE"); got == nil || len(got) != 0 {
		t.Errorf("no match must be an empty list, got %#v", got)
	}
}

// A project with the Auth defaults of a new Supavise project.
func defaults() AuthSettings {
	return AuthSettings{EmailEnabled: true, MailerOTPExp: 86400, DBMaxPoolSize: 5, DBMaxPoolSizeUnit: "connections"}
}

func configNamesOf(a AuthSettings) []string {
	var n []string
	for _, l := range ConfigLints(a) {
		n = append(n, l.Name)
	}
	return n
}

func TestConfigLintsOfTheDefaults(t *testing.T) {
	got := configNamesOf(defaults())
	want := []string{"auth_otp_long_expiry", "auth_db_connections_absolute"}
	if !slices.Equal(got, want) {
		t.Fatalf("lints of the defaults: %v, want %v", got, want)
	}
	byName := map[string]Lint{}
	for _, l := range ConfigLints(defaults()) {
		byName[l.Name] = l
	}
	for name, w := range map[string]struct {
		level, category, remediation string
	}{
		"auth_otp_long_expiry":         {LevelWarn, CategorySecurity, "https://supabase.com/docs/guides/platform/going-into-prod#security"},
		"auth_db_connections_absolute": {LevelInfo, CategoryPerformance, "https://supabase.com/docs/guides/deployment/going-into-prod"},
	} {
		l := byName[name]
		var obj map[string]any
		if err := json.Unmarshal(l.Raw, &obj); err != nil {
			t.Fatal(err)
		}
		if l.Level != w.level || !l.Has(w.category) || len(l.Categories) != 1 || obj["remediation"] != w.remediation || obj["facing"] != "EXTERNAL" {
			t.Errorf("%s: %s", name, l.Raw)
		}
		if obj["cache_key"] != name {
			t.Errorf("%s: cache_key %v", name, obj["cache_key"])
		}
		if md, _ := obj["metadata"].(map[string]any); md["type"] != "auth" || md["entity"] != "Auth" {
			t.Errorf("%s: metadata %v", name, obj["metadata"])
		}
		for _, f := range []string{"title", "description", "detail"} {
			if s, _ := obj[f].(string); s == "" {
				t.Errorf("%s: empty %s", name, f)
			}
		}
	}
	if d, _ := json.Marshal(byName["auth_db_connections_absolute"]); !strings.Contains(string(d), "at most 5 connections") {
		t.Errorf("the pool size is not in the detail: %s", d)
	}
}

func TestConfigLintsFollowTheSettings(t *testing.T) {
	cases := []struct {
		name   string
		change func(*AuthSettings)
		gone   string
		stays  []string
	}{
		{"otp expiry of an hour", func(a *AuthSettings) { a.MailerOTPExp = 3600 }, "auth_otp_long_expiry", nil},
		{"otp expiry just over an hour", func(a *AuthSettings) { a.MailerOTPExp = 3601 }, "", []string{"auth_otp_long_expiry"}},
		{"email provider off", func(a *AuthSettings) { a.EmailEnabled = false }, "auth_otp_long_expiry", nil},
		{"percent pool", func(a *AuthSettings) { a.DBMaxPoolSizeUnit = "percent" }, "auth_db_connections_absolute", nil},
	}
	for _, c := range cases {
		a := defaults()
		c.change(&a)
		got := configNamesOf(a)
		if c.gone != "" && slices.Contains(got, c.gone) {
			t.Errorf("%s: %s is still reported: %v", c.name, c.gone, got)
		}
		for _, s := range c.stays {
			if !slices.Contains(got, s) {
				t.Errorf("%s: %s is not reported: %v", c.name, s, got)
			}
		}
	}
	// Everything a hardened project can set: nothing is reported.
	hardened := AuthSettings{EmailEnabled: true, MailerOTPExp: 600, DBMaxPoolSize: 10, DBMaxPoolSizeUnit: "percent"}
	if got := configNamesOf(hardened); len(got) != 0 {
		t.Errorf("hardened project: %v", got)
	}
	if ls := ConfigLints(hardened); ls != nil {
		t.Errorf("want no lints, got %v", ls)
	}
}

// Hosted's recorded answers for a new project (CLI e2e suite, v2.120.0) hold auth_db_connections_absolute
// and nothing else of the Auth settings: mailer_otp_exp 3600 raises no auth_otp_long_expiry, and neither
// the leaked password nor the MFA settings raise a lint of their own.
func TestConfigLintsOfAHostedDefaultProject(t *testing.T) {
	hosted := AuthSettings{EmailEnabled: true, MailerOTPExp: 3600, DBMaxPoolSize: 10, DBMaxPoolSizeUnit: "connections"}
	got := ConfigLints(hosted)
	if len(got) != 1 || got[0].Name != "auth_db_connections_absolute" {
		t.Fatalf("lints of a hosted default project: %v", configNamesOf(hosted))
	}
	var obj map[string]any
	if err := json.Unmarshal(got[0].Raw, &obj); err != nil {
		t.Fatal(err)
	}
	// The recorded finding, word for word, with its 10 connections.
	for k, w := range map[string]string{
		"title":       "Auth DB Connection Strategy is not Percentage",
		"description": "Using a percentage based allocation connection strategy for Auth can help to improve the server's performance when increasing instance size.",
		"detail":      "Your project's Auth server is configured to use at most 10 connections. Increasing the instance size without manually adjusting this number will not improve the performance of the Auth server. Switch to a percentage based connection allocation strategy instead.",
		"remediation": "https://supabase.com/docs/guides/deployment/going-into-prod",
		"cache_key":   "auth_db_connections_absolute",
		"level":       "INFO",
		"facing":      "EXTERNAL",
	} {
		if obj[k] != w {
			t.Errorf("%s = %v, want %q", k, obj[k], w)
		}
	}
	if IsConfigLint("auth_leaked_password_protection") || IsConfigLint("auth_insufficient_mfa_options") {
		t.Error("a lint without a known trigger is computed")
	}
}

func TestConfigNamesListWhatConfigLintsCanReturn(t *testing.T) {
	worst := AuthSettings{EmailEnabled: true, MailerOTPExp: 1 << 30}
	for _, l := range ConfigLints(worst) {
		if !IsConfigLint(l.Name) {
			t.Errorf("%s is returned but IsConfigLint does not know it", l.Name)
		}
	}
	for n := range configNames {
		if !slices.Contains(configNamesOf(worst), n) {
			t.Errorf("%s is listed but never returned", n)
		}
	}
	for _, n := range SQLLintNames() {
		if IsConfigLint(n) {
			t.Errorf("%s is both a SQL lint and a configuration lint", n)
		}
	}
}

func TestHealthAnswers(t *testing.T) {
	u := Unavailable("log_auth_error_rate_high")
	if u.Name != "advisor_check_unavailable" || u.Level != LevelInfo || !u.Has(CategoryHealth) || u.CacheKey != "advisor_check_unavailable:logs_unavailable" {
		t.Errorf("unavailable: %s", u.Raw)
	}
	if !strings.Contains(string(u.Raw), "log_auth_error_rate_high") {
		t.Errorf("the check is not named: %s", u.Raw)
	}
	if n := NotActive(); n.Name != "project_not_active" || n.CacheKey != "project_not_active" {
		t.Errorf("not active: %s", n.Raw)
	}
	for _, n := range []string{"log_data_api_error_rate_high", "log_auth_error_rate_high", "log_storage_error_rate_high", "log_edge_function_error_rate_high", "instance_db_down"} {
		if !IsHealthCheck(n) {
			t.Errorf("%s is a health check", n)
		}
	}
	if IsHealthCheck("rls_disabled_in_public") || IsHealthCheck("advisor_check_unavailable") {
		t.Error("only health checks are health checks")
	}
}

func mustLints(t *testing.T, rows string) []Lint {
	t.Helper()
	l, err := ParseRows([]byte(rows))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestVisibleAppliesTheRules(t *testing.T) {
	lints := mustLints(t, `[
	 {"name":"rls_disabled_in_public","categories":["SECURITY"],"cache_key":"1","metadata":{"schema":"public","name":"a"}},
	 {"name":"rls_disabled_in_public","categories":["SECURITY"],"cache_key":"2","metadata":{"schema":"public","name":"b"}},
	 {"name":"unused_index","categories":["PERFORMANCE"],"cache_key":"3"}]`)
	const alice, bob = "aaaaaaaa-0000-4000-8000-000000000001", "bbbbbbbb-0000-4000-8000-000000000002"
	keys := func(ls []Lint) string {
		var k []string
		for _, l := range ls {
			k = append(k, l.CacheKey)
		}
		return strings.Join(k, ",")
	}
	for _, c := range []struct {
		name  string
		rules []Exception
		user  string
		want  string
	}{
		{"no rules", nil, alice, "1,2,3"},
		{"project-wide by name", []Exception{{LintName: "rls_disabled_in_public", IsDisabled: true}}, alice, "3"},
		{"by category", []Exception{{LintCategory: CategoryPerformance, IsDisabled: true}}, alice, "1,2"},
		{"by metadata", []Exception{{LintName: "rls_disabled_in_public", LintMetadata: map[string]any{"name": "a"}, IsDisabled: true}}, alice, "2,3"},
		{"for one member, that member", []Exception{{LintName: "unused_index", AssignedTo: alice, IsDisabled: true}}, alice, "1,2"},
		{"for one member, another member", []Exception{{LintName: "unused_index", AssignedTo: alice, IsDisabled: true}}, bob, "1,2,3"},
		{"only visible to one member, that member", []Exception{{LintName: "unused_index", AssignedTo: alice}}, alice, "1,2,3"},
		{"only visible to one member, another member", []Exception{{LintName: "unused_index", AssignedTo: alice}}, bob, "1,2"},
		{"enabled for the project changes nothing", []Exception{{LintName: "unused_index"}}, bob, "1,2,3"},
		{"a rule that selects nothing", []Exception{{LintName: "no_such_lint", IsDisabled: true}}, bob, "1,2,3"},
		{"a rule without selector hides all", []Exception{{IsDisabled: true}}, alice, ""},
	} {
		if got := keys(Visible(lints, c.rules, c.user)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// --- Runner ---

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)} }

const oneLint = `[{"name":"rls_disabled_in_public","title":"t","level":"ERROR","facing":"EXTERNAL","categories":["SECURITY"],"description":"d","detail":"x","remediation":"r","cache_key":"k"}]`

func TestRunnerCachesForTheTTL(t *testing.T) {
	ck := newClock()
	r := &Runner{TTL: 10 * time.Second, Now: ck.now}
	var runs int
	var lastSQL string
	exec := func(_ context.Context, q string) ([]byte, error) { runs++; lastSQL = q; return []byte(oneLint), nil }
	for i := 0; i < 3; i++ {
		l, err := r.SQL(context.Background(), "p1", "public", exec)
		if err != nil || len(l) != 1 {
			t.Fatalf("call %d: %v %v", i, l, err)
		}
	}
	if runs != 1 {
		t.Fatalf("%d runs for three calls inside the TTL", runs)
	}
	if !strings.Contains(lastSQL, "set local pgrst.db_schemas = 'public';") || !strings.Contains(lastSQL, "2026-10-11T00:00:00.000Z") {
		t.Errorf("the SQL sent: %.300s", lastSQL)
	}
	ck.add(9 * time.Second)
	_, _ = r.SQL(context.Background(), "p1", "public", exec)
	if runs != 1 {
		t.Errorf("run again before the TTL ended")
	}
	ck.add(2 * time.Second)
	_, _ = r.SQL(context.Background(), "p1", "public", exec)
	if runs != 2 {
		t.Errorf("%d runs after the TTL ended, want 2", runs)
	}
	// Another project has its own entry; another schema list is a different question.
	_, _ = r.SQL(context.Background(), "p2", "public", exec)
	_, _ = r.SQL(context.Background(), "p1", "public,api", exec)
	if runs != 4 {
		t.Errorf("%d runs, want 4", runs)
	}
	// An empty list means the default one, and shares the entry of the default.
	_, _ = r.SQL(context.Background(), "p3", "", exec)
	_, _ = r.SQL(context.Background(), "p3", DefaultExposedSchemas, exec)
	if runs != 5 {
		t.Errorf("%d runs, want 5", runs)
	}
}

func TestRunnerSharesARunBetweenSimultaneousCallers(t *testing.T) {
	r := &Runner{}
	var runs atomic.Int32
	release := make(chan struct{})
	exec := func(context.Context, string) ([]byte, error) {
		runs.Add(1)
		<-release
		return []byte(oneLint), nil
	}
	var wg sync.WaitGroup
	results := make([][]Lint, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := r.SQL(context.Background(), "p", "public", exec)
			if err != nil {
				t.Error(err)
			}
			results[i] = l
		}()
	}
	// Let every caller join the run before it finishes.
	deadline := time.Now().Add(5 * time.Second)
	for runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := runs.Load(); n != 1 {
		t.Fatalf("%d runs for eight simultaneous callers", n)
	}
	for i, l := range results {
		if len(l) != 1 {
			t.Errorf("caller %d got %v", i, l)
		}
	}
}

func TestRunnerDoesNotCacheFailures(t *testing.T) {
	r := &Runner{}
	boom := errors.New("pg-meta is down")
	var runs int
	exec := func(context.Context, string) ([]byte, error) {
		runs++
		if runs == 1 {
			return nil, boom
		}
		return []byte(oneLint), nil
	}
	if _, err := r.SQL(context.Background(), "p", "", exec); !errors.Is(err, boom) {
		t.Fatalf("first call: %v", err)
	}
	if l, err := r.SQL(context.Background(), "p", "", exec); err != nil || len(l) != 1 {
		t.Fatalf("second call: %v %v", l, err)
	}
	// Rows that are not lints are a failure too, and are not kept.
	bad := func(context.Context, string) ([]byte, error) { return []byte(`[{"?column?":1}]`), nil }
	if _, err := r.SQL(context.Background(), "q", "", bad); err == nil {
		t.Error("rows that are not lints were accepted")
	}
}

func TestRunnerInvalidateDropsTheCacheAndAStaleRun(t *testing.T) {
	r := &Runner{}
	var runs int
	exec := func(context.Context, string) ([]byte, error) { runs++; return []byte(oneLint), nil }
	_, _ = r.SQL(context.Background(), "p", "", exec)
	r.Invalidate("p")
	_, _ = r.SQL(context.Background(), "p", "", exec)
	if runs != 2 {
		t.Fatalf("%d runs, want 2: Invalidate must force a new run", runs)
	}

	// A run that started before Invalidate describes the schema before the change: it answers its
	// callers but is not kept.
	r2 := &Runner{}
	started, release := make(chan struct{}), make(chan struct{})
	var runs2 atomic.Int32
	slow := func(context.Context, string) ([]byte, error) {
		runs2.Add(1)
		if runs2.Load() == 1 {
			close(started)
			<-release
		}
		return []byte(oneLint), nil
	}
	done := make(chan error, 1)
	go func() { _, err := r2.SQL(context.Background(), "p", "", slow); done <- err }()
	<-started
	r2.Invalidate("p")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := r2.SQL(context.Background(), "p", "", slow); err != nil {
		t.Fatal(err)
	}
	if runs2.Load() != 2 {
		t.Errorf("%d runs: the result of the run that predates Invalidate was kept", runs2.Load())
	}
}

// A burst of Invalidate calls (a script running many statements through the API) must not start a
// run beside the one under way: the project has one run at a time, and the callers that wait for
// it share the single run that follows.
func TestRunnerRunsOneQueryAtATimePerProject(t *testing.T) {
	r := &Runner{}
	var runs, live, peak atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	exec := func(context.Context, string) ([]byte, error) {
		n := live.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer live.Add(-1)
		if runs.Add(1) == 1 {
			close(started)
			<-release
		} else {
			time.Sleep(20 * time.Millisecond)
		}
		return []byte(oneLint), nil
	}
	var wg sync.WaitGroup
	call := func() {
		defer wg.Done()
		if l, err := r.SQL(context.Background(), "p", "", exec); err != nil || len(l) != 1 {
			t.Errorf("SQL: %v %v", l, err)
		}
	}
	wg.Add(1)
	go call()
	<-started
	for i := 0; i < 5; i++ {
		r.Invalidate("p")
		wg.Add(1)
		go call()
	}
	time.Sleep(50 * time.Millisecond)
	if n := runs.Load(); n != 1 {
		t.Fatalf("%d runs started while the first was under way, want 1", n)
	}
	close(release)
	wg.Wait()
	if n := runs.Load(); n != 2 {
		t.Errorf("%d runs, want 2: the callers that waited share one follow-up run", n)
	}
	if p := peak.Load(); p != 1 {
		t.Errorf("%d runs at the same time for one project", p)
	}
	// The follow-up run was started after the last Invalidate, so it is the one kept.
	if _, err := r.SQL(context.Background(), "p", "", exec); err != nil || runs.Load() != 2 {
		t.Errorf("the result of the follow-up run was not kept (%d runs, %v)", runs.Load(), err)
	}
}

func TestRunnerCallerLeavesWhileWaitingForAStaleRun(t *testing.T) {
	r := &Runner{}
	started, release := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	exec := func(context.Context, string) ([]byte, error) {
		if runs.Add(1) == 1 {
			close(started)
			<-release
		}
		return []byte(oneLint), nil
	}
	go func() { _, _ = r.SQL(context.Background(), "p", "", exec) }()
	<-started
	r.Invalidate("p")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := r.SQL(ctx, "p", "", exec); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the caller's own deadline", err)
	}
	if n := runs.Load(); n != 1 {
		t.Errorf("%d runs: the caller that left started one", n)
	}
	close(release)
}

func TestRunnerBoundsTheRunsAcrossProjects(t *testing.T) {
	r := &Runner{MaxConcurrent: 2}
	var live, peak atomic.Int32
	exec := func(context.Context, string) ([]byte, error) {
		n := live.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		live.Add(-1)
		return []byte(oneLint), nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := r.SQL(context.Background(), fmt.Sprintf("p%d", i), "", exec); err != nil {
				t.Errorf("p%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if p := peak.Load(); p > 2 {
		t.Errorf("peak of %d runs at once, want at most 2", p)
	}
}

func TestRunnerDropsExpiredEntries(t *testing.T) {
	ck := newClock()
	r := &Runner{TTL: 10 * time.Second, Now: ck.now}
	exec := func(context.Context, string) ([]byte, error) { return []byte(`[]`), nil }
	for _, k := range []string{"p1", "p2"} {
		_, _ = r.SQL(context.Background(), k, "", exec)
	}
	ck.add(30 * time.Second)
	_, _ = r.SQL(context.Background(), "p3", "", exec)
	if len(r.cache) != 1 {
		t.Errorf("%d entries kept after the TTL of the others ended, want 1", len(r.cache))
	}
	// A deleted project leaves nothing behind.
	r.Invalidate("p3")
	if len(r.cache) != 0 || len(r.flight) != 0 {
		t.Errorf("Invalidate left %d entries and %d runs", len(r.cache), len(r.flight))
	}
}

func TestRunnerKeepsARunWhoseFirstCallerLeaves(t *testing.T) {
	r := &Runner{}
	started, release := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	exec := func(ctx context.Context, _ string) ([]byte, error) {
		runs.Add(1)
		close(started)
		select {
		case <-release:
			return []byte(oneLint), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := r.SQL(ctx1, "p", "", exec); first <- err }()
	<-started
	second := make(chan []Lint, 1)
	go func() { l, _ := r.SQL(context.Background(), "p", "", exec); second <- l }()
	time.Sleep(30 * time.Millisecond)
	cancel1()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller that left got %v", err)
	}
	close(release)
	if l := <-second; len(l) != 1 {
		t.Fatalf("the caller that stayed got %v", l)
	}
	if runs.Load() != 1 {
		t.Errorf("%d runs", runs.Load())
	}
}

func TestRunnerBoundsTheEntries(t *testing.T) {
	ck := newClock()
	r := &Runner{MaxEntries: 3, TTL: time.Hour, Now: ck.now}
	var runs int
	exec := func(context.Context, string) ([]byte, error) { runs++; return []byte(`[]`), nil }
	for i := 0; i < 5; i++ {
		_, _ = r.SQL(context.Background(), fmt.Sprintf("p%d", i), "", exec)
		ck.add(time.Second)
	}
	if len(r.cache) != 3 {
		t.Fatalf("%d entries kept, want 3", len(r.cache))
	}
	// The oldest went; the newest stay.
	before := runs
	_, _ = r.SQL(context.Background(), "p4", "", exec)
	if runs != before {
		t.Error("the newest entry was evicted")
	}
	_, _ = r.SQL(context.Background(), "p0", "", exec)
	if runs != before+1 {
		t.Error("the oldest entry was kept")
	}
}

func TestIsTimeout(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{context.DeadlineExceeded, true},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), true},
		{errors.New("canceling statement due to statement timeout"), true},
		{errors.New("Canceling statement due to Statement Timeout"), true},
		{errors.New("syntax error"), false},
	} {
		if got := IsTimeout(c.err); got != c.want {
			t.Errorf("IsTimeout(%v) = %v", c.err, got)
		}
	}
}

// The check above is only worth having if it fails when the tag moves.
func TestCheckStudioTagFailsOnAnotherCommit(t *testing.T) {
	src := Source{Commit: "94b8b06eb294cf6b217c68d30357566cc8f146d9"}
	if err := src.CheckStudioTag("2026.10.05-sha-94b8b06"); err != nil {
		t.Errorf("the commit of the tag: %v", err)
	}
	for _, tag := range []string{"2026.10.20-sha-1a2b3c4", "2026.10.05-sha-94b8b07", "2026.10.05", "2026.10.05-sha-94", ""} {
		if err := src.CheckStudioTag(tag); err == nil {
			t.Errorf("tag %q accepted", tag)
		}
	}
	if err := (Source{Commit: "94b8b06"}).CheckStudioTag("2026.10.05-sha-94b8b06"); err == nil {
		t.Error("a short commit in the header was accepted")
	}
}
