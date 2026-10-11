package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/units"
)

// studioAIDropIn is the file under config.d that `supavise studio ai set` and `clear` own. It holds
// every key of [studio.ai], empty ones included, so that it replaces whatever config.toml says.
const studioAIDropIn = "40-studio-ai.toml"

// studioAIKeyFile is the key file the command keeps next to config.toml. A key given to `set` is
// copied here (mode 0600, owned by the supavise user), so the daemon can read it whatever the
// permissions of the file the operator pointed at, and nothing needs the original again.
const studioAIKeyFile = "studio-ai.key"

func init() { rootCmd.AddCommand(newStudioCmd()) }

// newStudioCmd builds `supavise studio`. Tests build their own, because a command remembers the
// flags it was run with.
func newStudioCmd() *cobra.Command {
	studioCmd := &cobra.Command{
		Use:   "studio",
		Short: "Settings of the dashboard",
	}
	ai := &cobra.Command{
		Use:   "ai",
		Short: "Choose the model provider of Studio's AI Assistant and AI features",
		Long: `Studio's AI Assistant and the AI features of the SQL editor call a model provider, and a server
has none until you set one. The provider is any API that speaks OpenAI's protocol: OpenAI itself or a
gateway such as OpenRouter, LiteLLM, Vercel AI Gateway, an Azure OpenAI v1 endpoint or a local Ollama.
The key stays on this server: it goes to Studio's process and to the provider you name.

  sudo -u supavise supavise studio ai set --base-url https://openrouter.ai/api/v1 \
      --api-key-stdin --model openai/gpt-4o-mini <key.txt
  sudo -u supavise supavise studio ai show
  sudo -u supavise supavise studio ai check
  sudo -u supavise supavise studio ai clear

The settings live in the [studio.ai] section of /etc/supavise/config.toml, and the commands write them
to config.d/` + studioAIDropIn + `. The key is never in config.toml: [studio.ai] api_key_file names a file only its
owner can read.`,
	}

	var f studioAIFlags
	set := &cobra.Command{
		Use:   "set",
		Short: "Set the provider and restart Studio",
		Long: `Sets the provider and restarts Studio when its environment changed. Only the options you give
change; the others keep their value, so ` + "`set --model llama3.1`" + ` after a first ` + "`set`" + ` changes the model alone.

  --base-url          the root of the API (https://openrouter.ai/api/v1, http://127.0.0.1:11434/v1).
                      Empty means OpenAI's own endpoint.
  --api-key-file F    read the key from file F (one line); the command keeps its own copy at
                      /etc/supavise/` + studioAIKeyFile + ` (mode 0600). Use --api-key-stdin to pipe the key in.
  --no-api-key        send no key, for a gateway that needs none (a local Ollama)
  --model             the model of the Assistant's chat. Without one Studio uses OpenAI's names, which
                      suits only OpenAI itself: with another --base-url, name the model (set warns
                      when you do not). A name may start with @ (Cloudflare Workers AI).
  --completion-model  the model of the small calls (titles, filters, inline completion); default --model
  --api               "chat" (chat completions, which every gateway offers; the default with a --base-url)
                      or "responses" (OpenAI's newer API)
  --reasoning-effort  none, minimal, low, medium, high, xhigh or max; sent with every request when set

To keep a key somewhere else (a mounted secret), set api_key_file in config.toml yourself: an absolute path,
a regular file, mode 0600 or 0400, readable by the supavise user.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStudioAISet(cmd, f)
		},
	}
	sf := set.Flags()
	sf.StringVar(&f.baseURL, "base-url", "", "root URL of the OpenAI-compatible API")
	sf.StringVar(&f.keyFile, "api-key-file", "", "file with the API key on one line")
	sf.BoolVar(&f.keyStdin, "api-key-stdin", false, "read the API key from standard input")
	sf.BoolVar(&f.noKey, "no-api-key", false, "send no key (the gateway needs none)")
	sf.StringVar(&f.model, "model", "", "model of the Assistant's chat")
	sf.StringVar(&f.completionModel, "completion-model", "", "model of the small calls (default: --model)")
	sf.StringVar(&f.api, "api", "", `"chat" or "responses"`)
	sf.StringVar(&f.effort, "reasoning-effort", "", "reasoning effort sent with every request")
	sf.BoolVar(&f.noRestart, "no-restart", false, "write the settings and leave Studio as it is")
	set.MarkFlagsMutuallyExclusive("api-key-file", "api-key-stdin", "no-api-key")

	var asJSON bool
	show := &cobra.Command{
		Use:   "show",
		Short: "Show the provider, without the key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return printStudioAI(cmd.OutOrStdout(), studioAIView(cfg), asJSON)
		},
	}
	show.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	check := &cobra.Command{
		Use:   "check",
		Short: "Ask the provider for its model list with the configured key",
		Long: `Sends one request, GET <base_url>/models with the key, from this server, and says what came back.
It proves the URL, the key and the network path; it does not prove that the model names exist. Studio's
assistant shows a generic error when the provider refuses a request, so run this first when it fails.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return checkStudioAI(cmd.Context(), cmd.OutOrStdout(), cfg.Studio.AI, http.DefaultTransport)
		},
	}

	var noRestart bool
	clearCmd := &cobra.Command{
		Use:   "clear",
		Short: "Remove the provider and restart Studio",
		Long: `Removes the provider and the key file this command keeps. The AI Assistant then says no provider
is set. A [studio.ai] section in config.toml is overridden, not deleted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStudioAIClear(cmd, noRestart)
		},
	}
	clearCmd.Flags().BoolVar(&noRestart, "no-restart", false, "write the settings and leave Studio as it is")

	ai.AddCommand(set, show, check, clearCmd)
	studioCmd.AddCommand(ai)
	return studioCmd
}

type studioAIFlags struct {
	baseURL, keyFile, model, completionModel, api, effort string
	keyStdin, noKey, noRestart                            bool
}

// studioAIStore reads and writes the files of the settings next to the config file at path.
type studioAIStore struct{ path string }

func (s studioAIStore) dir() string { return filepath.Dir(config.ResolvePath(s.path)) }
func (s studioAIStore) dropIn() string {
	return filepath.Join(config.ConfigDDir(s.path), studioAIDropIn)
}
func (s studioAIStore) keyPath() string { return filepath.Join(s.dir(), studioAIKeyFile) }

// current is the [studio.ai] section as the files and this process's environment say.
func (s studioAIStore) current() (config.StudioAI, error) {
	cfg, err := config.Load(s.path)
	if err != nil {
		return config.StudioAI{}, err
	}
	return cfg.Studio.AI, nil
}

// save writes ai to the drop-in. key, when not empty, becomes the key file first (ai.APIKeyFile
// must name it); dropKey removes the key file afterwards.
func (s studioAIStore) save(ai config.StudioAI, key string, dropKey bool) error {
	uid, gid := supaviseOwner()
	if key != "" {
		if err := writeFileAtomic(s.keyPath(), []byte(key+"\n"), 0o600, uid, gid); err != nil {
			return fmt.Errorf("writing the key file: %w", err)
		}
	}
	body, err := studioAIDropInBody(ai)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.dropIn(), body, 0o600, uid, gid); err != nil {
		return fmt.Errorf("writing %s: %w", s.dropIn(), err)
	}
	if dropKey {
		if err := os.Remove(s.keyPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing the key file: %w", err)
		}
	}
	return nil
}

func studioAIDropInBody(ai config.StudioAI) ([]byte, error) {
	b, err := toml.Marshal(map[string]any{"studio": map[string]any{"ai": ai}})
	if err != nil {
		return nil, err
	}
	return append([]byte("# Written by `supavise studio ai set` and `clear`, which replace this file. It holds no key:\n# api_key_file names the file that does.\n"), b...), nil
}

func runStudioAISet(cmd *cobra.Command, f studioAIFlags) error {
	store := studioAIStore{path: selfUpdateConfigPath()}
	ai, err := store.current()
	if err != nil {
		return err
	}
	key, err := readNewAIKey(cmd, f)
	if err != nil {
		return err
	}
	ai = applyStudioAIFlags(cmd, ai, f, key != "", store.keyPath())
	if !ai.Enabled() {
		return errors.New("nothing to set: give --base-url or a key (--api-key-file, --api-key-stdin), or run `supavise studio ai clear`")
	}
	if ai.Provider == "" {
		ai.Provider = "openai"
	}
	if err := ai.Validate(); err != nil {
		return err
	}
	if key == "" && ai.APIKeyFile != "" {
		if _, err := ai.ReadKey(); err != nil { // the key file that stays must be one the daemon can use
			return err
		}
	}
	if err := store.save(ai, key, f.noKey && ai.APIKeyFile == ""); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Saved the provider in %s.\n", store.dropIn())
	warnAIEnvironment(cmd.ErrOrStderr())
	warnAIGatewayModel(cmd.ErrOrStderr(), ai)
	if f.noRestart {
		fmt.Fprintln(out, "Studio was not restarted (--no-restart): run `supavise fleet start` to apply the settings.")
		return nil
	}
	return refreshStudioAI(cmd)
}

func runStudioAIClear(cmd *cobra.Command, noRestart bool) error {
	store := studioAIStore{path: selfUpdateConfigPath()}
	if err := store.save(config.StudioAI{}, "", true); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Removed the provider (%s) and the key file.\n", store.dropIn())
	warnAIEnvironment(cmd.ErrOrStderr())
	if noRestart {
		fmt.Fprintln(out, "Studio was not restarted (--no-restart): run `supavise fleet start` to apply the settings.")
		return nil
	}
	return refreshStudioAI(cmd)
}

// readNewAIKey returns the key given with --api-key-file or --api-key-stdin, or "".
func readNewAIKey(cmd *cobra.Command, f studioAIFlags) (string, error) {
	var r io.Reader
	switch {
	case f.keyStdin:
		r = cmd.InOrStdin()
	case f.keyFile != "":
		fh, err := os.Open(f.keyFile)
		if err != nil {
			return "", err
		}
		defer fh.Close()
		r = fh
	default:
		return "", nil
	}
	b, err := io.ReadAll(io.LimitReader(r, 8192))
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(b))
	if err := config.ValidAIKey(key); err != nil {
		return "", fmt.Errorf("the API key you gave is not usable: %w", err)
	}
	return key, nil
}

// applyStudioAIFlags returns ai with the options that were given changed. gotKey says a new key
// was read; keyPath is where the command keeps it.
func applyStudioAIFlags(cmd *cobra.Command, ai config.StudioAI, f studioAIFlags, gotKey bool, keyPath string) config.StudioAI {
	fl := cmd.Flags()
	if fl.Changed("base-url") {
		ai.BaseURL = f.baseURL
	}
	if fl.Changed("model") {
		ai.Model = f.model
	}
	if fl.Changed("completion-model") {
		ai.CompletionModel = f.completionModel
	}
	if fl.Changed("api") {
		ai.API = f.api
	}
	if fl.Changed("reasoning-effort") {
		ai.ReasoningEffort = f.effort
	}
	switch {
	case gotKey:
		ai.APIKeyFile = keyPath
	case f.noKey:
		ai.APIKeyFile = ""
	}
	return ai
}

// warnAIGatewayModel says when the gateway has no model to answer to: Studio then sends its own
// OpenAI model names, which most gateways do not serve.
func warnAIGatewayModel(w io.Writer, ai config.StudioAI) {
	if ai.GatewayWithoutModel() {
		fmt.Fprintf(w, "warning: %s is not OpenAI and no model is set, so Studio asks it for Studio's own OpenAI model names, which most gateways do not serve: every Assistant message would fail. Name its model with `supavise studio ai set --model NAME`, unless the gateway maps those names.\n", ai.BaseURL)
	}
}

// warnAIEnvironment says when a SUPAVISE_STUDIO_AI_* variable of this process overrides the files.
func warnAIEnvironment(w io.Writer) {
	var names []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "SUPAVISE_STUDIO_AI_") {
			names = append(names, k)
		}
	}
	if len(names) > 0 {
		sort.Strings(names)
		fmt.Fprintf(w, "warning: %s override the settings in the files, here and in any process that has them\n", strings.Join(names, ", "))
	}
}

// refreshStudioAI renders Studio's unit again from the files and restarts it when its environment
// changed. The daemon reads the files again for each render, so it needs no restart. A variable, so
// that a test can stand in for the node.
var refreshStudioAI = func(cmd *cobra.Command) error {
	out, errw := cmd.OutOrStdout(), cmd.ErrOrStderr()
	n, err := openNode(cmd.Context())
	if err != nil {
		fmt.Fprintf(errw, "warning: Studio was not updated (%v); run `supavise fleet start` to apply the settings\n", err)
		return nil
	}
	defer n.Close()
	fm, err := fleet.NewManager(fleet.Deps{Cfg: n.Cfg, Log: newLogger(n.Cfg), Registry: n.Registry, Secrets: n.Secrets, Supervisor: n.Supervisor, Artifacts: n.Artifacts})
	if err == nil {
		fmt.Fprintln(out, "Updating Studio...")
		err = fm.RefreshStudio(cmd.Context())
	}
	if err != nil {
		fmt.Fprintf(errw, "warning: Studio was not updated (%v); run `supavise fleet start` to apply the settings\n", err)
		return nil
	}
	fmt.Fprintln(out, "Studio runs with the new settings if this server runs Studio (a server that follows another does not, and a node without a Studio build has none to restart). On a cluster, run the same command on the other server too: the settings belong to the server, and the one that leads after a failover needs them.")
	return nil
}

// ---- show ---------------------------------------------------------------------------------

// studioAIReport is what `show` prints. It holds no key.
type studioAIReport struct {
	Configured      bool   `json:"configured"`
	Provider        string `json:"provider"`
	BaseURL         string `json:"base_url"`
	APIKey          string `json:"api_key"`
	APIKeyFile      string `json:"api_key_file,omitempty"`
	Model           string `json:"model,omitempty"`
	CompletionModel string `json:"completion_model,omitempty"`
	API             string `json:"api,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// Studio says whether the unit of Studio on this node was rendered from these settings.
	Studio string `json:"studio"`
}

func studioAIView(cfg *config.Config) studioAIReport {
	ai := cfg.Studio.AI
	r := studioAIReport{
		Configured: ai.Enabled(), Provider: "openai", BaseURL: ai.BaseURL, APIKeyFile: ai.APIKeyFile,
		Model: ai.Model, CompletionModel: ai.ResolvedCompletionModel(), API: ai.ResolvedAPI(), ReasoningEffort: ai.ReasoningEffort,
	}
	if ai.Provider != "" {
		r.Provider = ai.Provider
	}
	if r.BaseURL == "" {
		r.BaseURL = "https://api.openai.com/v1 (default)"
	}
	if r.API == "" {
		r.API = "responses (default)"
	}
	switch {
	case !ai.Enabled():
		r.APIKey = "not set"
	case ai.APIKeyFile == "":
		r.APIKey = "none: the provider is called without a key"
	default:
		if _, err := ai.ReadKey(); err != nil {
			r.APIKey = "set but unusable: " + err.Error()
		} else {
			r.APIKey = "set (not shown)"
		}
	}
	r.Studio = studioAIApplied(cfg, ai)
	return r
}

// studioAIApplied compares the environment file of Studio's unit with what the settings render.
func studioAIApplied(cfg *config.Config, ai config.StudioAI) string {
	want, err := fleet.StudioAIEnv(ai)
	if err != nil {
		return "runs without the provider: " + err.Error()
	}
	b, err := os.ReadFile(units.FilesFor(cfg, units.Spec{Service: config.SvcStudio}).Env)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "not rendered on this node"
	case err != nil:
		return "unknown (the unit's environment file is not readable by this user)"
	}
	have, err := units.ParseEnv(b)
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	for _, k := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "SUPAVISE_AI_MODEL", "SUPAVISE_AI_COMPLETION_MODEL", "SUPAVISE_AI_API", "SUPAVISE_AI_REASONING_EFFORT"} {
		if have[k] != want[k] {
			return "not applied yet: run `supavise studio ai set` again or `supavise fleet start`"
		}
	}
	return "applied"
}

func printStudioAI(w io.Writer, r studioAIReport, asJSON bool) error {
	if asJSON {
		return printJSON(w, r)
	}
	if !r.Configured {
		fmt.Fprintln(w, "No AI provider is set: Studio's AI Assistant says that no API key is set.")
		fmt.Fprintln(w, "Set one with `supavise studio ai set --help`.")
		fmt.Fprintf(w, "Studio:            %s\n", r.Studio)
		return nil
	}
	rows := [][2]string{
		{"Provider", r.Provider}, {"Base URL", r.BaseURL}, {"API key", r.APIKey}, {"API key file", r.APIKeyFile},
		{"Model", orDefault(r.Model, "Studio's own (OpenAI names)")}, {"Completion model", orDefault(r.CompletionModel, "Studio's own (OpenAI names)")},
		{"API", r.API}, {"Reasoning effort", orDefault(r.ReasoningEffort, "not sent")}, {"Studio", r.Studio},
	}
	for _, row := range rows {
		if row[1] != "" {
			fmt.Fprintf(w, "%-18s %s\n", row[0]+":", row[1])
		}
	}
	return nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ---- check --------------------------------------------------------------------------------

// checkStudioAI asks the provider for GET <base_url>/models with the key and reports the answer.
// A non-2xx answer or a failure to connect is an error with a hint; the key is never printed.
func checkStudioAI(ctx context.Context, w io.Writer, ai config.StudioAI, rt http.RoundTripper) error {
	if !ai.Enabled() {
		return errors.New("no AI provider is set: see `supavise studio ai set --help`")
	}
	if err := ai.Validate(); err != nil {
		return err
	}
	key, err := ai.ReadKey()
	if err != nil {
		return err
	}
	base := strings.TrimRight(orDefault(ai.BaseURL, "https://api.openai.com/v1"), "/")
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s/models: %s\n  Check the URL, DNS and the firewall of this server.", base, redact(err.Error(), key))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		hint := "The provider refused the request."
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			hint = "The provider does not accept the key."
		case http.StatusNotFound:
			hint = "The provider has no such path: many gateways need /v1 at the end of the base URL."
		case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			hint = "The provider redirects (to " + redact(resp.Header.Get("Location"), key) + "): use that address as the base URL."
		}
		snippet := strings.TrimSpace(redact(string(body), key))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		return fmt.Errorf("GET %s/models answered %s. %s\n  %s", base, resp.Status, hint, snippet)
	}
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &list) == nil && list.Data != nil {
		fmt.Fprintf(w, "GET %s/models answered %s and lists %d models.\n", base, resp.Status, len(list.Data))
	} else {
		fmt.Fprintf(w, "GET %s/models answered %s.\n", base, resp.Status)
	}
	if ai.Model != "" {
		fmt.Fprintf(w, "The model %q is not checked: ask the Assistant a question to see whether the provider serves it.\n", ai.Model)
	}
	warnAIGatewayModel(w, ai)
	return nil
}

func redact(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "***")
}
