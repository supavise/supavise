package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestGitHubSection(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.GitHub.OperatorApp() || c.GitHub.API() != "https://api.github.com" || c.GitHub.Web() != "https://github.com" || c.GitHub.AllowForks {
		t.Fatalf("defaults = %+v", c.GitHub)
	}
	full := GitHub{AppID: 12, AppSlug: "my-app", ClientID: "Iv1.x", ClientSecretFile: "/a", PrivateKeyFile: "/b", WebhookSecretFile: "/c"}
	c.GitHub = full
	if err := c.Validate(); err != nil || !c.GitHub.OperatorApp() {
		t.Fatalf("a complete App: %v", err)
	}
	for name, mutate := range map[string]func(*GitHub){
		"no slug":         func(g *GitHub) { g.AppSlug = "" },
		"no client id":    func(g *GitHub) { g.ClientID = "" },
		"no private key":  func(g *GitHub) { g.PrivateKeyFile = "" },
		"no webhook":      func(g *GitHub) { g.WebhookSecretFile = " " },
		"negative id":     func(g *GitHub) { g.AppID = -1 },
		"api without web": func(g *GitHub) { g.APIURL = "https://ghe.example.com/api/v3" },
		"web without api": func(g *GitHub) { g.WebURL = "https://ghe.example.com" },
		"not a URL":       func(g *GitHub) { g.APIURL, g.WebURL = "ftp://x", "https://y" },
		"a query":         func(g *GitHub) { g.APIURL, g.WebURL = "https://x?a=1", "https://y" },
	} {
		c := Default()
		c.GitHub = full
		mutate(&c.GitHub)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "github") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Names without an id are a mistake worth saying.
	c = Default()
	c.GitHub = GitHub{AppSlug: "x"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "app_id") {
		t.Errorf("a slug without an id: %v", err)
	}
	// GitHub Enterprise Server: both origins, trailing slashes dropped.
	c = Default()
	c.GitHub = GitHub{APIURL: "https://ghe.example.com/api/v3/", WebURL: "https://ghe.example.com/"}
	if err := c.Validate(); err != nil || c.GitHub.API() != "https://ghe.example.com/api/v3" || c.GitHub.Web() != "https://ghe.example.com" {
		t.Errorf("GHES: %v %s %s", err, c.GitHub.API(), c.GitHub.Web())
	}
}

func TestGitHubSectionFromTOMLAndEnv(t *testing.T) {
	c := Default()
	if err := DecodeTOML([]byte("[github]\napp_id = 5\napp_slug = \"s\"\nclient_id = \"c\"\nclient_secret_file = \"/a\"\nprivate_key_file = \"/b\"\nwebhook_secret_file = \"/c\"\nallow_forks = true\n"), c); err != nil {
		t.Fatal(err)
	}
	if c.GitHub.AppID != 5 || !c.GitHub.AllowForks || c.GitHub.PrivateKeyFile != "/b" {
		t.Fatalf("%+v", c.GitHub)
	}
	if err := applyEnv(reflectValue(c), EnvPrefix, []string{"SUPAVISE_GITHUB_APP_ID=9", "SUPAVISE_GITHUB_ALLOW_FORKS=false"}); err != nil {
		t.Fatal(err)
	}
	if c.GitHub.AppID != 9 || c.GitHub.AllowForks {
		t.Fatalf("env: %+v", c.GitHub)
	}
}

func reflectValue(c *Config) reflect.Value { return reflect.ValueOf(c).Elem() }

func TestGitHubBuildsDefaultToThreeAndStayWithinBounds(t *testing.T) {
	var g GitHub
	if g.Builds() != DefaultGitHubBuilds || DefaultGitHubBuilds != 3 {
		t.Fatalf("default builds = %d", g.Builds())
	}
	g.MaxConcurrentBuilds = 5
	if g.Builds() != 5 || g.validate() != nil {
		t.Fatalf("builds = %d, %v", g.Builds(), g.validate())
	}
	for _, n := range []int{-1, MaxGitHubBuilds + 1} {
		g.MaxConcurrentBuilds = n
		if err := g.validate(); err == nil || !strings.Contains(err.Error(), "max_concurrent_builds") {
			t.Fatalf("max_concurrent_builds = %d: %v", n, err)
		}
	}
}
