package github

import (
	"context"
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
	commentMarker       = "<!-- supavise-preview -->"
	maxCheckText        = 60000
)

// reporter writes the check run and the comment of a repository.
type reporter struct {
	s     *Service
	repo  string
	token string
}

func (s *Service) reporterFor(ctx context.Context, installationID int64, repo string) (*reporter, error) {
	tok, err := s.installationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	return &reporter{s: s, repo: repo, token: tok}, nil
}

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
	body = commentMarker + "\n" + body
	if id != 0 {
		err := r.s.cl.do(ctx, r.token, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%d", escapeSegments(r.repo), id), map[string]string{"body": body}, nil)
		if err == nil {
			return id, nil
		}
		if !IsStatus(err, http.StatusNotFound) {
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
