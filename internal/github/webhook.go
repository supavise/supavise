package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// The webhook receiver. GitHub posts every event of the App's repositories to
// api.<domain>/hooks/github. Nothing in a delivery is believed until its signature checks out; then
// the delivery id is remembered so that a replay does nothing, and the work (which can take
// minutes) runs after the answer, on the queue of the branch or pull request it concerns.

// Delivery is a webhook as the HTTP layer hands it over.
type Delivery struct {
	Event     string // X-GitHub-Event
	ID        string // X-GitHub-Delivery
	Signature string // X-Hub-Signature-256
	Body      []byte
}

// MaxWebhookBody bounds a delivery. GitHub caps payloads at 25 MB; a pull request or push event
// of a real repository is a few kilobytes to a few hundred.
const MaxWebhookBody = 8 << 20

// WebhookResult is the answer to a delivery.
type WebhookResult struct {
	Status  int
	Message string
}

func res(status int, format string, a ...any) WebhookResult {
	return WebhookResult{Status: status, Message: fmt.Sprintf(format, a...)}
}

// rateLimiter is a token bucket for the receiver: the signature check is cheap but a body is not
// free to read, and the route has no credentials.
type rateLimiter struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	now    func() time.Time
}

const (
	hookRate  = 30.0  // deliveries per second, sustained
	hookBurst = 120.0 // and at once
)

func (l *rateLimiter) init(now func() time.Time) {
	l.now = now
	l.tokens = hookBurst
	l.last = now()
}

func (l *rateLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.tokens = min(hookBurst, l.tokens+now.Sub(l.last).Seconds()*hookRate)
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// SignatureOK checks X-Hub-Signature-256 ("sha256=<hex>") of the body under the webhook secret, in
// constant time.
func SignatureOK(secret string, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// payload types: only the fields the receiver reads.
type repoRef struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

type installRef struct {
	ID int64 `json:"id"`
}

type prPayload struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Number int    `json:"number"`
		State  string `json:"state"`
		Title  string `json:"title"`
		Merged bool   `json:"merged"`
		Head   struct {
			Ref  string   `json:"ref"`
			SHA  string   `json:"sha"`
			Repo *repoRef `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref  string   `json:"ref"`
			SHA  string   `json:"sha"`
			Repo *repoRef `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository   repoRef     `json:"repository"`
	Installation *installRef `json:"installation"`
}

type pushPayload struct {
	Ref     string `json:"ref"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`
	Commits []struct {
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
		Removed  []string `json:"removed"`
	} `json:"commits"`
	Repository   repoRef     `json:"repository"`
	Installation *installRef `json:"installation"`
}

type refPayload struct {
	Ref          string      `json:"ref"`
	RefType      string      `json:"ref_type"`
	Repository   repoRef     `json:"repository"`
	Installation *installRef `json:"installation"`
}

// HandleWebhook verifies and handles one delivery.
func (s *Service) HandleWebhook(ctx context.Context, d Delivery) WebhookResult {
	if !s.limit.allow() {
		return res(http.StatusTooManyRequests, "too many deliveries")
	}
	app, err := s.App(ctx)
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return res(http.StatusNotFound, "the GitHub integration is not configured")
		}
		s.log.Error("a GitHub webhook arrived but the App cannot be read", "err", err)
		return res(http.StatusServiceUnavailable, "the GitHub App cannot be read")
	}
	if !SignatureOK(app.WebhookSecret, d.Body, d.Signature) {
		return res(http.StatusUnauthorized, "the signature does not match")
	}
	if d.ID == "" || d.Event == "" {
		return res(http.StatusBadRequest, "X-GitHub-Delivery and X-GitHub-Event are required")
	}
	if d.Event == "ping" {
		return res(http.StatusOK, "pong")
	}
	fresh, err := s.st.RecordGitHubDelivery(ctx, d.ID, d.Event, s.now())
	if err != nil {
		s.log.Error("could not record a GitHub delivery", "err", err)
		return res(http.StatusInternalServerError, "could not record the delivery")
	}
	if !fresh {
		return res(http.StatusOK, "the delivery was handled before")
	}
	s.pruneSometimes(ctx)
	msg, err := s.dispatch(ctx, d)
	if err != nil {
		// A delivery that could not even be read may be sent again.
		_ = s.st.ForgetGitHubDelivery(ctx, d.ID)
		if errors.Is(err, ErrInvalid) {
			return res(http.StatusBadRequest, "%v", err)
		}
		s.log.Error("a GitHub delivery could not be handled", "event", d.Event, "err", err)
		return res(http.StatusInternalServerError, "the delivery could not be handled")
	}
	return res(http.StatusAccepted, "%s", msg)
}

func (s *Service) pruneSometimes(ctx context.Context) {
	s.pruneMu.Lock()
	now := s.now()
	due := now.Sub(s.prunedAt) > time.Hour
	if due {
		s.prunedAt = now
	}
	s.pruneMu.Unlock()
	if due {
		if err := s.st.PruneGitHubDeliveries(ctx, now.Add(-72*time.Hour)); err != nil {
			s.log.Warn("could not prune the GitHub deliveries", "err", err)
		}
	}
}

// connections returns the connections to the repository that belong to the installation the
// event came from.
func (s *Service) connectionsFor(ctx context.Context, repo repoRef, inst *installRef) ([]registry.GitHubConnection, error) {
	all, err := s.st.ListGitHubConnectionsByRepo(ctx, repo.ID)
	if err != nil {
		return nil, err
	}
	var out []registry.GitHubConnection
	for _, c := range all {
		if inst != nil && inst.ID != 0 && c.InstallationID != inst.ID {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *Service) dispatch(ctx context.Context, d Delivery) (string, error) {
	switch d.Event {
	case "pull_request":
		var p prPayload
		if err := json.Unmarshal(d.Body, &p); err != nil {
			return "", invalid("the pull_request payload: %v", err)
		}
		return s.onPullRequest(ctx, &p)
	case "push":
		var p pushPayload
		if err := json.Unmarshal(d.Body, &p); err != nil {
			return "", invalid("the push payload: %v", err)
		}
		return s.onPush(ctx, &p)
	case "delete":
		var p refPayload
		if err := json.Unmarshal(d.Body, &p); err != nil {
			return "", invalid("the delete payload: %v", err)
		}
		return s.onRefDeleted(ctx, &p)
	case "create":
		return "the creation of a Git branch needs no work", nil
	case "installation", "installation_repositories", "installation_target", "meta":
		// A repository leaving an installation cannot be told apart from one still there until a
		// token is used; the next event of it fails and says so.
		return "noted", nil
	}
	return "this event is not used", nil
}

// onPullRequest queues the work of a pull request event for every connection to its repository.
func (s *Service) onPullRequest(ctx context.Context, p *prPayload) (string, error) {
	switch p.Action {
	case "opened", "reopened", "synchronize", "ready_for_review", "closed":
	default:
		return "the action " + p.Action + " needs no work", nil
	}
	n := p.Number
	if n == 0 {
		n = p.PullRequest.Number
	}
	if n == 0 || p.PullRequest.Head.Ref == "" || p.PullRequest.Head.SHA == "" || p.Repository.ID == 0 {
		return "", invalid("the pull request payload lacks its number, head or repository")
	}
	// A pull request from a fork carries code that anybody chose. It is ignored unless the operator
	// said otherwise in [github] allow_forks.
	head := p.PullRequest.Head.Repo
	if (head == nil || head.ID != p.Repository.ID) && !s.cfg.GitHub.AllowForks {
		return "ignored: a pull request from a fork (set [github] allow_forks to build them)", nil
	}
	conns, err := s.connectionsFor(ctx, p.Repository, p.Installation)
	if err != nil {
		return "", err
	}
	if len(conns) == 0 {
		return "no project is connected to " + p.Repository.FullName, nil
	}
	ev := prEvent{Action: p.Action, Number: n, HeadRef: p.PullRequest.Head.Ref, HeadSHA: p.PullRequest.Head.SHA,
		BaseRef: p.PullRequest.Base.Ref, Title: p.PullRequest.Title, Merged: p.PullRequest.Merged}
	for _, c := range conns {
		s.enqueue(fmt.Sprintf("pr:%d:%d", c.ID, n), func(ctx context.Context) { s.runPR(ctx, c, ev) })
	}
	return fmt.Sprintf("queued for %d project(s)", len(conns)), nil
}

// onRefDeleted deletes the preview branch that followed a Git branch that was deleted, unless a
// pull request owns it (its close event does that) or it is persistent.
func (s *Service) onRefDeleted(ctx context.Context, p *refPayload) (string, error) {
	if p.RefType != "branch" || p.Ref == "" {
		return "only the deletion of a branch needs work", nil
	}
	conns, err := s.connectionsFor(ctx, p.Repository, p.Installation)
	if err != nil {
		return "", err
	}
	for _, c := range conns {
		s.enqueue(fmt.Sprintf("git:%d:%s", c.ID, p.Ref), func(ctx context.Context) {
			if err := s.deleteGitBranch(ctx, c, p.Ref); err != nil && ctx.Err() == nil {
				s.log.Warn("could not delete the branch of a deleted Git branch", "repo", c.RepositoryName, "branch", p.Ref, "err", err)
			}
		})
	}
	return fmt.Sprintf("queued for %d project(s)", len(conns)), nil
}

// onPush queues a deployment: of production, when the push is to the production branch, and of the
// preview and persistent branches that follow the pushed Git branch.
func (s *Service) onPush(ctx context.Context, p *pushPayload) (string, error) {
	branch, ok := strings.CutPrefix(p.Ref, "refs/heads/")
	if !ok || p.Deleted || p.After == "" || strings.Trim(p.After, "0") == "" {
		return "only a push to a branch needs work", nil
	}
	conns, err := s.connectionsFor(ctx, p.Repository, p.Installation)
	if err != nil {
		return "", err
	}
	queued := 0
	for _, c := range conns {
		prod, _ := s.productionBranch(ctx, c.ProjectRef)
		if prod != "" && prod == branch {
			if c.SupabaseChangesOnly && !pushTouches(p, supabaseDir(c.Workdir)) {
				continue
			}
			queued++
			s.enqueue("prod:"+c.ProjectRef, func(ctx context.Context) {
				if err := s.deployProduction(ctx, c, branch, p.After, nil); err != nil && ctx.Err() == nil {
					s.log.Warn("a production deployment failed", "repo", c.RepositoryName, "err", err)
				}
			})
			continue
		}
		queued++
		s.enqueue(fmt.Sprintf("git:%d:%s", c.ID, branch), func(ctx context.Context) {
			if err := s.syncGitBranch(ctx, c, branch, p.After); err != nil && ctx.Err() == nil {
				s.log.Warn("a Git branch could not be deployed", "repo", c.RepositoryName, "branch", branch, "err", err)
			}
		})
	}
	return fmt.Sprintf("queued for %d project(s)", queued), nil
}

// pushTouches reports whether the commits of a push change a file under dir. A push that lists
// no commits (a merge GitHub did not itemize) is taken to touch it.
func pushTouches(p *pushPayload, dir string) bool {
	if len(p.Commits) == 0 || len(p.Commits) >= 20 {
		return true // GitHub lists at most 20 commits; the rest cannot be told
	}
	for _, c := range p.Commits {
		for _, list := range [][]string{c.Added, c.Modified, c.Removed} {
			for _, f := range list {
				if strings.HasPrefix(f, dir) {
					return true
				}
			}
		}
	}
	return false
}
