package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// StudioAI is the [studio.ai] section: the model provider behind Studio's AI Assistant and the AI
// features of the SQL editor. Studio's server calls an OpenAI-compatible API (OpenAI itself, or a
// gateway such as OpenRouter, LiteLLM, Vercel AI Gateway, an Azure OpenAI v1 endpoint or a local
// Ollama) with the key and URL set here; nothing is configured by default and the features then
// say that no provider is set.
//
// The key never sits in config.toml: api_key_file names a file that only its owner can read.
// Studio's unit cannot read /etc/supavise, so the daemon reads the file and hands the key to the
// unit in its environment file (internal/fleet, studio_ai.go).
type StudioAI struct {
	// Provider is "openai" (also the default): any API that speaks OpenAI's chat completions or
	// responses protocol. Studio supports no other provider outside Supabase's own hosting.
	Provider string `toml:"provider"`
	// BaseURL is the root of the API, for example https://openrouter.ai/api/v1 or
	// http://127.0.0.1:11434/v1. Empty means OpenAI's own endpoint.
	BaseURL string `toml:"base_url"`
	// APIKeyFile is the absolute path of a file with the key on one line, mode 0600 or 0400. Empty
	// sends the placeholder key "unused", for a gateway that takes none (a local Ollama).
	APIKeyFile string `toml:"api_key_file"`
	// Model is the model the Assistant's chat uses. Empty keeps Studio's own pick, which names
	// OpenAI models and is right only for OpenAI itself.
	Model string `toml:"model"`
	// CompletionModel is the model of the small calls: titles, filters, cron and policy
	// suggestions, the SQL editor's inline completion. Empty means Model.
	CompletionModel string `toml:"completion_model"`
	// API is "chat" (the chat completions API, which every gateway offers), "responses" (OpenAI's
	// newer API) or empty: chat with a base_url, responses without.
	API string `toml:"api"`
	// ReasoningEffort is sent with each request when set: none, minimal, low, medium, high, xhigh
	// or max. Empty sends none, which is right for models that do not reason.
	ReasoningEffort string `toml:"reasoning_effort"`
}

// KeylessPlaceholder is the key Studio is given when no api_key_file is set. The OpenAI client
// refuses to start without one, and a gateway that takes none ignores it.
const KeylessPlaceholder = "unused"

// Enabled reports whether Studio's AI features have a provider: a key file, a gateway URL or both.
func (a StudioAI) Enabled() bool { return a.BaseURL != "" || a.APIKeyFile != "" }

// GatewayWithoutModel reports a base_url other than OpenAI's with no model. Studio then sends its
// own OpenAI model names to the gateway, which most gateways do not serve: every Assistant message
// fails. A gateway that maps those names (a LiteLLM alias, an Azure deployment of that name) is a
// legitimate setup, so this is a reason to warn and not to refuse.
func (a StudioAI) GatewayWithoutModel() bool {
	if a.BaseURL == "" || a.Model != "" {
		return false
	}
	u, err := url.Parse(a.BaseURL)
	return err == nil && !strings.EqualFold(u.Hostname(), "api.openai.com")
}

// ResolvedAPI is the API Studio uses: the setting, else chat for a gateway and responses (what an
// empty answer means to Studio) for OpenAI itself.
func (a StudioAI) ResolvedAPI() string {
	if a.API != "" {
		return a.API
	}
	if a.BaseURL != "" {
		return "chat"
	}
	return ""
}

// ResolvedCompletionModel is CompletionModel, else Model.
func (a StudioAI) ResolvedCompletionModel() string {
	if a.CompletionModel != "" {
		return a.CompletionModel
	}
	return a.Model
}

const aiKeyMaxLen = 4096

var (
	aiModelRE = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9._:/@+-]{0,199}$`)
	aiEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
)

// Validate checks the section and returns the first problem, naming the key.
func (a StudioAI) Validate() error {
	switch a.Provider {
	case "", "openai":
	default:
		return fmt.Errorf("config: studio.ai.provider %q must be \"openai\": Studio's AI features support OpenAI and APIs that speak its protocol", a.Provider)
	}
	if a.BaseURL != "" {
		if err := validateAIBaseURL(a.BaseURL); err != nil {
			return err
		}
	}
	if a.APIKeyFile != "" {
		if !filepath.IsAbs(a.APIKeyFile) || strings.ContainsAny(a.APIKeyFile, "\x00\r\n") {
			return fmt.Errorf("config: studio.ai.api_key_file %q must be an absolute path", a.APIKeyFile)
		}
	}
	for key, m := range map[string]string{"model": a.Model, "completion_model": a.CompletionModel} {
		if m != "" && !aiModelRE.MatchString(m) {
			return fmt.Errorf("config: studio.ai.%s %q is not a model name (letters, digits and . _ : / @ + -, at most 200 characters)", key, m)
		}
	}
	switch a.API {
	case "", "chat", "responses":
	default:
		return fmt.Errorf("config: studio.ai.api %q must be \"chat\" or \"responses\"", a.API)
	}
	if a.ReasoningEffort != "" {
		ok := false
		for _, e := range aiEfforts {
			ok = ok || a.ReasoningEffort == e
		}
		if !ok {
			return fmt.Errorf("config: studio.ai.reasoning_effort %q must be one of %s", a.ReasoningEffort, strings.Join(aiEfforts, ", "))
		}
	}
	if !a.Enabled() && (a.Model != "" || a.CompletionModel != "" || a.API != "" || a.ReasoningEffort != "") {
		return errors.New("config: studio.ai names a model, API or reasoning effort but no provider: set base_url or api_key_file too")
	}
	return nil
}

func validateAIBaseURL(s string) error {
	u, err := url.Parse(s)
	switch {
	case err != nil || len(s) > 2048 || strings.ContainsAny(s, " \t\r\n\x00"):
		return fmt.Errorf("config: studio.ai.base_url is not a URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("config: studio.ai.base_url must start with http:// or https://")
	case u.Hostname() == "":
		return errors.New("config: studio.ai.base_url has no host")
	case u.User != nil:
		// Credentials in a URL end up in logs and error messages; the key goes in api_key_file.
		return errors.New("config: studio.ai.base_url must not carry a user name or password: put the key in api_key_file")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return errors.New("config: studio.ai.base_url must not have a query or a fragment: Studio appends the API path to it")
	}
	return nil
}

// ValidAIKey reports why key cannot be used as an API key, or nil. The message never holds the key.
func ValidAIKey(key string) error {
	switch {
	case key == "":
		return errors.New("the key is empty")
	case len(key) > aiKeyMaxLen:
		return fmt.Errorf("the key is longer than %d characters", aiKeyMaxLen)
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return errors.New("the key must be one line of printable ASCII characters without spaces")
		}
	}
	return nil
}

// ReadKey returns the key in api_key_file, or "" when none is set. The file must be a regular file
// that group and others cannot access (chmod 600), and hold the key on one line; surrounding
// whitespace is dropped. An error names the file and the problem, never the key.
func (a StudioAI) ReadKey() (string, error) {
	if a.APIKeyFile == "" {
		return "", nil
	}
	f, err := os.Open(a.APIKeyFile)
	if err != nil {
		return "", fmt.Errorf("studio.ai.api_key_file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("studio.ai.api_key_file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("studio.ai.api_key_file %s is not a regular file", a.APIKeyFile)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("studio.ai.api_key_file %s is accessible to group or others (mode %04o): chmod 600 it", a.APIKeyFile, fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(aiKeyMaxLen)+1024))
	if err != nil {
		return "", fmt.Errorf("studio.ai.api_key_file: %w", err)
	}
	key := strings.TrimSpace(string(b))
	if err := ValidAIKey(key); err != nil {
		return "", fmt.Errorf("studio.ai.api_key_file %s: %w", a.APIKeyFile, err)
	}
	return key, nil
}
