package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// instToken is an installation token with its expiry.
type instToken struct {
	token   string
	expires time.Time
}

func (s *Service) dropTokens() {
	s.tokMu.Lock()
	s.tokens = map[int64]instToken{}
	s.tokMu.Unlock()
}

// installationToken returns a token for the App's installation, from the cache while it has more
// than five minutes left. The token reads only what the App was granted on the repositories the
// installation holds.
func (s *Service) installationToken(ctx context.Context, installationID int64) (string, error) {
	s.tokMu.Lock()
	t, ok := s.tokens[installationID]
	s.tokMu.Unlock()
	if ok && t.expires.Sub(s.now()) > 5*time.Minute {
		return t.token, nil
	}
	app, err := s.App(ctx)
	if err != nil {
		return "", err
	}
	jwtTok, err := app.JWT(s.now())
	if err != nil {
		return "", fmt.Errorf("github: sign the App's token: %w", err)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := s.cl.do(ctx, jwtTok, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installationID), map[string]any{}, &out); err != nil {
		if IsStatus(err, http.StatusNotFound) {
			return "", fmt.Errorf("%w: installation %d of the GitHub App is gone", ErrNotFound, installationID)
		}
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("github: GitHub issued an empty installation token")
	}
	if out.ExpiresAt.IsZero() {
		out.ExpiresAt = s.now().Add(time.Hour)
	}
	s.tokMu.Lock()
	s.tokens[installationID] = instToken{token: out.Token, expires: out.ExpiresAt}
	s.tokMu.Unlock()
	return out.Token, nil
}

// ---- users ------------------------------------------------------------------------------------

// tokenAnswer is GitHub's answer to the OAuth token endpoint. An error comes with status 200.
type tokenAnswer struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// oauthToken posts form fields to the OAuth token endpoint of GitHub's web origin.
func (s *Service) oauthToken(ctx context.Context, fields url.Values) (*tokenAnswer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cl.Web+"/login/oauth/access_token", strings.NewReader(fields.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", s.cl.UserAgent)
	resp, err := s.cl.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out tokenAnswer
	if err := json.Unmarshal(b, &out); err != nil || resp.StatusCode >= 500 {
		return nil, fmt.Errorf("github: the OAuth token endpoint answered %d", resp.StatusCode)
	}
	return &out, nil
}

// Authorize completes a user's OAuth authorization: it exchanges the code Studio's authorize page
// got from GitHub for the user's token and keeps it, sealed, for that Supavise user.
func (s *Service) Authorize(ctx context.Context, userID, code string) (*registry.GitHubAuthorization, error) {
	app, err := s.App(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(code) == "" {
		return nil, invalid("the code is required")
	}
	ans, err := s.oauthToken(ctx, url.Values{"client_id": {app.ClientID}, "client_secret": {app.ClientSecret}, "code": {code}})
	if err != nil {
		return nil, err
	}
	if ans.Error != "" || ans.AccessToken == "" {
		// bad_verification_code and the like: the code was used, expired or is not this App's.
		return nil, invalid("GitHub did not accept the authorization code (%s)", ans.Error)
	}
	var me struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := s.cl.do(ctx, ans.AccessToken, http.MethodGet, "/user", nil, &me); err != nil {
		return nil, fmt.Errorf("github: read the authorized user: %w", err)
	}
	row := &registry.GitHubAuthorization{UserID: userID, AppID: app.ID, GitHubUserID: me.ID, GitHubLogin: me.Login}
	if err := s.sealTokens(row, ans); err != nil {
		return nil, err
	}
	if err := s.st.PutGitHubAuthorization(ctx, row); err != nil {
		return nil, err
	}
	s.auditEvent(ctx, "github.authorized", map[string]any{"user_id": userID, "github_login": me.Login})
	return row, nil
}

// sealTokens puts a token answer into an authorization row.
func (s *Service) sealTokens(row *registry.GitHubAuthorization, ans *tokenAnswer) error {
	var err error
	if row.SealedAccess, err = s.sec.Seal([]byte(ans.AccessToken)); err != nil {
		return err
	}
	row.SealedRefresh = nil
	if ans.RefreshToken != "" {
		if row.SealedRefresh, err = s.sec.Seal([]byte(ans.RefreshToken)); err != nil {
			return err
		}
	}
	row.AccessExpiresAt, row.RefreshExpiresAt = nil, nil
	now := s.now()
	if ans.ExpiresIn > 0 {
		t := now.Add(time.Duration(ans.ExpiresIn) * time.Second).UTC()
		row.AccessExpiresAt = &t
	}
	if ans.RefreshExpiresIn > 0 {
		t := now.Add(time.Duration(ans.RefreshExpiresIn) * time.Second).UTC()
		row.RefreshExpiresAt = &t
	}
	return nil
}

// Authorization returns the user's authorization, ErrNotAuthorized when there is none (or it
// belongs to an App that is not this server's any more).
func (s *Service) Authorization(ctx context.Context, userID string) (*registry.GitHubAuthorization, error) {
	app, err := s.App(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.st.GetGitHubAuthorization(ctx, userID)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, ErrNotAuthorized
	}
	if err != nil {
		return nil, err
	}
	if row.AppID != app.ID {
		return nil, ErrNotAuthorized
	}
	return row, nil
}

// refreshLocks serialize the refresh of one user's token: a refresh token works once.
var refreshLocks sync.Map

// userToken returns a token that GitHub accepts for the user, refreshing it when it is about to
// expire. A user whose token cannot be refreshed loses their authorization.
func (s *Service) userToken(ctx context.Context, userID string) (string, error) {
	row, err := s.Authorization(ctx, userID)
	if err != nil {
		return "", err
	}
	if row.AccessExpiresAt == nil || row.AccessExpiresAt.Sub(s.now()) > time.Minute {
		b, err := s.sec.Open(row.SealedAccess)
		return string(b), err
	}
	mu, _ := refreshLocks.LoadOrStore(userID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	// Another call may have refreshed while this one waited.
	if row, err = s.Authorization(ctx, userID); err != nil {
		return "", err
	}
	if row.AccessExpiresAt == nil || row.AccessExpiresAt.Sub(s.now()) > time.Minute {
		b, err := s.sec.Open(row.SealedAccess)
		return string(b), err
	}
	drop := func() (string, error) {
		_, _ = s.st.DeleteGitHubAuthorization(ctx, userID)
		return "", ErrNotAuthorized
	}
	if len(row.SealedRefresh) == 0 || (row.RefreshExpiresAt != nil && !row.RefreshExpiresAt.After(s.now())) {
		return drop()
	}
	refresh, err := s.sec.Open(row.SealedRefresh)
	if err != nil {
		return "", err
	}
	app, err := s.App(ctx)
	if err != nil {
		return "", err
	}
	ans, err := s.oauthToken(ctx, url.Values{"client_id": {app.ClientID}, "client_secret": {app.ClientSecret},
		"grant_type": {"refresh_token"}, "refresh_token": {string(refresh)}})
	if err != nil {
		return "", err
	}
	if ans.Error != "" || ans.AccessToken == "" {
		return drop()
	}
	if err := s.sealTokens(row, ans); err != nil {
		return "", err
	}
	if err := s.st.PutGitHubAuthorization(ctx, row); err != nil {
		return "", err
	}
	return ans.AccessToken, nil
}

// asUser runs call with the user's token. When GitHub refuses the token the authorization is
// removed and the call fails with ErrNotAuthorized.
func (s *Service) asUser(ctx context.Context, userID string, call func(token string) error) error {
	tok, err := s.userToken(ctx, userID)
	if err != nil {
		return err
	}
	err = call(tok)
	if IsStatus(err, http.StatusUnauthorized) {
		_, _ = s.st.DeleteGitHubAuthorization(ctx, userID)
		return ErrNotAuthorized
	}
	return err
}

// Deauthorize removes the user's authorization and revokes the token at GitHub (best effort). It
// reports whether the user had one.
func (s *Service) Deauthorize(ctx context.Context, userID string) (bool, error) {
	row, err := s.st.GetGitHubAuthorization(ctx, userID)
	if errors.Is(err, registry.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if app, aerr := s.App(ctx); aerr == nil && row.AppID == app.ID {
		if tok, oerr := s.sec.Open(row.SealedAccess); oerr == nil {
			basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(app.ClientID+":"+app.ClientSecret))
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			if derr := s.cl.doAuth(rctx, basic, http.MethodDelete, "/applications/"+url.PathEscape(app.ClientID)+"/token",
				map[string]string{"access_token": string(tok)}, nil); derr != nil && !IsStatus(derr, http.StatusNotFound) {
				s.log.Warn("could not revoke a GitHub token", "err", derr)
			}
			cancel()
		}
	}
	had, err := s.st.DeleteGitHubAuthorization(ctx, userID)
	if had {
		s.auditEvent(ctx, "github.deauthorized", map[string]any{"user_id": userID})
	}
	return had, err
}
