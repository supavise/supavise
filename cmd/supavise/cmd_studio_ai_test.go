package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/config"
)

const cliAIKey = "sk-cli-0123456789abcdef"

// aiNode is a config directory standing in for /etc/supavise.
type aiNode struct {
	t    *testing.T
	dir  string
	path string
}

func newAINode(t *testing.T, toml string) *aiNode {
	t.Helper()
	dir := t.TempDir()
	n := &aiNode{t: t, dir: dir, path: filepath.Join(dir, "config.toml")}
	if err := os.WriteFile(n.path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	old := configPath
	configPath = n.path
	t.Cleanup(func() { configPath = old })
	return n
}

// run executes `supavise studio ...` with stdin and returns the output of both streams.
func (n *aiNode) run(stdin string, args ...string) (string, string, error) {
	n.t.Helper()
	cmd := newStudioCmd()
	var out, errw bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errw)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append([]string{"ai"}, args...))
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	return out.String(), errw.String(), err
}

func (n *aiNode) effective() config.StudioAI {
	n.t.Helper()
	c, err := config.Load(n.path)
	if err != nil {
		n.t.Fatal(err)
	}
	return c.Studio.AI
}

func stubRefresh(t *testing.T) *int {
	t.Helper()
	calls := new(int)
	old := refreshStudioAI
	refreshStudioAI = func(*cobra.Command) error { *calls++; return nil }
	t.Cleanup(func() { refreshStudioAI = old })
	return calls
}

func TestStudioAISetKeepsTheKeyOutOfConfigAndOnlyChangesWhatYouGive(t *testing.T) {
	n := newAINode(t, "domain = \"example.com\"\n")
	refreshes := stubRefresh(t)

	out, _, err := n.run(cliAIKey+"\n", "set", "--base-url", "https://openrouter.ai/api/v1", "--api-key-stdin", "--model", "openai/gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	if *refreshes != 1 {
		t.Fatalf("Studio was refreshed %d times, want once", *refreshes)
	}
	if strings.Contains(out, cliAIKey) {
		t.Fatalf("the key is in the output: %s", out)
	}
	keyPath := filepath.Join(n.dir, "studio-ai.key")
	fi, err := os.Stat(keyPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v, want mode 0600", fi, err)
	}
	if b, _ := os.ReadFile(keyPath); strings.TrimSpace(string(b)) != cliAIKey {
		t.Fatalf("key file holds %q", b)
	}
	dropIn := filepath.Join(n.dir, "config.d", studioAIDropIn)
	if fi, err := os.Stat(dropIn); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("drop-in: %v %v", fi, err)
	}
	for _, f := range []string{n.path, dropIn} {
		if b, _ := os.ReadFile(f); strings.Contains(string(b), cliAIKey) {
			t.Fatalf("the key is in %s", f)
		}
	}
	want := config.StudioAI{Provider: "openai", BaseURL: "https://openrouter.ai/api/v1", APIKeyFile: keyPath, Model: "openai/gpt-4o-mini"}
	if got := n.effective(); got != want {
		t.Fatalf("effective = %+v, want %+v", got, want)
	}

	// A second set changes the model alone.
	if _, _, err := n.run("", "set", "--model", "openai/gpt-4o", "--completion-model", "openai/gpt-4o-mini", "--reasoning-effort", "low"); err != nil {
		t.Fatal(err)
	}
	want.Model, want.CompletionModel, want.ReasoningEffort = "openai/gpt-4o", "openai/gpt-4o-mini", "low"
	if got := n.effective(); got != want {
		t.Fatalf("effective = %+v, want %+v", got, want)
	}
	// An empty value resets one.
	if _, _, err := n.run("", "set", "--reasoning-effort", "", "--completion-model", ""); err != nil {
		t.Fatal(err)
	}
	want.CompletionModel, want.ReasoningEffort = "", ""
	if got := n.effective(); got != want {
		t.Fatalf("effective = %+v, want %+v", got, want)
	}
}

// A gateway with no model gets Studio's own OpenAI names, which most gateways do not serve: set and
// check warn (a gateway that maps those names is legitimate, so neither refuses).
func TestStudioAIWarnsAboutAGatewayWithoutAModel(t *testing.T) {
	n := newAINode(t, "")
	stubRefresh(t)
	_, errOut, err := n.run(cliAIKey+"\n", "set", "--base-url", "https://openrouter.ai/api/v1", "--api-key-stdin")
	if err != nil {
		t.Fatalf("a gateway without a model is refused: %v", err)
	}
	if !strings.Contains(errOut, "no model is set") || !strings.Contains(errOut, "--model") || strings.Contains(errOut, cliAIKey) {
		t.Fatalf("set did not warn: %q", errOut)
	}
	if _, errOut, err = n.run("", "set", "--model", "anthropic/claude-sonnet-4.5"); err != nil || strings.Contains(errOut, "no model is set") {
		t.Fatalf("a gateway with a model got the warning: %q (%v)", errOut, err)
	}
	// OpenAI itself needs no model, with or without its URL.
	if _, errOut, err = n.run(cliAIKey+"\n", "set", "--base-url", "https://api.openai.com/v1", "--model", "", "--api-key-stdin"); err != nil || strings.Contains(errOut, "no model is set") {
		t.Fatalf("OpenAI got the warning: %q (%v)", errOut, err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"data":[]}`) }))
	defer srv.Close()
	var out bytes.Buffer
	if err := checkStudioAI(context.Background(), &out, config.StudioAI{BaseURL: srv.URL + "/v1"}, http.DefaultTransport); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no model is set") {
		t.Fatalf("check did not warn: %q", out.String())
	}
	out.Reset()
	if err := checkStudioAI(context.Background(), &out, config.StudioAI{BaseURL: srv.URL + "/v1", Model: "m"}, http.DefaultTransport); err != nil || strings.Contains(out.String(), "no model is set") {
		t.Fatalf("check with a model: %q (%v)", out.String(), err)
	}
}

func TestStudioAISetRefusesWhatCannotWork(t *testing.T) {
	n := newAINode(t, "")
	stubRefresh(t)
	for name, tc := range map[string]struct {
		stdin string
		args  []string
		want  string
	}{
		"nothing to configure":      {"", []string{"set", "--model", "m"}, "nothing to set"},
		"a model name with a space": {cliAIKey, []string{"set", "--api-key-stdin", "--model", "two words"}, "not a model name"},
		"a key with a space":        {"sk test", []string{"set", "--api-key-stdin"}, "not usable"},
		"an empty key":              {"\n", []string{"set", "--api-key-stdin"}, "empty"},
		"a URL with credentials":    {"", []string{"set", "--base-url", "https://u:p@gw.example.com/v1"}, "must not carry"},
		"a missing key file":        {"", []string{"set", "--api-key-file", filepath.Join(n.dir, "nope")}, "no such file"},
		"two key sources":           {cliAIKey, []string{"set", "--api-key-stdin", "--no-api-key"}, "none of the others can be"},
	} {
		_, _, err := n.run(tc.stdin, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want one containing %q", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), cliAIKey) {
			t.Errorf("%s: the error repeats the key: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(n.dir, "config.d", studioAIDropIn)); err == nil {
		t.Fatal("a refused set wrote a drop-in")
	}
}

func TestStudioAISetFromAFileAndKeyless(t *testing.T) {
	n := newAINode(t, "")
	stubRefresh(t)
	src := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(src, []byte("  "+cliAIKey+"  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := n.run("", "set", "--api-key-file", src); err != nil {
		t.Fatal(err)
	}
	ai := n.effective()
	if ai.APIKeyFile != filepath.Join(n.dir, "studio-ai.key") {
		t.Fatalf("api_key_file = %q: the command must keep its own copy", ai.APIKeyFile)
	}
	if key, err := ai.ReadKey(); err != nil || key != cliAIKey {
		t.Fatalf("ReadKey = %q, %v", key, err)
	}
	// A local Ollama takes no key: --no-api-key drops ours and keeps the gateway.
	if _, _, err := n.run("", "set", "--base-url", "http://127.0.0.1:11434/v1", "--no-api-key", "--model", "llama3.1:8b"); err != nil {
		t.Fatal(err)
	}
	ai = n.effective()
	if ai.APIKeyFile != "" || ai.BaseURL != "http://127.0.0.1:11434/v1" || !ai.Enabled() {
		t.Fatalf("effective = %+v", ai)
	}
	if _, err := os.Stat(filepath.Join(n.dir, "studio-ai.key")); err == nil {
		t.Fatal("the key file stayed after --no-api-key")
	}
}

func TestStudioAIKeepsAKeyFileTheOperatorPointedAt(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "mounted")
	if err := os.WriteFile(secret, []byte(cliAIKey), 0o600); err != nil {
		t.Fatal(err)
	}
	n := newAINode(t, "[studio.ai]\napi_key_file = \""+secret+"\"\nbase_url = \"https://gw.example.com/v1\"\n")
	stubRefresh(t)
	if _, _, err := n.run("", "set", "--model", "m"); err != nil {
		t.Fatal(err)
	}
	if ai := n.effective(); ai.APIKeyFile != secret || ai.Model != "m" {
		t.Fatalf("effective = %+v", ai)
	}
	// A key file that the daemon would refuse (readable by others) fails the command, not the next render.
	if err := os.Chmod(secret, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := n.run("", "set", "--model", "m2"); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("error = %v", err)
	}
}

func TestStudioAIClearOverridesConfigToml(t *testing.T) {
	n := newAINode(t, "[studio.ai]\nbase_url = \"https://gw.example.com/v1\"\nmodel = \"m\"\n")
	refreshes := stubRefresh(t)
	if _, _, err := n.run(cliAIKey, "set", "--api-key-stdin"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := n.run("", "clear"); err != nil {
		t.Fatal(err)
	}
	if *refreshes != 2 {
		t.Fatalf("refreshes = %d, want one per command", *refreshes)
	}
	if ai := n.effective(); ai != (config.StudioAI{}) {
		t.Fatalf("after clear: %+v", ai)
	}
	if _, err := os.Stat(filepath.Join(n.dir, "studio-ai.key")); err == nil {
		t.Fatal("clear left the key file")
	}
	// --no-restart writes and leaves Studio alone.
	if _, _, err := n.run("", "clear", "--no-restart"); err != nil || *refreshes != 2 {
		t.Fatalf("clear --no-restart: %v, refreshes %d", err, *refreshes)
	}
}

func TestStudioAIShowHasNoKey(t *testing.T) {
	n := newAINode(t, "")
	stubRefresh(t)
	out, _, err := n.run("", "show")
	if err != nil || !strings.Contains(out, "No AI provider is set") {
		t.Fatalf("show with nothing set: %v %q", err, out)
	}
	if _, _, err := n.run(cliAIKey, "set", "--api-key-stdin", "--base-url", "https://gw.example.com/v1", "--model", "m"); err != nil {
		t.Fatal(err)
	}
	out, _, err = n.run("", "show")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"https://gw.example.com/v1", "set (not shown)", "chat", "Model:"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	jsonOut, _, err := n.run("", "show", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var r studioAIReport
	if err := json.Unmarshal([]byte(jsonOut), &r); err != nil || !r.Configured || r.Model != "m" || r.CompletionModel != "m" {
		t.Fatalf("json = %q: %v %+v", jsonOut, err, r)
	}
	if strings.Contains(out, cliAIKey) || strings.Contains(jsonOut, cliAIKey) {
		t.Fatal("show printed the key")
	}
	// A key file that cannot be used is reported, without its content.
	if err := os.Chmod(filepath.Join(n.dir, "studio-ai.key"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, _ = n.run("", "show")
	if !strings.Contains(out, "unusable") || strings.Contains(out, cliAIKey) {
		t.Fatalf("show with a bad key file:\n%s", out)
	}
}

func TestCheckStudioAI(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		if gotAuth != "Bearer "+cliAIKey {
			http.Error(w, `{"error":"bad key `+gotAuth+`"}`, http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"data":[{"id":"a"},{"id":"b"}]}`)
	}))
	defer srv.Close()

	keyFile := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(keyFile, []byte(cliAIKey), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ai := config.StudioAI{BaseURL: srv.URL + "/v1/", APIKeyFile: keyFile, Model: "a"}
	if err := checkStudioAI(context.Background(), &out, ai, http.DefaultTransport); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/models" || gotAuth != "Bearer "+cliAIKey {
		t.Fatalf("request: %s %q", gotPath, gotAuth)
	}
	if !strings.Contains(out.String(), "lists 2 models") || strings.Contains(out.String(), cliAIKey) {
		t.Fatalf("output = %q", out.String())
	}

	// A wrong key: the error says so and never repeats the key, even when the provider echoes it.
	if err := os.WriteFile(keyFile, []byte(cliAIKey+"x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := checkStudioAI(context.Background(), &out, ai, http.DefaultTransport)
	if err == nil || !strings.Contains(err.Error(), "does not accept the key") || strings.Contains(err.Error(), cliAIKey) {
		t.Fatalf("error = %v", err)
	}
	// Nothing set, and a server that is not there.
	if err := checkStudioAI(context.Background(), &out, config.StudioAI{}, http.DefaultTransport); err == nil {
		t.Fatal("check without a provider succeeded")
	}
	srv.Close()
	if err := checkStudioAI(context.Background(), &out, config.StudioAI{BaseURL: srv.URL + "/v1"}, http.DefaultTransport); err == nil || !strings.Contains(err.Error(), "firewall") {
		t.Fatalf("error = %v", err)
	}
}
