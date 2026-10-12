package fleet

import (
	"context"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

func TestGitHubStudioEnvNamesOnlyAnAppThatExists(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMemory()
	d := Deps{Cfg: config.Default(), Registry: reg}
	n, err := d.githubApp(ctx)
	if err != nil || n != (githubNames{}) {
		t.Fatalf("no App: %+v, %v", n, err)
	}
	// Studio reads an empty value as Supabase's own App, so a server without one sets nothing and keeps the build's default.
	if env := githubStudioEnv(n); len(env) != 0 {
		t.Fatalf("env without an App = %v", env)
	}
	if err := reg.PutGitHubApp(ctx, &registry.GitHubApp{AppID: 1, Slug: "my-supavise", ClientID: "Iv1.abc", SealedClientSecret: []byte("x"), SealedWebhookSecret: []byte("x"), SealedPrivateKey: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if n, err = d.githubApp(ctx); err != nil || n.slug != "my-supavise" || n.clientID != "Iv1.abc" {
		t.Fatalf("stored App: %+v, %v", n, err)
	}
	env := githubStudioEnv(n)
	if env["NEXT_PUBLIC_GITHUB_INTEGRATION_APP_NAME"] != "my-supavise" || env["NEXT_PUBLIC_GITHUB_INTEGRATION_CLIENT_ID"] != "Iv1.abc" {
		t.Fatalf("env = %v", env)
	}
	// An App named in config.toml wins over the stored one.
	d.Cfg.GitHub = config.GitHub{AppID: 5, AppSlug: "from-config", ClientID: "Iv1.cfg", ClientSecretFile: "a", PrivateKeyFile: "b", WebhookSecretFile: "c"}
	if n, _ = d.githubApp(ctx); n.slug != "from-config" || n.clientID != "Iv1.cfg" {
		t.Fatalf("config App: %+v", n)
	}
}

// The values go into Studio's build as runtime substitutions that accept a narrow alphabet; the
// names GitHub issues stay inside it.
func TestGitHubStudioEnvValuesPassStudiosValueCheck(t *testing.T) {
	for _, v := range []string{"supavise-example", "my_app.2", "Iv1.b91a6d8eaa272168", "Iv23liABCdef"} {
		for _, r := range v {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_'
			if !ok {
				t.Errorf("%q has %q", v, r)
			}
		}
	}
}
