package fleet

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
)

const testAIKey = "sk-test-0123456789abcdef"

func aiKeyFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "studio-ai.key")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStudioAIEnv(t *testing.T) {
	key := aiKeyFile(t, testAIKey+"\n")
	for name, tc := range map[string]struct {
		ai   config.StudioAI
		want map[string]string
	}{
		"off": {config.StudioAI{}, nil},
		"openai itself": {
			config.StudioAI{APIKeyFile: key},
			map[string]string{"OPENAI_API_KEY": testAIKey},
		},
		"a gateway": {
			config.StudioAI{APIKeyFile: key, BaseURL: "https://openrouter.ai/api/v1", Model: "openai/gpt-4o", CompletionModel: "openai/gpt-4o-mini", ReasoningEffort: "low"},
			map[string]string{
				"OPENAI_API_KEY": testAIKey, "OPENAI_BASE_URL": "https://openrouter.ai/api/v1", "SUPAVISE_AI_MODEL": "openai/gpt-4o",
				"SUPAVISE_AI_COMPLETION_MODEL": "openai/gpt-4o-mini", "SUPAVISE_AI_API": "chat", "SUPAVISE_AI_REASONING_EFFORT": "low",
			},
		},
		"the completion model follows the model, the API can be chosen": {
			config.StudioAI{APIKeyFile: key, BaseURL: "https://gw.example.com/v1", Model: "m", API: "responses"},
			map[string]string{
				"OPENAI_API_KEY": testAIKey, "OPENAI_BASE_URL": "https://gw.example.com/v1", "SUPAVISE_AI_MODEL": "m",
				"SUPAVISE_AI_COMPLETION_MODEL": "m", "SUPAVISE_AI_API": "responses",
			},
		},
		"a local Ollama takes no key": {
			config.StudioAI{BaseURL: "http://127.0.0.1:11434/v1", Model: "llama3.1:8b"},
			map[string]string{
				"OPENAI_API_KEY": "unused", "OPENAI_BASE_URL": "http://127.0.0.1:11434/v1", "SUPAVISE_AI_MODEL": "llama3.1:8b",
				"SUPAVISE_AI_COMPLETION_MODEL": "llama3.1:8b", "SUPAVISE_AI_API": "chat",
			},
		},
	} {
		got, err := StudioAIEnv(tc.ai)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !maps.Equal(got, tc.want) {
			t.Errorf("%s: env = %v, want %v", name, got, tc.want)
		}
		// The key is in OPENAI_API_KEY and nowhere else; an empty value is never rendered (the OpenAI
		// client refuses an empty OPENAI_BASE_URL).
		for k, v := range got {
			if v == "" {
				t.Errorf("%s: %s is rendered empty", name, k)
			}
			if k != "OPENAI_API_KEY" && strings.Contains(v, testAIKey) {
				t.Errorf("%s: the key is in %s", name, k)
			}
		}
	}
}

func TestStudioAIEnvRefusesWhatItCannotRender(t *testing.T) {
	for name, ai := range map[string]config.StudioAI{
		"a missing key file": {APIKeyFile: filepath.Join(t.TempDir(), "gone")},
		"a bad base URL":     {APIKeyFile: aiKeyFile(t, testAIKey), BaseURL: "gw.example.com"},
		"a bad model":        {APIKeyFile: aiKeyFile(t, testAIKey), Model: "two words"},
	} {
		if env, err := StudioAIEnv(ai); err == nil || env != nil {
			t.Errorf("%s: env %v, error %v", name, env, err)
		}
	}
}

// The unit of Studio carries the provider, a render without one drops it again, and a provider that
// cannot be read leaves the dashboard up without AI and says why in the log, never the key.
func TestStudioSpecCarriesTheAIProvider(t *testing.T) {
	var logs bytes.Buffer
	r := newManagerRig(t, func(d *Deps) {
		d.Log = slog.New(slog.NewTextHandler(&logs, nil))
	})
	studio := func() map[string]string {
		t.Helper()
		specs, err := r.m.Specs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range specs {
			if s.Service == config.SvcStudio {
				return s.Env
			}
		}
		t.Fatal("no Studio spec")
		return nil
	}

	if env := studio(); env["OPENAI_API_KEY"] != "" || env["OPENAI_BASE_URL"] != "" {
		t.Fatalf("no provider is configured, yet Studio gets %v", env)
	}

	key := aiKeyFile(t, testAIKey)
	r.n.cfg.Studio.AI = config.StudioAI{APIKeyFile: key, BaseURL: "https://gw.example.com/v1", Model: "m"}
	env := studio()
	if env["OPENAI_API_KEY"] != testAIKey || env["OPENAI_BASE_URL"] != "https://gw.example.com/v1" || env["SUPAVISE_AI_MODEL"] != "m" {
		t.Fatalf("Studio env = %v", env)
	}
	if env["NEXT_PUBLIC_API_URL"] == "" {
		t.Fatal("the AI variables replaced the rest of Studio's environment")
	}

	// The daemon hands in a reader of the files, so a change made while it runs is not undone.
	live := config.StudioAI{BaseURL: "http://127.0.0.1:11434/v1", Model: "llama3.1:8b"}
	r.m.d.StudioAI = func() config.StudioAI { return live }
	if env := studio(); env["OPENAI_BASE_URL"] != "http://127.0.0.1:11434/v1" || env["OPENAI_API_KEY"] != "unused" {
		t.Fatalf("Deps.StudioAI is not used: %v", env)
	}

	r.m.d.StudioAI = func() config.StudioAI { return config.StudioAI{APIKeyFile: filepath.Join(t.TempDir(), "gone")} }
	env = studio()
	if _, ok := env["OPENAI_API_KEY"]; ok {
		t.Fatalf("an unreadable key file still rendered a key: %v", env)
	}
	if !strings.Contains(logs.String(), "Studio starts without its AI provider") || strings.Contains(logs.String(), testAIKey) {
		t.Fatalf("log = %q", logs.String())
	}
}
