package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKey(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ai.key")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // the umask may have cleared bits
		t.Fatal(err)
	}
	return p
}

func TestStudioAILoadsFromTOMLConfigDAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`
[studio.ai]
base_url = "https://openrouter.ai/api/v1"
api_key_file = "/etc/supavise/studio-ai.key"
model = "openai/gpt-4o"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "config.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A drop-in sets what it names and leaves the rest.
	if err := os.WriteFile(filepath.Join(dir, "config.d", "40-studio-ai.toml"), []byte("[studio.ai]\ncompletion_model = \"openai/gpt-4o-mini\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUPAVISE_STUDIO_AI_REASONING_EFFORT", "low")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := StudioAI{
		BaseURL: "https://openrouter.ai/api/v1", APIKeyFile: "/etc/supavise/studio-ai.key", Model: "openai/gpt-4o",
		CompletionModel: "openai/gpt-4o-mini", ReasoningEffort: "low",
	}
	if c.Studio.AI != want {
		t.Fatalf("studio.ai = %+v, want %+v", c.Studio.AI, want)
	}
	if !c.Studio.AI.Enabled() || c.Studio.AI.ResolvedAPI() != "chat" {
		t.Fatalf("a gateway URL enables the section and means the chat API: %+v", c.Studio.AI)
	}
}

func TestStudioAIDefaultIsOff(t *testing.T) {
	c := Default()
	if c.Studio.AI.Enabled() {
		t.Fatalf("no provider is configured by default: %+v", c.Studio.AI)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := (StudioAI{APIKeyFile: "/k"}).ResolvedAPI(); got != "" {
		t.Fatalf("OpenAI itself keeps Studio's own API choice, got %q", got)
	}
	if got := (StudioAI{APIKeyFile: "/k", API: "responses", BaseURL: "https://gw.example"}).ResolvedAPI(); got != "responses" {
		t.Fatalf("an explicit API wins, got %q", got)
	}
	if got := (StudioAI{Model: "m"}).ResolvedCompletionModel(); got != "m" {
		t.Fatalf("the completion model follows the model, got %q", got)
	}
}

func TestStudioAIValidate(t *testing.T) {
	good := []StudioAI{
		{},
		{Provider: "openai"},
		{APIKeyFile: "/etc/supavise/studio-ai.key"},
		{BaseURL: "http://127.0.0.1:11434/v1", Model: "llama3.1:8b", API: "chat"},
		{BaseURL: "https://gw.example.com/v1/", Model: "accounts/fireworks/models/llama-v3p1-70b-instruct", ReasoningEffort: "high"},
		{BaseURL: "https://ai-gateway.vercel.sh/v1", Model: "anthropic/claude-sonnet-4.5", CompletionModel: "openai/gpt-5-mini"},
	}
	for _, a := range good {
		if err := a.Validate(); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
	bad := map[string]StudioAI{
		"studio.ai.provider":         {Provider: "bedrock", APIKeyFile: "/k"},
		"must start with http":       {BaseURL: "ftp://gw.example.com"},
		"no host":                    {BaseURL: "https:///v1"},
		"user name or password":      {BaseURL: "https://user:pw@gw.example.com/v1"},
		"query or a fragment":        {BaseURL: "https://gw.example.com/v1?api-version=1"},
		"is not a URL":               {BaseURL: "https://gw.example.com/a b"},
		"absolute path":              {APIKeyFile: "relative/key"},
		"is not a model name":        {BaseURL: "https://gw.example.com", Model: "gpt 4"},
		"studio.ai.completion_model": {BaseURL: "https://gw.example.com", CompletionModel: "-x"},
		"studio.ai.api":              {BaseURL: "https://gw.example.com", API: "completions"},
		"reasoning_effort":           {BaseURL: "https://gw.example.com", ReasoningEffort: "extreme"},
		"but no provider":            {Model: "gpt-4o"},
	}
	for want, a := range bad {
		err := a.Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: error %v, want one containing %q", a, err, want)
		}
	}
	c := Default()
	c.Studio.AI = StudioAI{BaseURL: "ftp://x"}
	if err := c.Validate(); err == nil {
		t.Fatal("Config.Validate must check [studio.ai]")
	}
}

func TestStudioAIReadKey(t *testing.T) {
	const secret = "sk-test-0123456789abcdef"
	got, err := StudioAI{APIKeyFile: writeKey(t, "  "+secret+"\n", 0o600)}.ReadKey()
	if err != nil || got != secret {
		t.Fatalf("ReadKey = %q, %v", got, err)
	}
	if got, err := (StudioAI{APIKeyFile: writeKey(t, secret+"\n", 0o400)}).ReadKey(); err != nil || got != secret {
		t.Fatalf("a read-only file is fine: %q, %v", got, err)
	}
	if got, err := (StudioAI{}).ReadKey(); err != nil || got != "" {
		t.Fatalf("no file, no key: %q, %v", got, err)
	}

	for name, a := range map[string]StudioAI{
		"group-readable":   {APIKeyFile: writeKey(t, secret, 0o640)},
		"world-readable":   {APIKeyFile: writeKey(t, secret, 0o644)},
		"two lines":        {APIKeyFile: writeKey(t, secret+"\nsecond-line-"+secret, 0o600)},
		"with a space":     {APIKeyFile: writeKey(t, "sk test "+secret, 0o600)},
		"empty":            {APIKeyFile: writeKey(t, " \n", 0o600)},
		"too long":         {APIKeyFile: writeKey(t, strings.Repeat("k", aiKeyMaxLen+1), 0o600)},
		"missing":          {APIKeyFile: filepath.Join(t.TempDir(), "nope")},
		"a directory":      {APIKeyFile: t.TempDir()},
		"non-ASCII":        {APIKeyFile: writeKey(t, "sk-é"+secret, 0o600)},
		"a control byte":   {APIKeyFile: writeKey(t, "sk-\x01"+secret, 0o600)},
		"a NUL in the key": {APIKeyFile: writeKey(t, "sk-\x00"+secret, 0o600)},
	} {
		key, err := a.ReadKey()
		if err == nil {
			t.Errorf("%s: want an error", name)
			continue
		}
		if key != "" {
			t.Errorf("%s: returned a key together with the error", name)
		}
		// The message names the file and the problem; it never carries what the file holds.
		if strings.Contains(err.Error(), "0123456789abcdef") {
			t.Errorf("%s: the error repeats the key: %v", name, err)
		}
	}
}

func TestValidAIKey(t *testing.T) {
	for _, k := range []string{"sk-proj-AbC_123-xyz", "Bearer.like~token+/=", strings.Repeat("a", aiKeyMaxLen)} {
		if err := ValidAIKey(k); err != nil {
			t.Errorf("%q: %v", k, err)
		}
	}
	for _, k := range []string{"", "a b", "a\tb", "a\nb", "ключ", strings.Repeat("a", aiKeyMaxLen+1)} {
		if err := ValidAIKey(k); err == nil {
			t.Errorf("%q was accepted", k)
		}
	}
}
