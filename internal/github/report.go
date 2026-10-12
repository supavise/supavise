package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/supavise/supavise/internal/registry"
)

// The check run of a pull request is named like hosted's, which the Actions example in its
// documentation waits on.
const (
	CheckName           = "Supabase Preview"
	CheckNameProduction = "Supabase Production"
	// commentMarkerPrefix opens the first line of the pull request's comment. The line names the
	// project, so that two projects connected to one repository (a monorepo with two working
	// directories, or two servers) each keep their own comment.
	commentMarkerPrefix = "<!-- supavise-preview:"
	maxCheckText        = 60000
)

// reporter writes the check run and the comment of a repository.
type reporter struct {
	s       *Service
	repo    string
	project string // the project whose branches the reports are about
	token   string
	appID   int64 // the App whose comments these are; 0 when unknown
}

func (s *Service) reporterFor(ctx context.Context, installationID int64, repo, project string) (*reporter, error) {
	tok, err := s.installationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	r := &reporter{s: s, repo: repo, project: project, token: tok}
	if app, aerr := s.App(ctx); aerr == nil && app != nil {
		r.appID = app.ID
	}
	return r, nil
}

// marker is the first line of this project's comment.
func (r *reporter) marker() string { return commentMarkerPrefix + r.project + " -->" }

type checkOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Text    string `json:"text,omitempty"`
}

func cut(s string) string {
	if len(s) <= maxCheckText {
		return s
	}
	return s[:maxCheckText-40] + "\n\n... (shortened)"
}

// start opens a check run on the commit, in progress. detailsURL is where "Details" leads.
func (r *reporter) start(ctx context.Context, name, sha, detailsURL string, out checkOutput) (int64, error) {
	body := map[string]any{"name": name, "head_sha": sha, "status": "in_progress", "started_at": r.s.now().UTC().Format("2006-01-02T15:04:05Z"),
		"output": checkOutput{Title: out.Title, Summary: cut(out.Summary), Text: cut(out.Text)}}
	if detailsURL != "" {
		body["details_url"] = detailsURL
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := r.s.cl.do(ctx, r.token, http.MethodPost, "/repos/"+escapeSegments(r.repo)+"/check-runs", body, &res); err != nil {
		return 0, err
	}
	return res.ID, nil
}

// finish completes the check run with a conclusion (success, failure, neutral, cancelled).
func (r *reporter) finish(ctx context.Context, id int64, conclusion, detailsURL string, out checkOutput) error {
	body := map[string]any{"status": "completed", "conclusion": conclusion, "completed_at": r.s.now().UTC().Format("2006-01-02T15:04:05Z"),
		"output": checkOutput{Title: out.Title, Summary: cut(out.Summary), Text: cut(out.Text)}}
	if detailsURL != "" {
		body["details_url"] = detailsURL
	}
	return r.s.cl.do(ctx, r.token, http.MethodPatch, fmt.Sprintf("/repos/%s/check-runs/%d", escapeSegments(r.repo), id), body, nil)
}

// comment writes the pull request's comment: it updates the one it made before, and makes a new
// one when there is none or somebody deleted it. It returns the comment's id.
func (r *reporter) comment(ctx context.Context, pr int, id int64, body string) (int64, error) {
	body = r.marker() + "\n" + body
	if id == 0 {
		// A pull request whose branch was never made (or whose link was lost) may carry the comment of an
		// earlier report: take it over instead of adding another with every commit.
		id = r.findComment(ctx, pr)
	}
	if id != 0 {
		err := r.s.cl.do(ctx, r.token, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%d", escapeSegments(r.repo), id), map[string]string{"body": body}, nil)
		if err == nil {
			return id, nil
		}
		// A comment that is gone (404) or that this App may not edit (403: somebody else's, or the
		// App lost the permission to the comment) is replaced by a new one.
		if !IsStatus(err, http.StatusNotFound) && !IsStatus(err, http.StatusForbidden) {
			return id, err
		}
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := r.s.cl.do(ctx, r.token, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", escapeSegments(r.repo), pr), map[string]string{"body": body}, &res); err != nil {
		return id, err
	}
	return res.ID, nil
}

// findComment returns the id of the comment this server wrote for the project on the pull request,
// 0 when there is none.
func (r *reporter) findComment(ctx context.Context, pr int) int64 {
	var found int64
	stop := errors.New("found")
	_ = r.s.cl.getAll(ctx, r.token, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", escapeSegments(r.repo), pr), "", 5, func(raw json.RawMessage) error {
		var page []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			App  *struct {
				ID int64 `json:"id"`
			} `json:"performed_via_github_app"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, c := range page {
			// A comment that an App other than ours wrote is not ours to edit, whatever its first line says.
			if c.App != nil && r.appID != 0 && c.App.ID != r.appID {
				continue
			}
			if strings.HasPrefix(c.Body, r.marker()+"\n") || c.Body == r.marker() {
				found = c.ID
				return stop
			}
		}
		return nil
	})
	return found
}

// stepWord is a step's status as the table says it.
func stepWord(status string) string {
	switch status {
	case registry.StepExited:
		return "Success"
	case registry.StepRunning:
		return "Running"
	case registry.StepCreated:
		return "Waiting"
	case registry.StepPaused:
		return "Skipped"
	case registry.StepDead:
		return "Failed"
	}
	return status
}

// stepTable renders the steps of a run as a Markdown table.
func stepTable(run *registry.BranchRun) string {
	if run == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("| Step | Status |\n|---|---|\n")
	for _, st := range run.Steps {
		fmt.Fprintf(&b, "| %s | %s |\n", st.Name, stepWord(st.Status))
	}
	return b.String()
}

// Outcome is how a run ended, for the report.
type outcome struct {
	Success bool
	// Neutral is a run that did nothing on purpose (no supabase directory, a limit).
	Neutral   bool
	Headline  string
	Detail    string
	Run       *registry.BranchRun
	Branch    string // branch name
	BranchRef string
	Parent    string
	Links     []string // markdown links for the footer
}

func (o outcome) conclusion() string {
	switch {
	case o.Neutral:
		return "neutral"
	case o.Success:
		return "success"
	}
	return "failure"
}

func (o outcome) summary() string {
	var b strings.Builder
	b.WriteString(o.Headline)
	b.WriteString("\n\n")
	if t := stepTable(o.Run); t != "" {
		b.WriteString(t)
		b.WriteString("\n")
	}
	if o.Detail != "" {
		b.WriteString(o.Detail)
		b.WriteString("\n\n")
	}
	if len(o.Links) > 0 {
		b.WriteString(strings.Join(o.Links, " | "))
		b.WriteString("\n")
	}
	return b.String()
}

// logText is the log of a run for the check run's text.
func logText(run *registry.BranchRun) string {
	if run == nil {
		return ""
	}
	var b strings.Builder
	for _, st := range run.Steps {
		if st.Log != "" {
			b.WriteString(st.Log)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "```\n" + strings.ReplaceAll(b.String(), "```", "'''") + "```"
}

// dashboardLinks are the footer links of a report.
func (s *Service) dashboardLinks(parent, branchRef string) []string {
	base := s.cfg.DashboardURL()
	var out []string
	if branchRef != "" {
		out = append(out, fmt.Sprintf("[Open the branch in the dashboard](%s/project/%s)", base, branchRef))
	}
	out = append(out, fmt.Sprintf("[All branches](%s/project/%s/branches)", base, parent))
	return out
}
