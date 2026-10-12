package github

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
)

// The runner. It applies one commit of a repository to one project: a preview branch (built
// from nothing but the repository when it is new, brought up to the commit when it exists) or
// production. The steps are the Management API's DAG, in the order the dashboard shows them:
//
//	clone      list the supabase directory at the commit
//	pull       the branch's project is made (a new branch only)
//	health     the branch's database answers
//	configure  config.toml, with its [remotes] block, saved as the branch's settings (PostgREST's
//	           part waits for the end of migrate, which may make the schemas it exposes)
//	migrate    the migrations the branch does not have yet, each in its own transaction
//	seed       the seed files, once per branch: in the first run that gets this far
//	deploy     Edge Functions that changed, and the Storage buckets of config.toml

var errSkip = errors.New("skipped")

// repoSource builds or updates one branch from a Snapshot. As a branching.SchemaSource it builds a
// new branch; drive does the same steps for an existing one.
type repoSource struct {
	s    *Service
	conn registry.GitHubConnection
	sn   *Snapshot
	// job ends when newer work replaces this (queue.go).
	job context.Context
	// persistent says how config.toml applies; fresh whether the branch was made from this commit
	// (the words of the run's detail); seedDue whether the branch has not been seeded yet, which
	// is not the same: a first run that failed before its seed step leaves the seed due.
	persistent, fresh, seedDue bool
	have                       map[string]string // function hashes deployed before

	mu     sync.Mutex
	plan   *ConfigPlan
	hashes map[string]string
	stats  runStats
}

type runStats struct {
	migrations, functions int
	seeded, configured    bool
	// seedDone is true once the seed step ended without an error, whether it applied a seed or
	// found nothing to apply: the branch's link then records that the seed is not due any more.
	seedDone bool
}

// linked returns ctx that also ends when the job does.
func (r *repoSource) linked(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.job, cancel)
	return ctx, func() { stop(); cancel() }
}

// step runs fn as a step of the run: started, then finished, failed with fn's error or skipped
// when fn returns errSkip.
func step(run *branching.Run, name string, fn func() error) error {
	st := run.Step(name)
	st.Start()
	err := fn()
	switch {
	case err == nil:
		st.Finish()
	case errors.Is(err, errSkip):
		st.Skip()
		return nil
	default:
		st.Fail(err)
	}
	return err
}

// Schema implements branching.SchemaSource: it runs in place of the replay of the parent's
// migrations and seed.
func (r *repoSource) Schema(ctx context.Context, run *branching.Run, ref string) (string, error) {
	ctx, done := r.linked(ctx)
	defer done()
	if err := step(run, branching.StepClone, func() error { return r.clone(run) }); err != nil {
		return "", err
	}
	run.Step(branching.StepHealth).Finish()
	return r.apply(ctx, run, ref)
}

// apply runs configure, migrate and seed.
func (r *repoSource) apply(ctx context.Context, run *branching.Run, ref string) (string, error) {
	if err := step(run, branching.StepConfigure, func() error { return r.configure(ctx, run, ref) }); err != nil {
		return "", err
	}
	if err := step(run, branching.StepMigrate, func() error { return r.migrate(ctx, run, ref) }); err != nil {
		return "", err
	}
	if err := step(run, branching.StepSeed, func() error { return r.seed(ctx, run, ref) }); err != nil {
		return "", err
	}
	r.mu.Lock()
	r.stats.seedDone = true
	r.mu.Unlock()
	return r.detail(), nil
}

// Functions implements branching.SchemaSource: the deploy step.
func (r *repoSource) Functions(ctx context.Context, run *branching.Run, ref string) error {
	ctx, done := r.linked(ctx)
	defer done()
	return step(run, branching.StepDeploy, func() error { return r.deploy(ctx, run, ref) })
}

// driver is the work of a commit on an existing branch (branching.Service.Drive).
func (r *repoSource) driver(ref string) branching.Driver {
	return func(ctx context.Context, run *branching.Run) (string, error) {
		ctx, done := r.linked(ctx)
		defer done()
		if err := step(run, branching.StepClone, func() error { return r.clone(run) }); err != nil {
			return "", err
		}
		run.Step(branching.StepPull).Skip()
		run.Step(branching.StepHealth).Finish()
		if _, err := r.apply(ctx, run, ref); err != nil {
			return "", err
		}
		if err := step(run, branching.StepDeploy, func() error { return r.deploy(ctx, run, ref) }); err != nil {
			return "", err
		}
		return r.detail(), nil
	}
}

func (r *repoSource) detail() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	parts := []string{fmt.Sprintf("built from %s@%s", r.conn.RepositoryName, short(r.sn.SHA))}
	if !r.fresh {
		parts[0] = fmt.Sprintf("brought up to %s@%s", r.conn.RepositoryName, short(r.sn.SHA))
	}
	parts = append(parts, fmt.Sprintf("%d migration(s) applied", r.stats.migrations))
	if r.stats.seeded {
		parts = append(parts, "seed applied")
	}
	if r.stats.functions > 0 {
		parts = append(parts, fmt.Sprintf("%d function(s) deployed", r.stats.functions))
	}
	return strings.Join(parts, ", ")
}

func (r *repoSource) clone(run *branching.Run) error {
	run.Logf("read %s of %s at %s: %d file(s)", r.sn.Dir, r.conn.RepositoryName, short(r.sn.SHA), len(r.sn.entries))
	return nil
}

// ---- configure ----------------------------------------------------------------------------

func (r *repoSource) configure(ctx context.Context, run *branching.Run, ref string) error {
	if !r.sn.Has("config.toml") {
		r.setPlan(&ConfigPlan{Patches: map[projectconfig.Service]map[string]any{}, Functions: map[string]FunctionConfig{}})
		run.Log("no config.toml: nothing to configure")
		return errSkip
	}
	raw, err := r.sn.Read(ctx, "config.toml", maxConfigSize)
	if err != nil {
		return err
	}
	doc, err := ParseConfig(raw)
	if err != nil {
		return fmt.Errorf("config.toml is not valid TOML: %w", err)
	}
	in := PlanInput{Doc: doc, Ref: ref, Persistent: r.persistent, Branch: ref != r.conn.ProjectRef}
	if h := r.s.host; h != nil {
		in.Env = func(name string) (string, bool, error) { return h.FunctionSecret(ctx, ref, name) }
		in.Held = func(svc projectconfig.Service, key string) (bool, error) { return h.SettingSaved(ctx, ref, svc, key) }
	}
	in.Template = func(rel string) (string, bool) {
		if !r.sn.Has(rel) {
			return "", false
		}
		b, err := r.sn.Read(ctx, rel, maxFileBytes)
		return string(b), err == nil
	}
	plan, err := PlanConfig(in)
	if err != nil {
		return err
	}
	r.setPlan(plan)
	for _, w := range plan.Warnings {
		run.Logf("warning: %s", w)
	}
	if plan.Skipped != "" {
		run.Logf("%s", plan.Skipped)
		return errSkip
	}
	h := r.s.host
	for _, svc := range []projectconfig.Service{projectconfig.Auth, projectconfig.PostgREST, projectconfig.Storage, projectconfig.Postgres} {
		patch := plan.Patches[svc]
		if len(patch) == 0 {
			continue
		}
		if svc == projectconfig.PostgREST {
			// PostgREST does not start on a schema that does not exist, and the migrations of this commit
			// may make the schema [api] schemas names: its settings are saved when they have run
			// (exposeSchemas), in one restart.
			run.Logf("%s: %d setting(s) in config.toml, saved after the migrations", svc, len(patch))
			continue
		}
		if h == nil {
			run.Logf("warning: this node cannot save %s settings from config.toml", svc)
			continue
		}
		changed, err := h.SaveSettings(ctx, ref, svc, patch)
		if err != nil {
			return fmt.Errorf("save the %s settings of config.toml: %w", svc, err)
		}
		run.Logf("%s: %d setting(s) in config.toml, %s", svc, len(patch), changedWords(changed))
		r.mu.Lock()
		r.stats.configured = true
		r.mu.Unlock()
	}
	if len(plan.Applied) == 0 {
		run.Log("config.toml sets nothing that applies to a project")
	}
	return nil
}

func changedWords(changed []string) string {
	if len(changed) == 0 {
		return "none changed"
	}
	sort.Strings(changed)
	return "changed " + strings.Join(changed, ", ")
}

func (r *repoSource) setPlan(p *ConfigPlan) {
	r.mu.Lock()
	r.plan = p
	r.mu.Unlock()
}

func (r *repoSource) currentPlan() *ConfigPlan {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.plan == nil {
		return &ConfigPlan{Functions: map[string]FunctionConfig{}}
	}
	return r.plan
}

// ---- migrate ------------------------------------------------------------------------------

func (r *repoSource) migrate(ctx context.Context, run *branching.Run, ref string) error {
	db := r.s.br.Database()
	repoMigs, skipped, err := r.sn.Migrations(ctx)
	if err != nil {
		return err
	}
	for _, s := range skipped {
		run.Logf("warning: migrations/%s is not <version>_<name>.sql; it is not applied", s)
	}
	have, err := db.Migrations(ctx, ref)
	if err != nil {
		return fmt.Errorf("read the migration history of the branch: %w", err)
	}
	plan := branching.Compare(repoMigs, have)
	run.Logf("%d migration(s) in the repository, %d already applied, %d to apply", len(repoMigs), len(repoMigs)-len(plan.Apply)-len(plan.Conflicts), len(plan.Apply))
	for _, c := range plan.Conflicts {
		run.Logf("warning: migration %s changed after it was applied; the applied version stays (add a new migration to change the schema)", c.Version)
	}
	if len(plan.TargetOnly) > 0 {
		var v []string
		for _, m := range plan.TargetOnly {
			v = append(v, m.Version)
		}
		run.Logf("warning: the branch has migration(s) the repository does not: %s", strings.Join(v, ", "))
	}
	if len(plan.OutOfOrder) > 0 {
		run.Logf("warning: applying migration(s) older than the newest applied one: %s", strings.Join(plan.OutOfOrder, ", "))
	}
	for _, m := range plan.Apply {
		if err := ctx.Err(); err != nil {
			return err
		}
		// One migration, one transaction: a failure leaves the migrations before it applied.
		if _, err := db.Apply(ctx, ref, []branching.Migration{m}, branching.ApplyOptions{Lock: true}); err != nil {
			return fmt.Errorf("migration %s_%s: %w", m.Version, m.Name, err)
		}
		run.Logf("applied %s_%s", m.Version, m.Name)
		r.mu.Lock()
		r.stats.migrations++
		r.mu.Unlock()
	}
	return r.exposeSchemas(ctx, run, ref)
}

// exposeSchemas saves the PostgREST settings of config.toml, which configure held back: [api] schemas
// names schemas that a migration of the same commit may have made, and PostgREST does not start on
// one that is missing. The node refuses a list that still names a missing schema, with the name, in
// seconds and not after a restart that times out (Host.SaveSettings).
func (r *repoSource) exposeSchemas(ctx context.Context, run *branching.Run, ref string) error {
	plan := r.currentPlan()
	patch := plan.Patches[projectconfig.PostgREST]
	if plan.Skipped != "" || len(patch) == 0 {
		return nil
	}
	h := r.s.host
	if h == nil {
		run.Logf("warning: this node cannot save %s settings from config.toml", projectconfig.PostgREST)
		return nil
	}
	changed, err := h.SaveSettings(ctx, ref, projectconfig.PostgREST, patch)
	if err != nil {
		return fmt.Errorf("save the %s settings of config.toml, after the migrations: %w", projectconfig.PostgREST, err)
	}
	run.Logf("%s: %d setting(s) in config.toml, %s", projectconfig.PostgREST, len(patch), changedWords(changed))
	r.mu.Lock()
	r.stats.configured = true
	r.mu.Unlock()
	return nil
}

// ---- seed ---------------------------------------------------------------------------------

func (r *repoSource) seed(ctx context.Context, run *branching.Run, ref string) error {
	if !r.seedDue {
		return errSkip
	}
	seed := r.currentPlan().Seed
	if r.persistent && (seed.Enabled == nil || !*seed.Enabled) {
		run.Log("a persistent branch is seeded only when [db.seed] enabled = true")
		return errSkip
	}
	if seed.Enabled != nil && !*seed.Enabled {
		run.Log("[db.seed] is disabled")
		return errSkip
	}
	patterns := seed.SQLPaths
	if len(patterns) == 0 {
		patterns = []string{"./seed.sql"}
	}
	var files []string
	seen := map[string]bool{}
	for _, pat := range patterns {
		matches, err := r.sn.Match(pat)
		if err != nil {
			return err
		}
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				files = append(files, m)
			}
		}
	}
	if len(files) == 0 {
		run.Log("no seed file")
		return errSkip
	}
	db := r.s.br.Database()
	for _, f := range files {
		body, err := r.sn.Read(ctx, f, maxMigrationBytes)
		if err != nil {
			return err
		}
		if err := db.RunScript(ctx, ref, strings.TrimPrefix(string(body), "\ufeff")); err != nil {
			return fmt.Errorf("seed %s: %w", f, err)
		}
		run.Logf("applied seed %s", f)
	}
	r.mu.Lock()
	r.stats.seeded = true
	r.mu.Unlock()
	return nil
}

// ---- deploy -------------------------------------------------------------------------------

func (r *repoSource) deploy(ctx context.Context, run *branching.Run, ref string) error {
	plan := r.currentPlan()
	fns, notes, err := r.sn.Functions(ctx, plan.Functions, r.have)
	if err != nil {
		return err
	}
	for _, n := range notes {
		run.Logf("warning: %s", n)
	}
	r.mu.Lock()
	if r.hashes == nil {
		r.hashes = map[string]string{}
	}
	r.mu.Unlock()
	for _, f := range fns {
		if len(f.Files) == 0 {
			run.Logf("function %s is unchanged", f.Slug)
			r.mu.Lock()
			r.hashes[f.Slug] = f.Hash
			r.mu.Unlock()
			continue
		}
		if r.s.host == nil {
			run.Logf("warning: this node cannot deploy function %s", f.Slug)
			continue
		}
		if err := r.s.host.DeployFunction(ctx, ref, f.FunctionDeploy); err != nil {
			return fmt.Errorf("deploy function %s: %w", f.Slug, err)
		}
		run.Logf("deployed function %s (%d file(s))", f.Slug, len(f.Files))
		r.mu.Lock()
		r.hashes[f.Slug] = f.Hash
		r.stats.functions++
		r.mu.Unlock()
	}
	if len(fns) == 0 {
		run.Log("no Edge Function in the repository")
	}
	if len(plan.Buckets) > 0 {
		if err := r.buckets(ctx, ref, plan.Buckets); err != nil {
			return fmt.Errorf("create the Storage buckets of config.toml: %w", err)
		}
		run.Logf("storage buckets: %s", bucketNames(plan.Buckets))
	}
	return nil
}

// buckets creates the buckets once the Storage service has made its schema, which a new branch
// may not have finished.
func (r *repoSource) buckets(ctx context.Context, ref string, bs []BucketSpec) error {
	script := `do $$ begin if to_regclass('storage.buckets') is null then raise exception 'the Storage schema is not ready'; end if; end $$;` + "\n" + bucketsSQL(bs)
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		if err = r.s.br.Database().RunScript(ctx, ref, script); err == nil || !strings.Contains(err.Error(), "Storage schema is not ready") {
			return err
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func bucketNames(bs []BucketSpec) string {
	var n []string
	for _, b := range bs {
		n = append(n, b.Name)
	}
	return strings.Join(n, ", ")
}

// bucketsSQL creates or updates the buckets of config.toml. Objects are not touched.
func bucketsSQL(bs []BucketSpec) string {
	var b strings.Builder
	for _, bk := range bs {
		limit := "null"
		if bk.FileSizeLimit > 0 {
			limit = fmt.Sprintf("%d", bk.FileSizeLimit)
		}
		mimes := "null"
		if len(bk.AllowedMIMETypes) > 0 {
			q := make([]string, len(bk.AllowedMIMETypes))
			for i, m := range bk.AllowedMIMETypes {
				q[i] = sqlString(m)
			}
			mimes = "array[" + strings.Join(q, ", ") + "]::text[]"
		}
		fmt.Fprintf(&b, "insert into storage.buckets (id, name, public, file_size_limit, allowed_mime_types) values (%s, %s, %t, %s, %s) "+
			"on conflict (id) do update set public = excluded.public, file_size_limit = excluded.file_size_limit, allowed_mime_types = excluded.allowed_mime_types;\n",
			sqlString(bk.Name), sqlString(bk.Name), bk.Public, limit, mimes)
	}
	return b.String()
}

// sqlString quotes a string literal for a session with standard_conforming_strings on, which is
// every Postgres this node runs.
func sqlString(s string) string {
	return "'" + strings.NewReplacer("'", "''", "\x00", "").Replace(s) + "'"
}
