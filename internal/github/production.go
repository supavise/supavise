package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/registry"
)

// Production and Git branches. "Deploy to production" is the Production branch field of the
// connection form: the default branch's git_branch (registry.BranchRunStore.SetDefaultBranchGit).
// A push to that Git branch applies the repository's migrations and Edge Functions to the
// project itself; config.toml reaches production only through a [remotes] block whose project_id
// is the project's ref. A push to any other Git branch deploys to the branches that follow it.

// productionBranch is the Git branch the project's production syncs with, "" when Deploy to
// production is off.
func (s *Service) productionBranch(ctx context.Context, projectRef string) (string, error) {
	st := registry.BranchRunsOf(s.reg)
	if st == nil {
		return "", nil
	}
	return st.GetDefaultBranchGit(ctx, projectRef)
}

// ProductionBranch reports the Git branch production follows, "" when there is none or the
// project has no connection.
func (s *Service) ProductionBranch(ctx context.Context, projectRef string) string {
	if _, err := s.st.GetGitHubConnectionByProject(ctx, projectRef); err != nil {
		return ""
	}
	b, _ := s.productionBranch(ctx, projectRef)
	return b
}

// DeployProduction deploys the head of the production branch now (Resync on the production row of
// the Branches page). It returns the id of the action run, which the work continues under.
func (s *Service) DeployProduction(ctx context.Context, projectRef string) (string, error) {
	if s.br == nil {
		return "", errors.New("github: this node has no branching service")
	}
	conn, err := s.st.GetGitHubConnectionByProject(ctx, projectRef)
	if errors.Is(err, registry.ErrNotFound) {
		return "", fmt.Errorf("%w: this project has no GitHub connection", ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	branch, err := s.productionBranch(ctx, projectRef)
	if err != nil {
		return "", err
	}
	if branch == "" {
		return "", invalid("Deploy to production is off: no production branch is set for this project")
	}
	tok, err := s.installationToken(ctx, conn.InstallationID)
	if err != nil {
		return "", err
	}
	var c struct {
		SHA string `json:"sha"`
	}
	if err := s.cl.do(ctx, tok, http.MethodGet, "/repos/"+escapeSegments(conn.RepositoryName)+"/commits/"+url.PathEscape(branch), nil, &c); err != nil {
		if IsStatus(err, http.StatusNotFound) || IsStatus(err, http.StatusUnprocessableEntity) {
			return "", fmt.Errorf("%w: branch %q does not exist in %s", ErrNotFound, branch, conn.RepositoryName)
		}
		return "", err
	}
	run, err := s.br.StartRun(ctx, "deploy", projectRef, projectRef, projectRef)
	if err != nil {
		return "", err
	}
	s.enqueue("prod:"+projectRef, func(ctx context.Context) {
		if err := s.deployProduction(ctx, *conn, branch, c.SHA, run); err != nil && ctx.Err() == nil {
			s.log.Warn("a production deployment failed", "repo", conn.RepositoryName, "err", err)
		}
	}, func() {
		// A newer deployment replaced this one before it started (or the service is stopping): the run
		// made for it ends here and does not stay open.
		run.Finish(errors.New("replaced by a newer deployment before it started"))
	})
	return run.ID(), nil
}

// deployProduction applies the commit to the project. pre is a run the caller started, nil to
// start one.
func (s *Service) deployProduction(ctx context.Context, conn registry.GitHubConnection, branch, sha string, pre *branching.Run) error {
	if s.br == nil {
		return errors.New("this node has no branching service")
	}
	rep, err := s.reporterFor(ctx, conn.InstallationID, conn.RepositoryName, conn.ProjectRef)
	if err != nil {
		if pre != nil {
			pre.Finish(err)
		}
		return err
	}
	run := pre
	if run == nil {
		if run, err = s.br.StartRun(ctx, "deploy", conn.ProjectRef, conn.ProjectRef, conn.ProjectRef); err != nil {
			return err
		}
	}
	details := s.cfg.DashboardURL() + "/project/" + conn.ProjectRef + "/branches"
	checkID, cerr := rep.start(ctx, CheckNameProduction, sha, details, checkOutput{Title: "Deploying to production",
		Summary: fmt.Sprintf("Supavise is applying `%s` (%s) to the production database and Edge Functions.", branch, short(sha))})
	if cerr != nil {
		s.log.Warn("could not open the check run", "repo", conn.RepositoryName, "err", cerr)
		checkID = 0
	}
	s.setRunCheck(ctx, run.ID(), checkID)
	oc := outcome{Parent: conn.ProjectRef, BranchRef: conn.ProjectRef, Branch: "production"}
	oc.Links = s.dashboardLinks(conn.ProjectRef, "")
	src, nothing, err := s.productionSteps(ctx, conn, sha, run)
	failed := run.Finish(err)
	if rec, rerr := s.br.GetRun(context.WithoutCancel(ctx), conn.ProjectRef, run.ID()); rerr == nil {
		oc.Run = rec
	}
	switch {
	case nothing:
		oc.Neutral = true
		oc.Headline = "There is no supabase directory in this commit, so nothing was deployed to production."
	case err != nil:
		oc.Headline = fmt.Sprintf("The deployment to production failed in the %s step.", failed)
		oc.Detail = err.Error()
	default:
		oc.Success = true
		oc.Headline = fmt.Sprintf("Production is at `%s` (%s).", branch, short(sha))
		oc.Detail = src.detail()
		src.mu.Lock()
		if len(src.hashes) > 0 {
			merged := map[string]string{}
			for k, v := range conn.ProdFunctions {
				merged[k] = v
			}
			for k, v := range src.hashes {
				merged[k] = v
			}
			_ = s.st.SetGitHubProdFunctions(context.WithoutCancel(ctx), conn.ID, merged)
		}
		src.mu.Unlock()
	}
	s.auditEvent(ctx, "github.production.deployed", map[string]any{"project": conn.ProjectRef, "sha": sha, "ok": oc.Success})
	if checkID == 0 || ctx.Err() != nil {
		return err
	}
	// The check run is read by everyone who can read the repository, which may be public, and production's
	// errors can quote its data: the log stays in the dashboard, and the failure shown here is short.
	if len(oc.Detail) > 300 {
		oc.Detail = oc.Detail[:297] + "..."
	}
	if !oc.Success && !oc.Neutral {
		oc.Detail += "\n\nThe log of the run is in the dashboard (Branches > the production branch > Workflow logs)."
	}
	out := checkOutput{Title: oc.Headline, Summary: oc.summary()}
	if ferr := rep.finish(ctx, checkID, oc.conclusion(), details, out); ferr != nil {
		s.log.Warn("could not finish the check run", "repo", conn.RepositoryName, "err", ferr)
	}
	return err
}

// productionSteps runs the DAG against the project. nothing is true when the commit has no
// supabase directory.
func (s *Service) productionSteps(ctx context.Context, conn registry.GitHubConnection, sha string, run *branching.Run) (src *repoSource, nothing bool, err error) {
	p, err := s.reg.GetProject(ctx, conn.ProjectRef)
	if err != nil {
		return nil, false, err
	}
	if p.Status != registry.StatusActiveHealthy && p.Status != registry.StatusActiveUnhealthy {
		run.Step(branching.StepClone).Start()
		err := fmt.Errorf("the project is %s; it must be running to be deployed", p.Status)
		run.Step(branching.StepClone).Fail(err)
		return nil, false, err
	}
	var sn *Snapshot
	err = step(run, branching.StepClone, func() error {
		var lerr error
		sn, lerr = s.loadSnapshot(ctx, conn.RepositoryName, conn.InstallationID, conn.Workdir, sha)
		return lerr
	})
	if errors.Is(err, ErrNoSupabaseDir) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	run.Logf("read %s of %s at %s: %d file(s)", sn.Dir, conn.RepositoryName, short(sn.SHA), len(sn.entries))
	src = &repoSource{s: s, conn: conn, sn: sn, job: ctx, persistent: true, have: conn.ProdFunctions}
	run.Step(branching.StepPull).Skip()
	run.Step(branching.StepHealth).Finish()
	if _, err := src.apply(ctx, run, conn.ProjectRef); err != nil {
		return src, false, err
	}
	if err := step(run, branching.StepDeploy, func() error { return src.deploy(ctx, run, conn.ProjectRef) }); err != nil {
		return src, false, err
	}
	return src, false, nil
}

// syncGitBranch deploys a push to a Git branch to the branches that follow it and are not tied to
// a pull request (a pull request's own events carry its commits).
func (s *Service) syncGitBranch(ctx context.Context, conn registry.GitHubConnection, gitBranch, sha string) error {
	if s.br == nil {
		return errors.New("this node has no branching service")
	}
	list, err := s.br.List(ctx, conn.ProjectRef)
	if err != nil {
		return err
	}
	var first error
	for i := range list {
		b := list[i]
		if b.IsDefault || b.GitBranch != gitBranch {
			continue
		}
		link, lerr := s.st.GetGitHubBranchLink(ctx, b.Ref)
		if lerr != nil && !errors.Is(lerr, registry.ErrNotFound) {
			return lerr
		}
		if link != nil && link.PRNumber > 0 {
			continue
		}
		if err := s.syncOne(ctx, conn, &b, link, gitBranch, sha); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Service) syncOne(ctx context.Context, conn registry.GitHubConnection, b *branching.Branch, link *registry.GitHubBranchLink, gitBranch, sha string) error {
	rep, err := s.reporterFor(ctx, conn.InstallationID, conn.RepositoryName, conn.ProjectRef)
	if err != nil {
		return err
	}
	details := s.cfg.DashboardURL() + "/project/" + b.Ref
	checkID, cerr := rep.start(ctx, CheckName, sha, details, checkOutput{Title: "Deploying the branch", Summary: "Supavise is applying this commit to the branch `" + b.Name + "`."})
	if cerr != nil {
		s.log.Warn("could not open the check run", "repo", conn.RepositoryName, "err", cerr)
		checkID = 0
	}
	ev := prEvent{HeadRef: gitBranch, HeadSHA: sha}
	if link == nil {
		link = s.linkFor(conn, ev, b, checkID)
		s.markSeeded(link) // the branch was made by somebody else and has its own data
	}
	link.HeadRef, link.HeadSHA, link.CheckRunID = gitBranch, sha, checkID
	var oc outcome
	sn, err := s.loadSnapshot(ctx, conn.RepositoryName, conn.InstallationID, conn.Workdir, sha)
	switch {
	case errors.Is(err, ErrNoSupabaseDir):
		oc = outcome{Neutral: true, Parent: conn.ProjectRef, Headline: "There is no supabase directory in this commit, so there is nothing to deploy."}
	case err != nil:
		oc = outcome{Parent: conn.ProjectRef, Headline: "Supavise could not read the repository.", Detail: err.Error()}
	default:
		src := &repoSource{s: s, conn: conn, sn: sn, job: ctx, persistent: b.Persistent, have: link.Functions, seedDue: link.SeededAt == nil}
		oc = s.driveBranch(ctx, conn, b, link, src, checkID)
	}
	return s.finishPR(ctx, conn, ev, rep, checkID, oc)
}

// deleteGitBranch deletes the preview branches that followed a Git branch that no longer exists.
func (s *Service) deleteGitBranch(ctx context.Context, conn registry.GitHubConnection, gitBranch string) error {
	if s.br == nil {
		return errors.New("this node has no branching service")
	}
	list, err := s.br.List(ctx, conn.ProjectRef)
	if err != nil {
		return err
	}
	for i := range list {
		b := list[i]
		if b.IsDefault || b.Persistent || b.GitBranch != gitBranch {
			continue
		}
		if link, lerr := s.st.GetGitHubBranchLink(ctx, b.Ref); lerr == nil && link.PRNumber > 0 {
			continue // the pull request's close event deletes it
		}
		if _, err := s.br.WaitIdle(ctx, b.Ref); err != nil {
			return err
		}
		if _, err := s.br.Delete(context.WithoutCancel(ctx), b.Ref, branching.DeleteOptions{}); err != nil && !errors.Is(err, branching.ErrNotFound) {
			return err
		}
		_ = s.st.DeleteGitHubBranchLink(ctx, b.Ref)
		s.auditEvent(ctx, "github.branch.deleted", map[string]any{"project": conn.ProjectRef, "branch": b.Ref, "git_branch": gitBranch, "repository": conn.RepositoryName})
	}
	return nil
}
