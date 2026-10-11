package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/supavise/supavise/internal/advisors"
	v2 "github.com/supavise/supavise/internal/api/gen/v2"
	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
)

// The Security and Performance Advisors. Every surface hosted offers for them is served from one
// engine (internal/advisors), so Studio, the CLI and the MCP server see the same findings:
//
//	GET  /platform/projects/{ref}/run-lints       Studio's Advisors pages, the project home widget, the
//	                                              table editor badges: every lint, split by the client
//	GET  /v1/projects/{ref}/advisors/security     `supabase db advisors --linked`, MCP get_advisors:
//	GET  /v1/projects/{ref}/advisors/performance  the lints of one category, in {"lints": [...]}
//	POST /v2/projects/{ref}/advisors/run          the lints asked for by name (Studio's health advisor,
//	                                              MCP get_advisors "health")
//	.../notifications/advisor/exceptions          the rules of the Advisor Rules page (advisors_exceptions.go)
//
// The lints of the schema are the SQL Studio redistributes, run as supabase_admin through pg-meta
// like Studio's own self-hosted runner; the lints of the configuration are computed from the
// project's Auth settings.

func (s *Server) routesAdvisors(add func(string, handlerFunc)) {
	add("GET /platform/projects/{ref}/run-lints", s.runLints)
	add("GET /v1/projects/{ref}/advisors/security", s.advisorsOfCategory(advisors.CategorySecurity))
	add("GET /v1/projects/{ref}/advisors/performance", s.advisorsOfCategory(advisors.CategoryPerformance))
	add("POST /v2/projects/{ref}/advisors/run", s.advisorsRunV2)
	s.routesAdvisorExceptions(add)
}

// runLints answers Studio with every lint of the project; the pages filter by category.
func (s *Server) runLints(w http.ResponseWriter, r *http.Request) error {
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	lints, err := s.projectLints(r, p, false)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, lints)
	return nil
}

// advisorsOfCategory serves the lints of one category in the envelope of the v1 spec. The query
// parameter lint_type=sql, the only value of the spec, leaves out the lints computed from the
// project's configuration.
func (s *Server) advisorsOfCategory(category string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if lt := r.URL.Query().Get("lint_type"); lt != "" && lt != "sql" {
			return errf(http.StatusBadRequest, "lint_type must be sql")
		}
		p, err := s.running(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		lints, err := s.projectLints(r, p, r.URL.Query().Get("lint_type") == "sql")
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"lints": advisors.Filter(lints, category)})
		return nil
	}
}

// projectLints computes the lints of p for the caller of r: the SQL lints (cached for a few
// seconds, one run shared by simultaneous callers), the lints of the Auth settings unless sqlOnly,
// and then the rules of the Advisor Rules page applied.
func (s *Server) projectLints(r *http.Request, p *registry.Project, sqlOnly bool) ([]advisors.Lint, error) {
	ctx := r.Context()
	lints, err := s.sqlLints(ctx, p)
	if err != nil {
		return nil, err
	}
	// The cached slice is shared: build a new one.
	out := make([]advisors.Lint, 0, len(lints)+4)
	out = append(out, lints...)
	if !sqlOnly {
		if a, ok := s.authSettings(ctx, p.Ref); ok {
			out = append(out, advisors.ConfigLints(a)...)
		}
	}
	return s.applyAdvisorRules(r, p.Ref, out), nil
}

// sqlLints runs the lint SQL in p's database, or serves the run of the last few seconds.
func (s *Server) sqlLints(ctx context.Context, p *registry.Project) ([]advisors.Lint, error) {
	exec := func(ctx context.Context, q string) ([]byte, error) {
		return s.sqlRows(ctx, p.Ref, "supabase_admin", false, q)
	}
	lints, err := s.advisorRunner.SQL(ctx, p.Ref, s.exposedSchemas(ctx, p.Ref), exec)
	if err != nil {
		return nil, s.advisorError(p.Ref, err)
	}
	return lints, nil
}

// advisorError turns a failed run into the error the caller sees. A SQL error is not the caller's
// mistake, so pg-meta's 400 becomes a 500; a run that outlasts the statement timeout is a 504.
func (s *Server) advisorError(ref string, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	if advisors.IsTimeout(err) {
		return errf(http.StatusGatewayTimeout, "The advisor checks took longer than %d seconds; try again", int(advisors.StatementTimeout.Seconds()))
	}
	var e *Error
	if errors.As(err, &e) {
		if e.Status != http.StatusBadRequest {
			return err
		}
		s.log.Error("advisor query failed", "ref", ref, "message", truncate(e.Message, 200))
		return errf(http.StatusInternalServerError, "Failed to run the advisor checks: %s", e.Message)
	}
	s.log.Error("advisor query failed", "ref", ref, "err", err)
	return errf(http.StatusInternalServerError, "Failed to run the advisor checks")
}

// exposedSchemas is the comma-separated list of schemas PostgREST serves for ref, which the lints
// that ask what the Data API exposes read as pgrst.db_schemas. Empty selects the default.
func (s *Server) exposedSchemas(ctx context.Context, ref string) string {
	st, err := s.settings.Get(ctx, ref, projectconfig.PostgREST)
	if err != nil {
		s.log.Warn("advisors: PostgREST settings unreadable, using the default schemas", "ref", ref, "err", err)
		return ""
	}
	return st.Effective.Str("db_schema")
}

// authSettings reads the Auth settings the configuration lints look at; ok is false when they
// cannot be read, and the lints are left out then rather than guessed.
func (s *Server) authSettings(ctx context.Context, ref string) (advisors.AuthSettings, bool) {
	st, err := s.settings.Get(ctx, ref, projectconfig.Auth)
	if err != nil {
		s.log.Warn("advisors: Auth settings unreadable, leaving out the configuration lints", "ref", ref, "err", err)
		return advisors.AuthSettings{}, false
	}
	e := st.Effective
	exp, _ := e.Int("mailer_otp_exp")
	pool, _ := e.Int("db_max_pool_size")
	return advisors.AuthSettings{
		EmailEnabled:        e.Bool("external_email_enabled"),
		MailerOTPExp:        exp,
		PasswordHIBPEnabled: e.Bool("password_hibp_enabled"),
		TOTPEnabled:         e.Bool("mfa_totp_enroll_enabled") && e.Bool("mfa_totp_verify_enabled"),
		PhoneEnabled:        e.Bool("mfa_phone_enroll_enabled") && e.Bool("mfa_phone_verify_enabled"),
		WebAuthnEnabled:     e.Bool("mfa_web_authn_enroll_enabled") && e.Bool("mfa_web_authn_verify_enabled"),
		DBMaxPoolSize:       pool,
		DBMaxPoolSizeUnit:   e.Str("db_max_pool_size_unit"),
	}, true
}

var advisorNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// advisorsRunV2 runs the lints asked for by name. The SQL lints and the configuration lints come
// from the same engine as the other routes; the health lints need logs and metrics Supavise does
// not collect, and answer advisor_check_unavailable, which the clients leave out of what they show.
func (s *Server) advisorsRunV2(w http.ResponseWriter, r *http.Request) error {
	var in v2.V2RunProjectAdvisorsBody
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.Data.Type != "project_advisors" {
		return errf(http.StatusBadRequest, `data.type must be "project_advisors"`)
	}
	asked := in.Data.Attributes.Lints
	if len(asked) < 1 || len(asked) > 10 {
		return errf(http.StatusBadRequest, "data.attributes.lints needs between 1 and 10 lints")
	}
	var names []string
	seen := map[string]bool{}
	for _, l := range asked {
		n := string(l.Name)
		if !advisorNameRe.MatchString(n) {
			return errf(http.StatusBadRequest, "invalid lint name %q", truncate(n, 40))
		}
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var out []advisors.Lint
	if p.Status != registry.StatusActiveHealthy && p.Status != registry.StatusActiveUnhealthy {
		out = []advisors.Lint{advisors.NotActive()}
	} else if out, err = s.lintsByName(r, p, names); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
		"type":       "project_advisors",
		"attributes": map[string]any{"lints": out},
	}})
	return nil
}

// lintsByName returns the lints of the requested names that p has, then one advisor_check_unavailable
// for each health check this node cannot run. Lints of a name that nothing here computes are not
// reported, as hosted does for a check that finds nothing.
func (s *Server) lintsByName(r *http.Request, p *registry.Project, names []string) ([]advisors.Lint, error) {
	ctx := r.Context()
	want := make(map[string]bool, len(names))
	needSQL, needConfig := false, false
	for _, n := range names {
		want[n] = true
		needSQL = needSQL || advisors.IsSQLLint(n)
		needConfig = needConfig || advisors.IsConfigLint(n)
	}
	var all []advisors.Lint
	if needSQL {
		l, err := s.sqlLints(ctx, p)
		if err != nil {
			return nil, err
		}
		all = append(all, l...)
	}
	if needConfig {
		if a, ok := s.authSettings(ctx, p.Ref); ok {
			all = append(all, advisors.ConfigLints(a)...)
		}
	}
	out := []advisors.Lint{}
	for _, l := range all {
		if want[l.Name] {
			out = append(out, l)
		}
	}
	for _, n := range names {
		if advisors.IsHealthCheck(n) {
			out = append(out, advisors.Unavailable(n))
		}
	}
	return out, nil
}

// invalidateAdvisors drops what is kept of p's lints after a change of its schema through the API.
func (s *Server) invalidateAdvisors(ref string) { s.advisorRunner.Invalidate(ref) }

// schemaChangeRe finds the statements that change what the advisors read: definitions, grants and
// ownership. A word inside a string or a comment matches too, which costs one more run of the lints
// and nothing else.
var schemaChangeRe = regexp.MustCompile(`(?i)\b(create|alter|drop|grant|revoke|truncate|comment\s+on|security\s+label)\b`)

// mayChangeSchema reports whether the SQL text may change the schema.
func mayChangeSchema(sql []byte) bool { return schemaChangeRe.Match(sql) }

// jsonWhitespace turns the escaped line breaks and tabs of a JSON string into spaces, for a body
// that cannot be decoded: \n followed by a keyword has no word boundary before the keyword.
var jsonWhitespace = strings.NewReplacer(`\n`, " ", `\r`, " ", `\t`, " ")

// mayChangeSchemaBody is mayChangeSchema for the JSON body of a pg-meta /query request. It matches
// the decoded query, because a line break in the SQL is the two characters \n in the body and
// leaves the keyword after it without a word boundary; a body that is not the expected JSON is
// matched as it came, with its escaped whitespace opened up.
func mayChangeSchemaBody(body []byte) bool {
	var in struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(body, &in); err == nil && in.Query != "" {
		return mayChangeSchema([]byte(in.Query))
	}
	return mayChangeSchema(bytes.TrimSpace([]byte(jsonWhitespace.Replace(string(body)))))
}
