package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/registry"
)

// prEvent is a pull request event, reduced to what the runner needs.
type prEvent struct {
	Action  string
	Number  int
	HeadRef string
	HeadSHA string
	BaseRef string
	Title   string
	Merged  bool
}

// branchNameRE keeps the characters a Supavise branch name allows.
var (
	badNameChars = regexp.MustCompile(`[^A-Za-z0-9._/-]+`)
	dotRuns      = regexp.MustCompile(`\.{2,}`)
)

// branchName derives a Supavise branch name from a Git branch: the same name when it is valid, else
// the nearest valid one, else pr-<number>.
func branchName(headRef string, pr int) string {
	n := badNameChars.ReplaceAllString(headRef, "-")
	n = dotRuns.ReplaceAllString(n, ".")
	n = strings.TrimLeft(n, "-._/")
	n = strings.TrimRight(n, "/.")
	if len(n) > 100 {
		n = strings.TrimRight(n[:100], "/.-")
	}
	if branching.ValidName(n) != nil {
		return fmt.Sprintf("pr-%d", pr)
	}
	return n
}

// supabaseDir is the directory whose changes matter, with the trailing slash.
func supabaseDir(workdir string) string { return path.Join(workdir, "supabase") + "/" }

// touchesSupabase reports whether the pull request changes a file under the supabase directory.
func (s *Service) touchesSupabase(ctx context.Context, rep *reporter, conn registry.GitHubConnection, pr int) (bool, error) {
	dir := supabaseDir(conn.Workdir)
	found := false
	stop := errors.New("found")
	err := s.cl.getAll(ctx, rep.token, fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100", escapeSegments(conn.RepositoryName), pr), "", 30, func(raw json.RawMessage) error {
		var page []struct {
			Filename string `json:"filename"`
			Previous string `json:"previous_filename"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, f := range page {
			if strings.HasPrefix(f.Filename, dir) || strings.HasPrefix(f.Previous, dir) {
				found = true
				return stop
			}
		}
		return nil
	})
	if errors.Is(err, stop) {
		return true, nil
	}
	return found, err
}

// runPR handles one pull request event for one connection. It runs on the queue of the pull
// request, so events of one pull request never overlap, and a newer one cancels an older.
func (s *Service) runPR(ctx context.Context, conn registry.GitHubConnection, ev prEvent) {
	log := s.log.With("repo", conn.RepositoryName, "pr", ev.Number, "project", conn.ProjectRef, "action", ev.Action)
	var err error
	if ev.Action == "closed" {
		err = s.closePR(ctx, conn, ev)
	} else {
		release, ok := s.acquireBuild(ctx)
		if !ok {
			return // a newer commit replaced this one while it waited for its turn
		}
		defer release()
		err = s.syncPR(ctx, conn, ev)
	}
	if err != nil && ctx.Err() == nil {
		log.Warn("a pull request could not be handled", "err", err)
	}
}

// acquireBuild waits for one of the node's build slots ([github] max_concurrent_builds). It
// reports false when the job ended first. Each build reads a repository and makes or migrates a
// Postgres cluster, so a rebase that updates forty pull requests must not start forty at once.
func (s *Service) acquireBuild(ctx context.Context) (release func(), ok bool) {
	select {
	case s.builds <- struct{}{}:
		return func() { <-s.builds }, true
	case <-ctx.Done():
		return nil, false
	}
}

// findLink returns the link of the pull request, or the one of its Git branch that no pull request
// has claimed yet (a branch made by hand with git_branch), or nil.
func (s *Service) findLink(ctx context.Context, conn registry.GitHubConnection, ev prEvent) (*registry.GitHubBranchLink, error) {
	l, err := s.st.FindGitHubBranchLink(ctx, conn.ProjectRef, conn.RepositoryID, ev.HeadRef, ev.Number)
	if err == nil {
		return l, nil
	}
	if !errors.Is(err, registry.ErrNotFound) {
		return nil, err
	}
	l, err = s.st.FindGitHubBranchLink(ctx, conn.ProjectRef, conn.RepositoryID, ev.HeadRef, 0)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if l.PRNumber != 0 && l.PRNumber != ev.Number {
		return nil, nil // another pull request from the same Git branch has it
	}
	return l, nil
}

// manualBranch finds a branch somebody made by hand for the Git branch.
func (s *Service) manualBranch(ctx context.Context, conn registry.GitHubConnection, gitBranch string) (*branching.Branch, error) {
	list, err := s.br.List(ctx, conn.ProjectRef)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if !list[i].IsDefault && list[i].GitBranch == gitBranch {
			if _, lerr := s.st.GetGitHubBranchLink(ctx, list[i].Ref); lerr == nil {
				continue // linked to a pull request already
			}
			return &list[i], nil
		}
	}
	return nil, nil
}

func (s *Service) linkFor(conn registry.GitHubConnection, ev prEvent, b *branching.Branch, checkID int64) *registry.GitHubBranchLink {
	return &registry.GitHubBranchLink{BranchRef: b.Ref, ProjectRef: conn.ProjectRef, RepositoryID: conn.RepositoryID,
		RepositoryName: conn.RepositoryName, InstallationID: conn.InstallationID, PRNumber: ev.Number, HeadRef: ev.HeadRef,
		HeadSHA: ev.HeadSHA, CheckRunID: checkID, Functions: map[string]string{}}
}

// markSeeded records that a branch somebody else made is not ours to seed: it has its own data.
func (s *Service) markSeeded(link *registry.GitHubBranchLink) {
	if link.SeededAt == nil {
		t := s.now().UTC()
		link.SeededAt = &t
	}
}

// syncPR creates the preview branch of a pull request, or brings it up to the head commit.
func (s *Service) syncPR(ctx context.Context, conn registry.GitHubConnection, ev prEvent) error {
	if s.br == nil {
		return errors.New("this node has no branching service")
	}
	rep, err := s.reporterFor(ctx, conn.InstallationID, conn.RepositoryName, conn.ProjectRef)
	if err != nil {
		return err
	}
	link, err := s.findLink(ctx, conn, ev)
	if err != nil {
		return err
	}
	var manual *branching.Branch
	if link == nil {
		if manual, err = s.manualBranch(ctx, conn, ev.HeadRef); err != nil {
			return err
		}
		if manual == nil {
			if !conn.NewBranchPerPR {
				s.log.Debug("a pull request is ignored: automatic branching is off", "repo", conn.RepositoryName, "pr", ev.Number)
				return nil
			}
			if conn.SupabaseChangesOnly {
				touched, err := s.touchesSupabase(ctx, rep, conn, ev.Number)
				if err != nil {
					return err
				}
				if !touched {
					s.log.Debug("a pull request is ignored: it changes nothing under supabase/", "repo", conn.RepositoryName, "pr", ev.Number)
					return nil
				}
			}
		}
	}
	name := CheckName
	details := s.cfg.DashboardURL() + "/project/" + conn.ProjectRef + "/branches"
	start := checkOutput{Title: "Deploying the preview branch", Summary: "Supavise is building the preview branch of this pull request."}
	checkID, err := rep.start(ctx, name, ev.HeadSHA, details, start)
	if err != nil {
		// A failed check run does not stop the branch; the comment and the dashboard still report.
		s.log.Warn("could not open the check run", "repo", conn.RepositoryName, "err", err)
		checkID = 0
	}
	oc := s.buildPR(ctx, conn, ev, rep, link, manual, checkID)
	return s.finishPR(ctx, conn, ev, rep, checkID, oc)
}

// buildPR does the work and describes the outcome.
func (s *Service) buildPR(ctx context.Context, conn registry.GitHubConnection, ev prEvent, rep *reporter, link *registry.GitHubBranchLink, manual *branching.Branch, checkID int64) outcome {
	oc := outcome{Parent: conn.ProjectRef}
	sn, err := s.loadSnapshot(ctx, conn.RepositoryName, conn.InstallationID, conn.Workdir, ev.HeadSHA)
	switch {
	case errors.Is(err, ErrNoSupabaseDir):
		oc.Neutral = true
		oc.Headline = "There is no supabase directory in this commit, so there is nothing to deploy."
		oc.Detail = fmt.Sprintf("Supavise looks for %s (the working directory of the connection is %q).", strings.TrimSuffix(supabaseDir(conn.Workdir), "/"), conn.Workdir)
		return oc
	case err != nil:
		oc.Headline = "Supavise could not read the repository."
		oc.Detail = err.Error()
		return oc
	}
	var b *branching.Branch
	var src *repoSource
	switch {
	case link != nil:
		if b, err = s.br.Resolve(ctx, link.BranchRef); err != nil {
			if errors.Is(err, branching.ErrNotFound) {
				// The branch was deleted by hand: this commit builds a new one.
				_ = s.st.DeleteGitHubBranchLink(ctx, link.BranchRef)
				link = nil
			} else {
				oc.Headline, oc.Detail = "Supavise could not find the preview branch.", err.Error()
				return oc
			}
		}
	case manual != nil:
		b = manual
	}
	if link == nil && b == nil {
		return s.createFromPR(ctx, conn, ev, sn, checkID)
	}
	if link == nil {
		// A branch that somebody made before the pull request has its own data: it is not seeded.
		link = s.linkFor(conn, ev, b, checkID)
		s.markSeeded(link)
	}
	link.PRNumber, link.HeadRef, link.HeadSHA, link.CheckRunID = ev.Number, ev.HeadRef, ev.HeadSHA, checkID
	src = &repoSource{s: s, conn: conn, sn: sn, job: ctx, persistent: b.Persistent, have: link.Functions, seedDue: link.SeededAt == nil}
	return s.driveBranch(ctx, conn, b, link, src, checkID)
}

// createFromPR makes the branch of a pull request that has none.
//
// The branch is made, and its link to the pull request stored, under a context that a newer event
// does not cancel: Create returns once the branch's row is visible and the rest of the create goes on
// without the caller, so a caller that gave up at that moment would leave a branch that nothing
// points to (no link: the next event would take it for a branch somebody made by hand and never seed
// it, and a close event would not delete it). The create itself still ends when the job does (the
// repoSource is linked to it); the link says the seed is due, and the next event for the pull request
// finds the branch through it and finishes the work. Waiting for the result stays with the job.
func (s *Service) createFromPR(ctx context.Context, conn registry.GitHubConnection, ev prEvent, sn *Snapshot, checkID int64) outcome {
	oc := outcome{Parent: conn.ProjectRef}
	if err := ctx.Err(); err != nil {
		// A newer event replaced this one before it began: it makes the branch itself.
		oc.Headline, oc.Detail = "Supavise stopped before it created the preview branch.", err.Error()
		return oc
	}
	if links, err := s.st.ListGitHubBranchLinks(ctx, conn.ProjectRef); err == nil && len(links) >= conn.BranchLimit {
		oc.Neutral = true
		oc.Headline = fmt.Sprintf("No preview branch was made: the project already has %d, the branch limit of its GitHub connection.", len(links))
		oc.Detail = "Close a pull request or raise the limit in Settings > Integrations > GitHub."
		return oc
	}
	src := &repoSource{s: s, conn: conn, sn: sn, job: ctx, fresh: true, seedDue: true, have: map[string]string{}}
	name := branchName(ev.HeadRef, ev.Number)
	in := branching.CreateInput{Name: name, GitBranch: ev.HeadRef, TTL: -1, SchemaSource: src}
	keep := context.WithoutCancel(ctx)
	b, err := s.br.Create(keep, conn.ProjectRef, in)
	if errors.Is(err, branching.ErrConflict) && strings.Contains(err.Error(), "already has a branch named") {
		// The name is taken: by this pull request's own branch after a cancelled create, or by another.
		if exist := s.sameBranch(ctx, conn, name, ev.HeadRef); exist != nil {
			link := s.linkFor(conn, ev, exist, checkID)
			if perr := s.st.PutGitHubBranchLink(keep, link); perr != nil {
				oc.Headline, oc.Detail = "Supavise could not record the preview branch.", perr.Error()
				return oc
			}
			// The create was cut short: whatever it did not reach, this run does, the seed included.
			src.fresh = false
			src.have = map[string]string{}
			return s.driveBranch(ctx, conn, exist, link, src, checkID)
		}
		in.Name = fmt.Sprintf("%s-%d", name, ev.Number)
		if len(in.Name) > 100 {
			in.Name = fmt.Sprintf("pr-%d", ev.Number)
		}
		b, err = s.br.Create(keep, conn.ProjectRef, in)
	}
	if err != nil {
		oc.Headline = "Supavise could not create the preview branch."
		oc.Detail = branchErrText(err)
		oc.Neutral = errors.Is(err, branching.ErrLimit)
		return oc
	}
	link := s.linkFor(conn, ev, b, checkID)
	if err := s.st.PutGitHubBranchLink(keep, link); err != nil {
		oc.Headline, oc.Detail = "Supavise could not record the preview branch.", err.Error()
		return oc
	}
	s.auditEvent(ctx, "github.branch.created", map[string]any{"project": conn.ProjectRef, "branch": b.Ref, "pr": ev.Number, "repository": conn.RepositoryName})
	s.setRunCheck(ctx, b.RunID, checkID)
	return s.await(ctx, conn, b.Ref, b.RunID, link, src)
}

// sameBranch returns the existing branch called name when it follows this Git branch.
func (s *Service) sameBranch(ctx context.Context, conn registry.GitHubConnection, name, gitBranch string) *branching.Branch {
	list, err := s.br.List(ctx, conn.ProjectRef)
	if err != nil {
		return nil
	}
	for i := range list {
		if !list[i].IsDefault && list[i].Name == name && list[i].GitBranch == gitBranch {
			return &list[i]
		}
	}
	return nil
}

func branchErrText(err error) string {
	msg := err.Error()
	for _, e := range []error{branching.ErrInvalid, branching.ErrConflict, branching.ErrLimit} {
		if p := e.Error() + ": "; strings.HasPrefix(msg, p) {
			return msg[len(p):]
		}
	}
	return msg
}

// driveBranch brings an existing branch up to the commit.
func (s *Service) driveBranch(ctx context.Context, conn registry.GitHubConnection, b *branching.Branch, link *registry.GitHubBranchLink, src *repoSource, checkID int64) outcome {
	oc := outcome{Parent: conn.ProjectRef, Branch: b.Name, BranchRef: b.Ref, Private: b.WithData || b.Persistent}
	// A previous commit's run may still be winding down after it was cancelled.
	if _, err := s.br.WaitIdle(ctx, b.Ref); err != nil {
		oc.Headline, oc.Detail = "Supavise could not wait for the branch.", err.Error()
		return oc
	}
	// A preview branch that the node paused while the pull request sat idle runs again for this commit. One
	// its owner paused, one stopped as a quarantine and one scheduled for deletion do not: anyone who can push
	// can make a commit, and starting such a branch is not their decision. The check says why.
	if _, err := s.br.WakeForDrive(ctx, b.Ref); err != nil {
		oc.Headline = "Supavise could not wake the paused preview branch."
		oc.Detail = branchErrText(err)
		return oc
	}
	if err := s.st.PutGitHubBranchLink(ctx, link); err != nil {
		oc.Headline, oc.Detail = "Supavise could not record the preview branch.", err.Error()
		return oc
	}
	runID, err := s.br.Drive(ctx, b.Ref, "push", src.driver(b.Ref))
	if err != nil {
		oc.Headline = "Supavise could not start the deployment."
		oc.Detail = branchErrText(err)
		return oc
	}
	s.setRunCheck(ctx, runID, checkID)
	return s.await(ctx, conn, b.Ref, runID, link, src)
}

func (s *Service) setRunCheck(ctx context.Context, runID string, checkID int64) {
	if runID == "" || checkID == 0 {
		return
	}
	if st := registry.BranchRunsOf(s.reg); st != nil {
		if err := st.SetBranchRunCheck(context.WithoutCancel(ctx), runID, checkID); err != nil {
			s.log.Warn("could not record the check run of an action run", "run", runID, "err", err)
		}
	}
}

// await waits for the branch's operation to end and describes how it went. The function hashes
// the run deployed are kept on the link for the next commit.
func (s *Service) await(ctx context.Context, conn registry.GitHubConnection, ref, runID string, link *registry.GitHubBranchLink, src *repoSource) outcome {
	oc := outcome{Parent: conn.ProjectRef, BranchRef: ref}
	// Recording what the run did survives a newer commit cancelling this one.
	rctx := context.WithoutCancel(ctx)
	defer s.recordRun(rctx, link, src)
	b, err := s.br.WaitIdle(ctx, ref)
	if err != nil {
		oc.Headline = "Supavise stopped waiting for the branch."
		oc.Detail = err.Error()
		return oc
	}
	oc.Branch = b.Name
	oc.Private = b.WithData || b.Persistent
	oc.secrets = src.knownSecrets()
	if runID != "" {
		if run, rerr := s.br.GetRun(rctx, ref, runID); rerr == nil {
			oc.Run = run
		}
	}
	oc.Links = s.dashboardLinks(conn.ProjectRef, ref)
	if b.State == registry.BranchFunctionsDeployed {
		oc.Success = true
		oc.Headline = fmt.Sprintf("The preview branch `%s` is ready.", b.Name)
		oc.Detail = b.Detail
		return oc
	}
	oc.Headline = fmt.Sprintf("The preview branch `%s` could not be deployed (%s).", b.Name, b.State)
	oc.Detail = b.Detail
	return oc
}

// recordRun keeps on the link what the run did: the hashes of the functions it deployed, and that
// it got past the seed step, so that a branch whose first run failed in a migration, or was
// cancelled by a newer commit, is seeded by the next one.
func (s *Service) recordRun(ctx context.Context, link *registry.GitHubBranchLink, src *repoSource) {
	if src == nil || link == nil {
		return
	}
	src.mu.Lock()
	if len(src.hashes) > 0 {
		if link.Functions == nil {
			link.Functions = map[string]string{}
		}
		for k, v := range src.hashes {
			link.Functions[k] = v
		}
	}
	seeded := src.stats.seedDone
	src.mu.Unlock()
	if seeded {
		s.markSeeded(link)
	}
	_ = s.st.PutGitHubBranchLink(ctx, link)
}

// finishPR writes the end of the check run and the pull request's comment.
func (s *Service) finishPR(ctx context.Context, conn registry.GitHubConnection, ev prEvent, rep *reporter, checkID int64, oc outcome) error {
	// A newer commit cancels this one: it writes its own report, and this one is not worth a
	// "cancelled" note on a commit nobody looks at any more.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Everyone who can read the repository reads the check run and the comment: they get the public
	// form of the outcome, and the run's log stays in the dashboard.
	oc, text := oc.public()
	out := checkOutput{Title: oc.Headline, Summary: oc.summary(), Text: text}
	if len(out.Title) > 120 {
		out.Title = out.Title[:117] + "..."
	}
	details := s.cfg.DashboardURL() + "/project/" + conn.ProjectRef + "/branches"
	if oc.BranchRef != "" {
		details = s.cfg.DashboardURL() + "/project/" + oc.BranchRef
	}
	var first error
	if checkID != 0 {
		if err := rep.finish(ctx, checkID, oc.conclusion(), details, out); err != nil {
			first = err
		}
	}
	// The comment follows the branch: a pull request that made none has nothing to comment on.
	if ev.Number > 0 {
		var link *registry.GitHubBranchLink
		if oc.BranchRef != "" {
			link, _ = s.st.GetGitHubBranchLink(ctx, oc.BranchRef)
		}
		var id int64
		if link != nil {
			id = link.CommentID
		}
		nid, err := rep.comment(ctx, ev.Number, id, "## Supabase Preview\n\n"+oc.summary())
		if err != nil && first == nil {
			first = err
		}
		if link != nil && nid != 0 && nid != id {
			link.CommentID = nid
			_ = s.st.PutGitHubBranchLink(ctx, link)
		}
	}
	return first
}

// closePR deletes the preview branch of a pull request that was closed or merged. A persistent
// branch is kept: only its link to the pull request goes.
func (s *Service) closePR(ctx context.Context, conn registry.GitHubConnection, ev prEvent) error {
	link, err := s.st.FindGitHubBranchLink(ctx, conn.ProjectRef, conn.RepositoryID, ev.HeadRef, ev.Number)
	if errors.Is(err, registry.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if s.br == nil {
		return errors.New("this node has no branching service")
	}
	b, err := s.br.Resolve(ctx, link.BranchRef)
	if errors.Is(err, branching.ErrNotFound) {
		return s.st.DeleteGitHubBranchLink(ctx, link.BranchRef)
	}
	if err != nil {
		return err
	}
	if b.Persistent {
		link.PRNumber = 0
		return s.st.PutGitHubBranchLink(ctx, link)
	}
	if _, err := s.br.WaitIdle(ctx, b.Ref); err != nil {
		return err
	}
	if _, err := s.br.Delete(context.WithoutCancel(ctx), b.Ref, branching.DeleteOptions{}); err != nil && !errors.Is(err, branching.ErrNotFound) {
		return err
	}
	_ = s.st.DeleteGitHubBranchLink(ctx, link.BranchRef)
	s.auditEvent(ctx, "github.branch.deleted", map[string]any{"project": conn.ProjectRef, "branch": b.Ref, "pr": ev.Number, "repository": conn.RepositoryName, "merged": ev.Merged})
	return nil
}
