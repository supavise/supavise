package fleet

import (
	"context"
	"errors"

	"github.com/supavise/supavise/internal/registry"
)

// githubNames are what Studio's GitHub buttons are built from: the App's name in
// github.com/apps/<name> and its client id (NEXT_PUBLIC_GITHUB_INTEGRATION_APP_NAME and _CLIENT_ID,
// studio/placeholders.json).
type githubNames struct{ slug, clientID string }

// githubApp reads the names of the server's GitHub App: the one [github] names, else the one the
// manifest flow stored. Without an App both are empty.
func (d Deps) githubApp(ctx context.Context) (githubNames, error) {
	if d.Cfg != nil && d.Cfg.GitHub.OperatorApp() {
		return githubNames{slug: d.Cfg.GitHub.AppSlug, clientID: d.Cfg.GitHub.ClientID}, nil
	}
	st := registry.GitHubOf(d.Registry)
	if st == nil {
		return githubNames{}, nil
	}
	a, err := st.GetGitHubApp(ctx)
	if errors.Is(err, registry.ErrNotFound) {
		return githubNames{}, nil
	}
	if err != nil {
		return githubNames{}, err
	}
	return githubNames{slug: a.Slug, clientID: a.ClientID}, nil
}

// githubStudioEnv is the part of Studio's environment that names the GitHub App. A server without
// one gets nothing, and the build's default (a name that belongs to nobody) stays: Studio reads
// an empty value as "use Supabase's own App", which no server of ours may do. It also hides the
// GitHub surfaces (the profile's disabled_features, Management API).
func githubStudioEnv(n githubNames) map[string]string {
	if n.slug == "" || n.clientID == "" {
		return nil
	}
	return map[string]string{
		"NEXT_PUBLIC_GITHUB_INTEGRATION_APP_NAME":  n.slug,
		"NEXT_PUBLIC_GITHUB_INTEGRATION_CLIENT_ID": n.clientID,
	}
}
