package github

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/supavise/supavise/internal/projectconfig"
)

// config.toml, the Supabase CLI's project file, applied to a branch. The CLI pushes it with
// `supabase config push`; this maps the same sections onto the settings a project saves
// (internal/projectconfig), so that the dashboard shows what the repository says.
//
// Rules, from hosted's documentation of the integration:
//
//   - The top-level values apply to every preview branch. A persistent branch is configured by the
//     [remotes.<name>] block whose project_id is the branch's ref, laid over the top-level values;
//     without such a block the configure step of a persistent branch is skipped.
//   - env(NAME) reads the branch's Edge Function secret NAME. A value that cannot be read is not
//     applied, and the log says so.
//   - Nothing is dropped silently: every key that is not applied is named in the log.

// ConfigPlan is what a config.toml asks of a branch.
type ConfigPlan struct {
	// Patches are the settings to save, by service, in the keys of internal/projectconfig.
	Patches map[projectconfig.Service]map[string]any
	// Applied lists the config.toml keys that were mapped, for the log.
	Applied []string
	// Warnings are the keys that were not applied, and why.
	Warnings []string
	// Buckets are the [storage.buckets.*] tables.
	Buckets []BucketSpec
	// Functions are the [functions.*] tables by slug.
	Functions map[string]FunctionConfig
	// Seed is [db.seed]; SeedSet is whether the table was there.
	Seed SeedConfig
	// Templates are the files the email templates name, relative to supabase/.
	Templates []string
	// Skipped says why the settings of the file are not applied at all ("" when they are): a
	// persistent branch with no matching [remotes] block. Functions, Seed and Buckets still come
	// from the top-level values.
	Skipped string
}

// BucketSpec is a [storage.buckets.<name>] table.
type BucketSpec struct {
	Name             string
	Public           bool
	FileSizeLimit    int64 // bytes, 0 for none
	AllowedMIMETypes []string
	ObjectsPath      string
}

// FunctionConfig is a [functions.<slug>] table.
type FunctionConfig struct {
	Enabled    *bool
	VerifyJWT  *bool
	Entrypoint string
	ImportMap  string
	Static     []string
}

// SeedConfig is [db.seed].
type SeedConfig struct {
	// Enabled is nil when the file does not say; the default is on.
	Enabled  *bool
	SQLPaths []string
}

// EnvLookup resolves env(NAME): the value of the branch's secret NAME.
type EnvLookup func(name string) (value string, ok bool, err error)

// PlanInput is what planConfig needs besides the document.
type PlanInput struct {
	Doc map[string]any
	// Ref is the branch's project ref, matched against [remotes.*] project_id.
	Ref        string
	Persistent bool
	// Branch is true when Ref is a branch and not the project the repository is connected to. A
	// branch starts on its parent's saved settings, less the credentials, and its owner can give it
	// secrets of its own, so the repository must not be able to send those somewhere else
	// (dropInheritedSecrets).
	Branch bool
	Env    EnvLookup
	// Held reports whether the project Ref has a value saved for a setting (a secret counts when one is
	// sealed). A branch plan asks it before it leaves a switch out for want of a secret
	// (holdBackSwitchesWithoutSecrets); nil means the branch holds none.
	Held func(svc projectconfig.Service, key string) (bool, error)
	// Template returns the content of a file named in the config (relative to supabase/).
	Template func(rel string) (string, bool)
}

// ParseConfig parses config.toml.
func ParseConfig(b []byte) (map[string]any, error) {
	doc := map[string]any{}
	if err := toml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// effectiveConfig lays the matching [remotes.<name>] block over the top-level values and returns
// the block's name ("" when none matched).
func effectiveConfig(doc map[string]any, ref string) (map[string]any, string) {
	remotes, _ := doc["remotes"].(map[string]any)
	var name string
	var match map[string]any
	var names []string
	for n := range remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t, ok := remotes[n].(map[string]any)
		if !ok {
			continue
		}
		if id, _ := t["project_id"].(string); id == ref && ref != "" {
			name, match = n, t
			break
		}
	}
	base := map[string]any{}
	for k, v := range doc {
		if k != "remotes" && k != "project_id" {
			base[k] = v
		}
	}
	if match == nil {
		return base, ""
	}
	over := map[string]any{}
	for k, v := range match {
		if k != "project_id" {
			over[k] = v
		}
	}
	return deepMerge(base, over), name
}

func deepMerge(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if bm, ok := v.(map[string]any); ok {
			if am, ok := out[k].(map[string]any); ok {
				out[k] = deepMerge(am, bm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// planner holds the state of one mapping.
type planner struct {
	in       PlanInput
	doc      map[string]any
	consumed map[string]bool
	plan     *ConfigPlan
}

func (p *planner) warn(format string, a ...any) {
	p.plan.Warnings = append(p.plan.Warnings, fmt.Sprintf(format, a...))
}

// get returns the value at the dotted path and marks it (and, for a table, everything under it)
// as handled.
func (p *planner) get(path string) (any, bool) {
	v, ok := lookup(p.doc, strings.Split(path, "."))
	if ok {
		p.consumed[path] = true
	}
	return v, ok
}

func lookup(doc map[string]any, parts []string) (any, bool) {
	var cur any = doc
	for _, k := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

var envRE = regexp.MustCompile(`^env\(\s*([A-Za-z_][A-Za-z0-9_]*)\s*\)$`)

// resolve replaces env(NAME) in a string. skip is true when the value cannot be had; the warning
// is already recorded.
func (p *planner) resolve(path string, v any) (out any, skip bool) {
	s, ok := v.(string)
	if !ok {
		return v, false
	}
	if strings.HasPrefix(s, "encrypted:") {
		p.warn("%s is an encrypted: value, which Supavise cannot decrypt; it is not applied (set the secret on the branch and use env())", path)
		return nil, true
	}
	m := envRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return v, false
	}
	if p.in.Env == nil {
		p.warn("%s is env(%s), which has nothing to read it from here; it is not applied", path, m[1])
		return nil, true
	}
	val, ok, err := p.in.Env(m[1])
	switch {
	case err != nil:
		p.warn("%s is env(%s), which could not be read (%v); it is not applied", path, m[1], err)
		return nil, true
	case !ok || val == "":
		p.warn("%s is env(%s), but the branch has no secret %s (supabase secrets set --project-ref <branch>); it is not applied", path, m[1], m[1])
		return nil, true
	}
	return val, false
}

// conv turns a config.toml value into a setting's value.
type conv func(v any) (any, error)

func asBool(v any) (any, error) {
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("want true or false")
	}
	return b, nil
}

func asNotBool(v any) (any, error) {
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("want true or false")
	}
	return !b, nil
}

func asInt(v any) (any, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case float64:
		if n == math.Trunc(n) {
			return int64(n), nil
		}
	}
	return nil, fmt.Errorf("want a whole number")
}

func asString(v any) (any, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("want a string")
	}
	return s, nil
}

// asNumberString is for a setting that is a string but is written as a number (smtp port).
func asNumberString(v any) (any, error) {
	switch n := v.(type) {
	case int64:
		return strconv.FormatInt(n, 10), nil
	case string:
		return n, nil
	}
	return nil, fmt.Errorf("want a port number")
}

func asList(v any) (any, error) {
	l, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("want a list of strings")
	}
	parts := make([]string, 0, len(l))
	for _, e := range l {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("want a list of strings")
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ","), nil
}

// durationSeconds accepts a number of seconds or a Go duration ("1m0s").
func durationSeconds(v any) (any, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case string:
		d, err := time.ParseDuration(n)
		if err != nil {
			return nil, fmt.Errorf("want seconds or a duration like 1h")
		}
		return int64(d / time.Second), nil
	}
	return nil, fmt.Errorf("want seconds or a duration like 1h")
}

// durationHours is a duration in hours as a number (the sessions settings).
func durationHours(v any) (any, error) {
	switch n := v.(type) {
	case int64:
		return float64(n), nil
	case float64:
		return n, nil
	case string:
		d, err := time.ParseDuration(n)
		if err != nil {
			return nil, fmt.Errorf("want a duration like 24h")
		}
		return d.Hours(), nil
	}
	return nil, fmt.Errorf("want a duration like 24h")
}

var passwordReq = map[string]string{
	"":                                   "",
	"letters_digits":                     "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ:0123456789",
	"lower_upper_letters_digits":         "abcdefghijklmnopqrstuvwxyz:ABCDEFGHIJKLMNOPQRSTUVWXYZ:0123456789",
	"lower_upper_letters_digits_symbols": "abcdefghijklmnopqrstuvwxyz:ABCDEFGHIJKLMNOPQRSTUVWXYZ:0123456789:!@#$%^&*()_+-=[]{};'\\\\:\"|<>?,./`~",
}

func asPasswordReq(v any) (any, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("want letters_digits, lower_upper_letters_digits or lower_upper_letters_digits_symbols")
	}
	r, ok := passwordReq[s]
	if !ok {
		return nil, fmt.Errorf("want letters_digits, lower_upper_letters_digits or lower_upper_letters_digits_symbols")
	}
	return r, nil
}

var sizeRE = regexp.MustCompile(`^\s*([0-9]+(?:\.[0-9]+)?)\s*([A-Za-z]*)\s*$`)

// ParseBytes reads a size as config.toml writes it: 50MiB, 5GB, 1024, "10 KB".
func ParseBytes(v any) (int64, error) {
	var s string
	switch n := v.(type) {
	case int64:
		return n, nil
	case string:
		s = n
	default:
		return 0, fmt.Errorf("want a size like 50MiB")
	}
	m := sizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("want a size like 50MiB")
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	mult := map[string]float64{"": 1, "b": 1, "k": 1e3, "kb": 1e3, "kib": 1 << 10, "m": 1e6, "mb": 1e6, "mib": 1 << 20,
		"g": 1e9, "gb": 1e9, "gib": 1 << 30, "t": 1e12, "tb": 1e12, "tib": 1 << 40}
	x, ok := mult[strings.ToLower(m[2])]
	if !ok {
		return 0, fmt.Errorf("unknown size unit %q", m[2])
	}
	return int64(f * x), nil
}

func asBytes(v any) (any, error) { return ParseBytes(v) }

// one is a mapping of a config.toml key to a setting.
type one struct {
	path string
	svc  projectconfig.Service
	key  string
	conv conv
}

// set records a setting of the plan.
func (p *planner) set(svc projectconfig.Service, key string, v any) {
	if p.plan.Patches[svc] == nil {
		p.plan.Patches[svc] = map[string]any{}
	}
	p.plan.Patches[svc][key] = v
}

// apply maps one key.
func (p *planner) apply(m one) {
	v, ok := p.get(m.path)
	if !ok {
		return
	}
	v, skip := p.resolve("["+tableOf(m.path)+"] "+keyOf(m.path), v)
	if skip {
		return
	}
	out, err := m.conv(v)
	if err != nil {
		p.warn("%s is not applied: %v", m.path, err)
		return
	}
	p.set(m.svc, m.key, out)
	p.plan.Applied = append(p.plan.Applied, m.path)
}

func tableOf(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[:i]
	}
	return ""
}

func keyOf(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

var (
	extProviders = []string{"apple", "azure", "bitbucket", "discord", "facebook", "figma", "github", "gitlab", "google", "kakao",
		"keycloak", "linkedin_oidc", "notion", "slack", "slack_oidc", "spotify", "twitch", "twitter", "workos", "x", "zoom"}
	hookNames = []string{"custom_access_token", "send_sms", "send_email", "mfa_verification_attempt", "password_verification_attempt",
		"before_user_created", "after_user_created"}
	templateNames = []string{"invite", "confirmation", "recovery", "magic_link", "email_change", "reauthentication"}
	notifyNames   = []string{"password_changed", "email_changed", "phone_changed", "mfa_factor_enrolled", "mfa_factor_unenrolled", "identity_linked", "identity_unlinked"}
	smsProviders  = map[string][][2]string{
		"twilio":        {{"account_sid", "sms_twilio_account_sid"}, {"message_service_sid", "sms_twilio_message_service_sid"}, {"auth_token", "sms_twilio_auth_token"}},
		"twilio_verify": {{"account_sid", "sms_twilio_verify_account_sid"}, {"message_service_sid", "sms_twilio_verify_message_service_sid"}, {"auth_token", "sms_twilio_verify_auth_token"}},
		"messagebird":   {{"originator", "sms_messagebird_originator"}, {"access_key", "sms_messagebird_access_key"}},
		"textlocal":     {{"sender", "sms_textlocal_sender"}, {"api_key", "sms_textlocal_api_key"}},
		"vonage":        {{"from", "sms_vonage_from"}, {"api_key", "sms_vonage_api_key"}, {"api_secret", "sms_vonage_api_secret"}},
	}
)

// localOnly are the sections of config.toml that configure the local development stack and mean
// nothing for a hosted project; they are named once, not key by key.
var localOnly = []string{"studio", "inbucket", "analytics", "edge_runtime", "experimental", "realtime", "api.port", "api.tls", "api.enabled",
	"db.port", "db.shadow_port", "db.major_version", "db.root_key", "db.migrations", "db.vault", "db.pooler", "auth.enabled", "auth.jwt_secret", "auth.signing_keys_path",
	"storage.enabled", "storage.s3_protocol.s3_credentials"}

// PlanConfig maps a parsed config.toml onto a branch.
func PlanConfig(in PlanInput) (*ConfigPlan, error) {
	plan := &ConfigPlan{Patches: map[projectconfig.Service]map[string]any{}, Functions: map[string]FunctionConfig{}}
	doc, remote := effectiveConfig(in.Doc, in.Ref)
	if remote != "" {
		plan.Applied = append(plan.Applied, "remotes."+remote)
	}
	p := &planner{in: in, doc: doc, consumed: map[string]bool{}, plan: plan}
	p.api()
	p.db()
	p.auth()
	p.storage()
	p.functions()
	p.sweep()
	if in.Branch {
		p.dropInheritedSecrets()
		if err := p.holdBackSwitchesWithoutSecrets(); err != nil {
			return nil, err
		}
	}
	sort.Strings(plan.Applied)
	if in.Persistent && remote == "" {
		// Hosted configures a persistent branch from its [remotes] block alone: without one the step is skipped.
		plan.Patches, plan.Applied, plan.Warnings, plan.Templates = map[projectconfig.Service]map[string]any{}, nil, nil, nil
		plan.Skipped = "no [remotes.*] block in config.toml has project_id = " + in.Ref + ", so the settings of a persistent branch are not applied (find the ref with `supabase branches list`)"
	}
	return plan, nil
}

// dropInheritedSecrets clears the secret a branch holds when config.toml sends the secret's
// destination somewhere else and gives the branch no secret of its own: the SMTP password when the SMTP
// host is set, the client secret of an OAuth provider when its url is set. A branch does not start
// with its parent's credentials (projectconfig.BranchWithheld), so the secret is one somebody set on
// the branch, or one an earlier commit gave it. Whoever can open a pull request chooses config.toml,
// and may have no access to the branch's secrets; a branch's mail or token requests go to the host the
// repository names, so the credential must not go with them. A secret that config.toml gives
// (env(NAME) of a secret of the branch) is applied as before.
func (p *planner) dropInheritedSecrets() {
	auth := p.plan.Patches[projectconfig.Auth]
	keys := make([]string, 0, len(auth))
	for k := range auth {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		var secret, path string
		switch {
		case key == "smtp_host":
			secret, path = "smtp_pass", "[auth.email.smtp]"
		case strings.HasPrefix(key, "external_") && strings.HasSuffix(key, "_url"):
			name := strings.TrimSuffix(strings.TrimPrefix(key, "external_"), "_url")
			secret, path = "external_"+name+"_secret", "[auth.external."+name+"]"
		default:
			continue
		}
		if v, ok := auth[key].(string); !ok || v == "" {
			continue
		}
		if _, given := auth[secret]; given {
			continue
		}
		auth[secret] = nil
		p.warn("%s sets where %s goes, so the secret the branch holds for it is cleared; give the branch its own with a secret set by env(NAME)", path, secret)
	}
}

// holdBackSwitchesWithoutSecrets leaves the switch of an OAuth provider, of the captcha or of an
// HTTP hook out of a branch's patch when the credential it needs is neither in the patch nor already
// saved on the branch. A branch starts without its parent's credentials (projectconfig.BranchWithheld)
// and a preview branch holds no Edge Function secret before its first run, so env(NAME) cannot be
// read then. The node refuses a switch without its secret ("external_github_secret is required when
// the provider is enabled"), and that refusal would fail the configure step, and with it the run,
// before the migrations. The rest of the file applies; the switch follows on the first push after the
// branch has the secret, and a secret somebody saved on the branch itself counts. A secret that
// dropInheritedSecrets cleared is not held.
func (p *planner) holdBackSwitchesWithoutSecrets() error {
	auth := p.plan.Patches[projectconfig.Auth]
	if len(auth) == 0 {
		return nil
	}
	var failed error
	have := func(secret string) bool {
		if v, given := auth[secret]; given {
			s, _ := v.(string)
			return strings.TrimSpace(s) != ""
		}
		if p.in.Held == nil {
			return false
		}
		ok, err := p.in.Held(projectconfig.Auth, secret)
		if err != nil && failed == nil {
			failed = fmt.Errorf("read whether the branch has %s: %w", secret, err)
		}
		return ok
	}
	hold := func(switchKey, path, secret string) {
		if on, _ := auth[switchKey].(bool); !on || have(secret) {
			return
		}
		delete(auth, switchKey)
		p.plan.Applied = slices.DeleteFunc(p.plan.Applied, func(a string) bool { return a == path })
		p.warn("%s is not applied: the branch has no %s yet, and the node refuses the switch without it (give the branch its own, for example secret = env(NAME) in config.toml with NAME set by supabase secrets set --project-ref <branch>, and push again)", path, secret)
	}
	for _, e := range extProviders {
		if e != "apple" { // Sign in with Apple builds its client secret itself
			hold("external_"+e+"_enabled", "auth.external."+e+".enabled", "external_"+e+"_secret")
		}
	}
	hold("security_captcha_enabled", "auth.captcha.enabled", "security_captcha_secret")
	for _, h := range hookNames {
		if uri, _ := auth["hook_"+h+"_uri"].(string); strings.HasPrefix(strings.ToLower(uri), "http") {
			hold("hook_"+h+"_enabled", "auth.hook."+h+".enabled", "hook_"+h+"_secrets")
		}
	}
	return failed
}

func (p *planner) api() {
	for _, m := range []one{
		{"api.schemas", projectconfig.PostgREST, "db_schema", asList},
		{"api.extra_search_path", projectconfig.PostgREST, "db_extra_search_path", asList},
		{"api.max_rows", projectconfig.PostgREST, "max_rows", asInt},
	} {
		p.apply(m)
	}
}

func (p *planner) db() {
	if v, ok := p.get("db.settings"); ok {
		t, _ := v.(map[string]any)
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			f, known := projectconfig.PostgresSchema.Field(k)
			if !known {
				p.warn("db.settings.%s is not a Postgres setting Supavise saves; it is not applied", k)
				p.consumed["db.settings."+k] = true
				continue
			}
			p.consumed["db.settings."+k] = true
			val, skip := p.resolve("[db.settings] "+k, t[k])
			if skip {
				continue
			}
			switch f.Kind {
			case projectconfig.Int:
				val, _ = asInt(val)
			case projectconfig.Bool:
				val, _ = asBool(val)
			default:
				if n, ok := val.(int64); ok {
					val = strconv.FormatInt(n, 10)
				}
			}
			p.set(projectconfig.Postgres, k, val)
			p.plan.Applied = append(p.plan.Applied, "db.settings."+k)
		}
	}
	if v, ok := p.get("db.seed"); ok {
		t, _ := v.(map[string]any)
		if e, ok := t["enabled"].(bool); ok {
			p.plan.Seed.Enabled = &e
		}
		if l, ok := t["sql_paths"].([]any); ok {
			for _, e := range l {
				if s, ok := e.(string); ok {
					p.plan.Seed.SQLPaths = append(p.plan.Seed.SQLPaths, s)
				}
			}
		}
		p.consumed["db.seed.enabled"], p.consumed["db.seed.sql_paths"] = true, true
	}
}

func (p *planner) auth() {
	maps := []one{
		{"auth.site_url", projectconfig.Auth, "site_url", asString},
		{"auth.additional_redirect_urls", projectconfig.Auth, "uri_allow_list", asList},
		{"auth.jwt_expiry", projectconfig.Auth, "jwt_exp", durationSeconds},
		{"auth.enable_refresh_token_rotation", projectconfig.Auth, "refresh_token_rotation_enabled", asBool},
		{"auth.refresh_token_reuse_interval", projectconfig.Auth, "security_refresh_token_reuse_interval", durationSeconds},
		{"auth.enable_manual_linking", projectconfig.Auth, "security_manual_linking_enabled", asBool},
		{"auth.enable_signup", projectconfig.Auth, "disable_signup", asNotBool},
		{"auth.enable_anonymous_sign_ins", projectconfig.Auth, "external_anonymous_users_enabled", asBool},
		{"auth.minimum_password_length", projectconfig.Auth, "password_min_length", asInt},
		{"auth.password_requirements", projectconfig.Auth, "password_required_characters", asPasswordReq},

		{"auth.rate_limit.email_sent", projectconfig.Auth, "rate_limit_email_sent", asInt},
		{"auth.rate_limit.sms_sent", projectconfig.Auth, "rate_limit_sms_sent", asInt},
		{"auth.rate_limit.anonymous_users", projectconfig.Auth, "rate_limit_anonymous_users", asInt},
		{"auth.rate_limit.token_refresh", projectconfig.Auth, "rate_limit_token_refresh", asInt},
		{"auth.rate_limit.sign_in_sign_ups", projectconfig.Auth, "rate_limit_otp", asInt},
		{"auth.rate_limit.token_verifications", projectconfig.Auth, "rate_limit_verify", asInt},
		{"auth.rate_limit.web3", projectconfig.Auth, "rate_limit_web3", asInt},

		{"auth.email.enable_signup", projectconfig.Auth, "external_email_enabled", asBool},
		{"auth.email.double_confirm_changes", projectconfig.Auth, "mailer_secure_email_change_enabled", asBool},
		{"auth.email.enable_confirmations", projectconfig.Auth, "mailer_autoconfirm", asNotBool},
		{"auth.email.secure_password_change", projectconfig.Auth, "security_update_password_require_reauthentication", asBool},
		{"auth.email.max_frequency", projectconfig.Auth, "smtp_max_frequency", durationSeconds},
		{"auth.email.otp_length", projectconfig.Auth, "mailer_otp_length", asInt},
		{"auth.email.otp_expiry", projectconfig.Auth, "mailer_otp_exp", asInt},
		{"auth.email.smtp.host", projectconfig.Auth, "smtp_host", asString},
		{"auth.email.smtp.port", projectconfig.Auth, "smtp_port", asNumberString},
		{"auth.email.smtp.user", projectconfig.Auth, "smtp_user", asString},
		{"auth.email.smtp.pass", projectconfig.Auth, "smtp_pass", asString},
		{"auth.email.smtp.admin_email", projectconfig.Auth, "smtp_admin_email", asString},
		{"auth.email.smtp.sender_name", projectconfig.Auth, "smtp_sender_name", asString},

		{"auth.sms.enable_signup", projectconfig.Auth, "external_phone_enabled", asBool},
		{"auth.sms.enable_confirmations", projectconfig.Auth, "sms_autoconfirm", asNotBool},
		{"auth.sms.template", projectconfig.Auth, "sms_template", asString},
		{"auth.sms.max_frequency", projectconfig.Auth, "sms_max_frequency", durationSeconds},

		{"auth.mfa.max_enrolled_factors", projectconfig.Auth, "mfa_max_enrolled_factors", asInt},
		{"auth.mfa.totp.enroll_enabled", projectconfig.Auth, "mfa_totp_enroll_enabled", asBool},
		{"auth.mfa.totp.verify_enabled", projectconfig.Auth, "mfa_totp_verify_enabled", asBool},
		{"auth.mfa.phone.enroll_enabled", projectconfig.Auth, "mfa_phone_enroll_enabled", asBool},
		{"auth.mfa.phone.verify_enabled", projectconfig.Auth, "mfa_phone_verify_enabled", asBool},
		{"auth.mfa.phone.otp_length", projectconfig.Auth, "mfa_phone_otp_length", asInt},
		{"auth.mfa.phone.template", projectconfig.Auth, "mfa_phone_template", asString},
		{"auth.mfa.phone.max_frequency", projectconfig.Auth, "mfa_phone_max_frequency", durationSeconds},
		{"auth.mfa.web_authn.enroll_enabled", projectconfig.Auth, "mfa_web_authn_enroll_enabled", asBool},
		{"auth.mfa.web_authn.verify_enabled", projectconfig.Auth, "mfa_web_authn_verify_enabled", asBool},

		{"auth.captcha.enabled", projectconfig.Auth, "security_captcha_enabled", asBool},
		{"auth.captcha.provider", projectconfig.Auth, "security_captcha_provider", asString},
		{"auth.captcha.secret", projectconfig.Auth, "security_captcha_secret", asString},

		{"auth.sessions.timebox", projectconfig.Auth, "sessions_timebox", durationHours},
		{"auth.sessions.inactivity_timeout", projectconfig.Auth, "sessions_inactivity_timeout", durationHours},

		{"auth.passkey.enabled", projectconfig.Auth, "passkey_enabled", asBool},
		{"auth.webauthn.rp_display_name", projectconfig.Auth, "webauthn_rp_display_name", asString},
		{"auth.webauthn.rp_id", projectconfig.Auth, "webauthn_rp_id", asString},
		{"auth.webauthn.rp_origins", projectconfig.Auth, "webauthn_rp_origins", asList},
		{"auth.web3.solana.enabled", projectconfig.Auth, "external_web3_solana_enabled", asBool},
		{"auth.web3.ethereum.enabled", projectconfig.Auth, "external_web3_ethereum_enabled", asBool},
	}
	for _, n := range notifyNames {
		maps = append(maps, one{"auth.email.notification." + n + ".enabled", projectconfig.Auth, "mailer_notifications_" + n + "_enabled", asBool})
	}
	for _, h := range hookNames {
		maps = append(maps,
			one{"auth.hook." + h + ".enabled", projectconfig.Auth, "hook_" + h + "_enabled", asBool},
			one{"auth.hook." + h + ".uri", projectconfig.Auth, "hook_" + h + "_uri", asString},
			one{"auth.hook." + h + ".secrets", projectconfig.Auth, "hook_" + h + "_secrets", asString})
	}
	for _, e := range extProviders {
		pre := "auth.external." + e + "."
		maps = append(maps,
			one{pre + "enabled", projectconfig.Auth, "external_" + e + "_enabled", asBool},
			one{pre + "client_id", projectconfig.Auth, "external_" + e + "_client_id", asString},
			one{pre + "secret", projectconfig.Auth, "external_" + e + "_secret", asString},
			one{pre + "url", projectconfig.Auth, "external_" + e + "_url", asString},
			one{pre + "skip_nonce_check", projectconfig.Auth, "external_" + e + "_skip_nonce_check", asBool},
			one{pre + "email_optional", projectconfig.Auth, "external_" + e + "_email_optional", asBool})
		// The redirect URI of a provider is GoTrue's own callback here and cannot be changed.
		if _, ok := p.get(pre + "redirect_uri"); ok {
			p.warn("%sredirect_uri is not applied: the callback of a provider is the project's own address and cannot be changed", pre)
		}
	}
	// [auth.email.smtp] enabled = false keeps the CLI from pushing the SMTP settings.
	if v, ok := p.get("auth.email.smtp.enabled"); ok {
		if b, isBool := v.(bool); isBool && !b {
			var kept []one
			for _, m := range maps {
				if strings.HasPrefix(m.path, "auth.email.smtp.") {
					p.consumed[m.path] = true
					continue
				}
				kept = append(kept, m)
			}
			maps = kept
		}
	}
	p.get("auth.email.enabled")
	p.get("auth.sms.enabled")
	for _, m := range maps {
		// Only the settings the schema has: a provider has no url unless it is a self-hosted one.
		if _, known := projectconfig.AuthSchema.Field(m.key); !known {
			if _, present := lookup(p.doc, strings.Split(m.path, ".")); present {
				p.warn("%s is not applied: Supavise has no such Auth setting", m.path)
				p.consumed[m.path] = true
			}
			continue
		}
		p.apply(m)
	}
	// Email templates and subjects.
	for _, n := range templateNames {
		pre := "auth.email.template." + n + "."
		p.apply(one{pre + "subject", projectconfig.Auth, "mailer_subjects_" + n, asString})
		if v, ok := p.get(pre + "content_path"); ok {
			rel, _ := v.(string)
			rel = strings.TrimPrefix(strings.TrimPrefix(rel, "./"), "/")
			p.plan.Templates = append(p.plan.Templates, rel)
			if p.in.Template != nil {
				if body, ok := p.in.Template(rel); ok {
					p.set(projectconfig.Auth, "mailer_templates_"+n+"_content", body)
					p.plan.Applied = append(p.plan.Applied, strings.TrimSuffix(pre, "."))
				} else {
					p.warn("%scontent_path %q is not a file of the repository; the template is not applied", pre, rel)
				}
			}
		}
	}
	// SMS providers: the one that is enabled is the provider.
	enabled := ""
	for _, name := range []string{"twilio", "twilio_verify", "messagebird", "textlocal", "vonage"} {
		pre := "auth.sms." + name + "."
		if v, ok := p.get(pre + "enabled"); ok {
			if b, _ := v.(bool); b {
				enabled = name
			}
		}
		for _, kv := range smsProviders[name] {
			if _, known := projectconfig.AuthSchema.Field(kv[1]); known {
				p.apply(one{pre + kv[0], projectconfig.Auth, kv[1], asString})
			}
		}
	}
	if enabled != "" {
		p.set(projectconfig.Auth, "sms_provider", enabled)
		p.plan.Applied = append(p.plan.Applied, "auth.sms."+enabled+".enabled")
	}
	// Test OTPs: [auth.sms.test_otp] phone = "123456" becomes "phone=123456,...".
	if v, ok := p.get("auth.sms.test_otp"); ok {
		if t, ok := v.(map[string]any); ok && len(t) > 0 {
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s=%v", k, t[k]))
			}
			p.set(projectconfig.Auth, "sms_test_otp", strings.Join(parts, ","))
			p.plan.Applied = append(p.plan.Applied, "auth.sms.test_otp")
		}
	}
}

func (p *planner) storage() {
	p.apply(one{"storage.file_size_limit", projectconfig.Storage, "fileSizeLimit", asBytes})
	features := map[string]any{}
	if v, ok := p.get("storage.image_transformation.enabled"); ok {
		if b, ok := v.(bool); ok && b {
			// The same refusal Studio and the CLI get from the node, as a warning here: an error would fail
			// the configure step of every run of a repository that has the setting, and with it the Storage
			// settings and the migrations that follow. `false` still applies, so a project that has it on can
			// be switched off.
			p.warn("storage.image_transformation.enabled = true is not applied: %s (set it to false)", projectconfig.NoImageTransformation)
		} else if ok {
			features["imageTransformation"] = map[string]any{"enabled": b}
			p.plan.Applied = append(p.plan.Applied, "storage.image_transformation.enabled")
		}
	}
	if v, ok := p.get("storage.s3_protocol.enabled"); ok {
		if b, ok := v.(bool); ok {
			features["s3Protocol"] = map[string]any{"enabled": b}
			p.plan.Applied = append(p.plan.Applied, "storage.s3_protocol.enabled")
		}
	}
	if len(features) > 0 {
		p.set(projectconfig.Storage, "features", features)
	}
	if v, ok := p.get("storage.buckets"); ok {
		t, _ := v.(map[string]any)
		names := make([]string, 0, len(t))
		for n := range t {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			bt, _ := t[n].(map[string]any)
			for k := range bt {
				switch k {
				case "public", "file_size_limit", "allowed_mime_types", "objects_path":
				default:
					p.warn("storage.buckets.%s.%s is not applied: Supavise does not support it", n, k)
				}
			}
			b := BucketSpec{Name: n}
			b.Public, _ = bt["public"].(bool)
			if lim, ok := bt["file_size_limit"]; ok {
				n, err := ParseBytes(lim)
				if err != nil {
					p.warn("storage.buckets.%s.file_size_limit is not applied: %v", b.Name, err)
				} else {
					b.FileSizeLimit = n
				}
			}
			if l, ok := bt["allowed_mime_types"].([]any); ok {
				for _, e := range l {
					if s, ok := e.(string); ok {
						b.AllowedMIMETypes = append(b.AllowedMIMETypes, s)
					}
				}
			}
			if op, _ := bt["objects_path"].(string); op != "" {
				b.ObjectsPath = op
				p.warn("storage.buckets.%s.objects_path is not applied: Supavise creates the bucket and does not upload files into it", b.Name)
			}
			p.plan.Buckets = append(p.plan.Buckets, b)
			p.plan.Applied = append(p.plan.Applied, "storage.buckets."+b.Name)
		}
	}
}

func (p *planner) functions() {
	v, ok := p.get("functions")
	if !ok {
		return
	}
	t, _ := v.(map[string]any)
	for slug, e := range t {
		ft, ok := e.(map[string]any)
		if !ok {
			continue
		}
		var fc FunctionConfig
		for k := range ft {
			switch k {
			case "enabled", "verify_jwt", "entrypoint", "import_map", "static_files":
			default:
				p.warn("functions.%s.%s is not applied: Supavise does not support it", slug, k)
			}
		}
		if b, ok := ft["enabled"].(bool); ok {
			fc.Enabled = &b
		}
		if b, ok := ft["verify_jwt"].(bool); ok {
			fc.VerifyJWT = &b
		}
		fc.Entrypoint, _ = ft["entrypoint"].(string)
		fc.ImportMap, _ = ft["import_map"].(string)
		if l, ok := ft["static_files"].([]any); ok {
			for _, s := range l {
				if str, ok := s.(string); ok {
					fc.Static = append(fc.Static, str)
				}
			}
		}
		p.plan.Functions[slug] = fc
	}
}

// sweep names every key of the document that nothing consumed. The sections that configure the
// local development stack are named once.
func (p *planner) sweep() {
	var leaves []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		if m, ok := v.(map[string]any); ok {
			for k, e := range m {
				walk(join(prefix, k), e)
			}
			return
		}
		leaves = append(leaves, prefix)
	}
	walk("", p.doc)
	sort.Strings(leaves)
	var local []string
	seenLocal := map[string]bool{}
	for _, l := range leaves {
		if l == "" || p.isConsumed(l) {
			continue
		}
		if sec, ok := p.localSection(l); ok {
			if !seenLocal[sec] {
				seenLocal[sec] = true
				local = append(local, sec)
			}
			continue
		}
		p.warn("%s is not applied: Supavise does not support it", l)
	}
	if len(local) > 0 {
		p.warn("%s: local development settings, not applied", strings.Join(local, ", "))
	}
}

func join(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

// isConsumed reports whether the leaf is a mapped key or lies under a consumed table.
func (p *planner) isConsumed(leaf string) bool {
	parts := strings.Split(leaf, ".")
	for i := len(parts); i > 0; i-- {
		if p.consumed[strings.Join(parts[:i], ".")] {
			return true
		}
	}
	return false
}

func (p *planner) localSection(leaf string) (string, bool) {
	for _, l := range localOnly {
		if leaf == l || strings.HasPrefix(leaf, l+".") {
			return "[" + l + "]", true
		}
	}
	return "", false
}
