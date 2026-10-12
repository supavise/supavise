package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/projectconfig"
)

// The auth settings of a project are served three ways: the public Management API
// (GET and PATCH /v1/projects/{ref}/config/auth, lower-case names, what the CLI and the MCP
// server use), and Studio's /platform twins (/platform/auth/{ref}/config and .../hooks, the
// same names in upper case). All three read and write projectconfig.AuthSchema.

func (s *Server) routesConfigAuth(add func(string, handlerFunc)) {
	add("GET /v1/projects/{ref}/config/auth", s.getAuthConfig("GET /v1/projects/{ref}/config/auth", false))
	add("PATCH /v1/projects/{ref}/config/auth", s.patchAuthConfig("GET /v1/projects/{ref}/config/auth", false, false))
	add("GET /platform/auth/{ref}/config", s.getAuthConfig("GET /platform/auth/{ref}/config", true))
	add("PATCH /platform/auth/{ref}/config", s.patchAuthConfig("GET /platform/auth/{ref}/config", true, false))
	add("PATCH /platform/auth/{ref}/config/hooks", s.patchAuthConfig("GET /platform/auth/{ref}/config", true, true))
}

// authView is the response of the config GETs: every setting with its effective value,
// secrets as their redaction, on top of the spec's minimal instance so that fields this
// server does not model stay valid.
func authView(key string, st *projectconfig.State, upper bool) map[string]any {
	resp := base(key)
	name := func(n string) string {
		if upper {
			return strings.ToUpper(n)
		}
		return n
	}
	for i := range projectconfig.AuthSchema.Fields {
		f := &projectconfig.AuthSchema.Fields[i]
		k := name(f.Name)
		if _, known := resp[k]; !known {
			continue // the platform spec leaves a few settings out
		}
		v, ok := st.Effective[f.Name]
		if !ok {
			// The public API reports an unset setting as null; Studio's twin has no nullable
			// fields (its forms read an empty string, false or 0 as "not set").
			resp[k] = nil
			if upper {
				resp[k] = zeroValue(f)
			}
			continue
		}
		resp[k] = secretView(f, v)
	}
	resp[name("mailer_autoconfirm")] = st.AuthAutoconfirm()
	if upper {
		markCustomTemplates(resp, st)
	}
	if _, ok := resp[name("custom_oauth_max_providers")]; ok {
		resp[name("custom_oauth_max_providers")] = 0
	}
	return resp
}

// markCustomTemplates fills MAILER_TEMPLATES_CUSTOM_CONTENTS and MAILER_SUBJECTS_CUSTOM_CONTENTS of Studio's
// twin: for each template, whether a body or a subject is saved on top of GoTrue's own default. Studio's
// Auth > Emails shows Reset template only for a template that is reported custom (hasCustomTemplate in
// TemplateEditor.tsx), so with every flag false, as the spec's minimal object has them, a template that
// the user had edited could never be reset from the dashboard. Text that is saved counts, and nothing
// else: an unset setting, an empty text and a default of this node (the invitation and sign-in link
// subjects that the node's mail settings add) do not.
func markCustomTemplates(resp map[string]any, st *projectconfig.State) {
	for _, marker := range []string{"MAILER_TEMPLATES_CUSTOM_CONTENTS", "MAILER_SUBJECTS_CUSTOM_CONTENTS"} {
		flags, ok := resp[marker].(map[string]any)
		if !ok {
			continue
		}
		for setting := range flags {
			if text, _ := st.Set[strings.ToLower(setting)].(string); text != "" {
				flags[setting] = true
			}
		}
	}
}

func (s *Server) getAuthConfig(key string, upper bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.loadProject(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		st, err := s.settings.Get(r.Context(), p.Ref, projectconfig.Auth)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, authView(key, st, upper))
		return nil
	}
}

func (s *Server) patchAuthConfig(key string, upper, hooksOnly bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.settingsProject(r)
		if err != nil {
			return err
		}
		in, err := patchBody(r)
		if err != nil {
			return err
		}
		if upper && !hooksOnly {
			if err := s.refuseUnsupportedAuthChanges(r.Context(), p.Ref, key, in); err != nil {
				return err
			}
		}
		patch := make(map[string]any, len(in))
		for k, v := range in {
			if upper {
				k = strings.ToLower(k)
			}
			if hooksOnly && !strings.HasPrefix(k, "hook_") {
				continue
			}
			patch[k] = v
		}
		ch, _, err := s.saveSettings(r.Context(), p, projectconfig.Auth, patch, lifecycle.ApplyOptions{})
		if err != nil {
			return err
		}
		for _, c := range ch.Changed {
			if c == "jwt_exp" {
				// PostgREST exposes the token lifetime to SQL (app.settings.jwt_exp).
				s.applyBestEffort(r, p.Ref, projectconfig.PostgREST)
				break
			}
		}
		writeJSON(w, http.StatusOK, authView(key, ch.State, upper))
		return nil
	}
}

// authSettingReasons say why Studio's twin lists a setting this node cannot honor. They are the keys of the
// platform spec's GoTrueConfigResponse that projectconfig.AuthSchema does not model. Any other unknown name
// gets the generic reason.
var authSettingReasons = map[string]string{
	"MFA_ALLOW_LOW_AAL": "GoTrue sets it as a duration (GOTRUE_SESSIONS_ALLOW_LOW_AAL) and the switch has no value that maps to it, " +
		"so a session of a user with an enrolled factor always needs the second factor",
	"INDEX_WORKER_ENSURE_USER_SEARCH_INDEXES_EXIST": "this node does not run hosted's index worker",
	"MAILER_SUBJECTS_CUSTOM_CONTENTS":               "it only reports which subjects are custom; set a subject with MAILER_SUBJECTS_<TEMPLATE>",
	"MAILER_TEMPLATES_CUSTOM_CONTENTS":              "it only reports which templates are custom; set a template with MAILER_TEMPLATES_<TEMPLATE>_CONTENT",
}

// refuseUnsupportedAuthChanges answers 400, before anything is saved, when a PATCH of Studio's twin would
// change a setting that this node does not model. Studio's forms save the fields they show, so a name that is
// not a setting is refused only when its value differs from what the GET reports for it: a form that sends
// the whole object back still saves. Without this the form toasts "Successfully updated" and the next read
// shows the old value (Auth > Audit Logs and MFA before audit_log_disable_postgres was stored).
func (s *Server) refuseUnsupportedAuthChanges(ctx context.Context, ref, key string, in map[string]any) error {
	var unknown []string
	for k := range in {
		if _, ok := projectconfig.AuthSchema.Field(strings.ToLower(k)); !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	st, err := s.settings.Get(ctx, ref, projectconfig.Auth)
	if err != nil {
		return err
	}
	cur := authView(key, st, true)
	sort.Strings(unknown)
	var refused []string
	for _, k := range unknown {
		have, ok := cur[k]
		// A name the GET does not report is unchanged only when the patch clears it; one it reports, when the value is the same.
		if (!ok && in[k] == nil) || (ok && sameJSON(have, in[k])) {
			continue
		}
		reason := authSettingReasons[k]
		if reason == "" {
			reason = "it is not a setting of this node"
		}
		refused = append(refused, k+" ("+reason+")")
	}
	if len(refused) == 0 {
		return nil
	}
	return errf(http.StatusBadRequest, "These settings cannot be changed on this Supavise node, and nothing was saved: %s.", strings.Join(refused, "; "))
}

// sameJSON reports whether two decoded JSON values are equal.
func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// applyBestEffort re-applies svc of ref without failing the request: the change that
// triggered it is already saved and applied.
func (s *Server) applyBestEffort(r *http.Request, ref string, svc projectconfig.Service) {
	rc, ok := s.mgr.(lifecycle.Reconfigurer)
	if !ok {
		return
	}
	ctx, cancel := detached(r, applyTimeout)
	defer cancel()
	if _, err := rc.ApplyConfig(ctx, ref, svc, lifecycle.ApplyOptions{}); err != nil {
		s.log.Warn("could not apply a dependent setting", "ref", ref, "service", svc, "err", err)
	}
}

// zeroValue is the "not set" value of a setting for a response that cannot say null.
func zeroValue(f *projectconfig.Field) any {
	switch f.Kind {
	case projectconfig.Bool:
		return false
	case projectconfig.Int:
		return 0
	case projectconfig.Number:
		return 0.0
	}
	return ""
}
