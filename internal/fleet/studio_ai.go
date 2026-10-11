package fleet

import "github.com/supavise/supavise/internal/config"

// The variables that point Studio's AI features at the operator's provider (studio/README.md,
// patch 0006). OPENAI_API_KEY and OPENAI_BASE_URL are the ones the OpenAI client reads itself; the
// SUPAVISE_AI_* ones are read by the patch, per request on the server, and never reach the browser.
const (
	envAIKey             = "OPENAI_API_KEY"
	envAIBaseURL         = "OPENAI_BASE_URL"
	envAIModel           = "SUPAVISE_AI_MODEL"
	envAICompletionModel = "SUPAVISE_AI_COMPLETION_MODEL"
	envAIAPI             = "SUPAVISE_AI_API"
	envAIEffort          = "SUPAVISE_AI_REASONING_EFFORT"
)

// studioAIEnv is the environment of Studio's unit for the [studio.ai] settings ai, or nil when no
// provider is configured, so that Studio answers "API key not set" instead of failing a request.
// The key is read from api_key_file here, by the daemon: Studio's unit cannot read /etc/supavise,
// and the key reaches it only through the unit's environment file (mode 0600). A section that does
// not validate or a key file that cannot be read is an error and renders nothing.
func studioAIEnv(ai config.StudioAI) (map[string]string, error) {
	if !ai.Enabled() {
		return nil, nil
	}
	if err := ai.Validate(); err != nil {
		return nil, err
	}
	key, err := ai.ReadKey()
	if err != nil {
		return nil, err
	}
	if key == "" {
		key = config.KeylessPlaceholder
	}
	env := map[string]string{envAIKey: key}
	for name, v := range map[string]string{
		envAIBaseURL:         ai.BaseURL,
		envAIModel:           ai.Model,
		envAICompletionModel: ai.ResolvedCompletionModel(),
		envAIAPI:             ai.ResolvedAPI(),
		envAIEffort:          ai.ReasoningEffort,
	} {
		// An empty OPENAI_BASE_URL is an error in the OpenAI client, so unset values are left out.
		if v != "" {
			env[name] = v
		}
	}
	return env, nil
}
