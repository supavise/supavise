package config

import (
	"fmt"
	"net/url"
	"strings"
)

// GitHub is the [github] config section: the GitHub App that connects projects to repositories.
//
// Without this section the App comes from GitHub's manifest flow (`supavise github app create`),
// which stores the App's credentials sealed in the registry. A node that must use an App that
// already exists sets the six App fields below instead; the secrets stay in files (mode 0600,
// readable by the supavise user) and never in config.toml. The App fields win over a stored App.
// Without either, the dashboard hides every GitHub surface: Supavise never sends an operator to
// another party's GitHub App.
type GitHub struct {
	// AppID, AppSlug and ClientID identify an App you made yourself. AppSlug is the name in
	// github.com/apps/<slug>, which the install link needs.
	AppID    int64  `toml:"app_id"`
	AppSlug  string `toml:"app_slug"`
	ClientID string `toml:"client_id"`
	// ClientSecretFile, PrivateKeyFile and WebhookSecretFile name the files that hold the App's
	// client secret, its private key (PEM) and its webhook secret.
	ClientSecretFile  string `toml:"client_secret_file"`
	PrivateKeyFile    string `toml:"private_key_file"`
	WebhookSecretFile string `toml:"webhook_secret_file"`

	// APIURL and WebURL are the API and the web origins of GitHub. Empty means github.com
	// (https://api.github.com and https://github.com); a GitHub Enterprise Server sets both.
	APIURL string `toml:"api_url"`
	WebURL string `toml:"web_url"`

	// AllowForks lets a pull request from a fork create and update a preview branch. Off by
	// default: the author of a pull request chooses the SQL and the function code that runs on the
	// branch, and anyone can open a pull request from a fork of a public repository.
	AllowForks bool `toml:"allow_forks"`
}

// Default origins of GitHub.
const (
	DefaultGitHubAPIURL = "https://api.github.com"
	DefaultGitHubWebURL = "https://github.com"
)

// OperatorApp reports whether the App is given in this section, not created by the manifest flow.
func (g GitHub) OperatorApp() bool { return g.AppID != 0 }

// API returns the API origin, no trailing slash.
func (g GitHub) API() string {
	if g.APIURL == "" {
		return DefaultGitHubAPIURL
	}
	return strings.TrimRight(g.APIURL, "/")
}

// Web returns the web origin, no trailing slash.
func (g GitHub) Web() string {
	if g.WebURL == "" {
		return DefaultGitHubWebURL
	}
	return strings.TrimRight(g.WebURL, "/")
}

func (g GitHub) validate() error {
	for name, v := range map[string]string{"api_url": g.APIURL, "web_url": g.WebURL} {
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("config: github.%s %q: want an http or https URL", name, v)
		}
	}
	if (g.APIURL == "") != (g.WebURL == "") {
		return fmt.Errorf("config: github.api_url and github.web_url go together (a GitHub Enterprise Server has both)")
	}
	if g.AppID == 0 {
		for name, v := range map[string]string{"app_slug": g.AppSlug, "client_id": g.ClientID, "client_secret_file": g.ClientSecretFile,
			"private_key_file": g.PrivateKeyFile, "webhook_secret_file": g.WebhookSecretFile} {
			if v != "" {
				return fmt.Errorf("config: github.%s is set without github.app_id", name)
			}
		}
		return nil
	}
	if g.AppID < 0 {
		return fmt.Errorf("config: github.app_id %d is not an App id", g.AppID)
	}
	for name, v := range map[string]string{"app_slug": g.AppSlug, "client_id": g.ClientID, "client_secret_file": g.ClientSecretFile,
		"private_key_file": g.PrivateKeyFile, "webhook_secret_file": g.WebhookSecretFile} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("config: github.app_id is set, so github.%s is needed too", name)
		}
	}
	return nil
}
