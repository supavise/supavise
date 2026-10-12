package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/projectconfig"
)

// Two Auth calls of Studio that the Management API's specs list and a stub used to answer with a success
// that changed nothing.
//
//   - POST /platform/auth/{ref}/templates/{template}/reset: Auth > Emails > Reset to default, and the flow
//     that turns custom SMTP off (it resets every template). Studio caches the answer as the project's Auth
//     config (auth-template-reset-mutation.ts), so the answer is the full config, the same body as
//     GET /platform/auth/{ref}/config. The stub's minimal object zeroed the Auth pages until a reload, and
//     a save from such a page could write the zeros.
//   - DELETE /platform/auth/{ref}/users/{id}/factors: Auth > Users > Remove MFA factors.

func (s *Server) routesAuthExtras(add func(string, handlerFunc)) {
	add("POST /platform/auth/{ref}/templates/{template}/reset", s.resetAuthTemplate)
	add("DELETE /platform/auth/{ref}/users/{id}/factors", s.removeUserFactors)
}

// resetAuthTemplate clears the subject and the body saved for one email template, so that GoTrue sends its
// own default again, and answers with the project's Auth config. Nothing else is touched: the Site URL, the
// redirect list and the other templates stay as they are.
func (s *Server) resetAuthTemplate(w http.ResponseWriter, r *http.Request) error {
	p, err := s.settingsProject(r)
	if err != nil {
		return err
	}
	// The path carries the template in kebab case (magic-link, mfa-factor-enrolled-notification).
	name := strings.ReplaceAll(r.PathValue("template"), "-", "_")
	subject, content := "mailer_subjects_"+name, "mailer_templates_"+name+"_content"
	if _, ok := projectconfig.AuthSchema.Field(subject); !ok {
		return errf(http.StatusBadRequest, "%q is not an email template", r.PathValue("template"))
	}
	if _, ok := projectconfig.AuthSchema.Field(content); !ok {
		return errf(http.StatusBadRequest, "%q is not an email template", r.PathValue("template"))
	}
	ch, _, err := s.saveSettings(r.Context(), p, projectconfig.Auth, map[string]any{subject: nil, content: nil}, lifecycle.ApplyOptions{})
	if err != nil {
		return err
	}
	const key = "GET /platform/auth/{ref}/config"
	writeStatusFor(w, "POST /platform/auth/{ref}/templates/{template}/reset", authView(key, ch.State, true))
	return nil
}

// removeUserFactors deletes every MFA factor of a user through GoTrue's admin API (GET and DELETE
// /admin/users/{id}/factors[/{factor_id}], internal/api/api.go of supabase/auth v2.197.0), so that the user
// can enroll again. The service_role key of the project is the admin credential, as for the other
// user routes (adminProxy).
func (s *Server) removeUserFactors(w http.ResponseWriter, r *http.Request) error {
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	if id == "" || strings.ContainsAny(id, "/?#") || id == "." || id == ".." {
		return errf(http.StatusBadRequest, "Invalid id")
	}
	keys, err := s.mgr.Keys(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	base := s.upstream(p, upGoTrue) + "/admin/users/" + url.PathEscape(id) + "/factors"
	call := func(method, target string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(r.Context(), method, target, nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+keys.ServiceRoleKey)
		req.Header.Set("apikey", keys.ServiceRoleKey)
		resp, err := s.hc.Do(req)
		if err != nil {
			s.log.Error("upstream unreachable", "svc", upGoTrue, "ref", p.Ref, "err", err)
			return 0, nil, errf(http.StatusBadGateway, "%s is unavailable", upGoTrue)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		return resp.StatusCode, b, nil
	}
	status, body, err := call(http.MethodGet, base)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return errf(http.StatusNotFound, "User not found")
	}
	if status != http.StatusOK {
		return gotrueError(status, body, "list the user's MFA factors")
	}
	var factors []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &factors); err != nil {
		return errf(http.StatusBadGateway, "GoTrue answered the list of factors with something that is not a list")
	}
	var failed []string
	for _, f := range factors {
		if f.ID == "" || strings.ContainsAny(f.ID, "/?#") {
			failed = append(failed, f.ID)
			continue
		}
		st, b, err := call(http.MethodDelete, base+"/"+url.PathEscape(f.ID))
		if err != nil {
			return err
		}
		if st != http.StatusOK && st != http.StatusNoContent && st != http.StatusNotFound { // gone already is fine
			s.log.Warn("could not remove an MFA factor", "ref", p.Ref, "user", id, "factor", f.ID, "status", st, "body", string(b))
			failed = append(failed, f.ID)
		}
	}
	if len(failed) > 0 {
		return errf(http.StatusBadGateway, "Removed %d of %d MFA factors; GoTrue refused to remove %s", len(factors)-len(failed), len(factors), strings.Join(failed, ", "))
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

// gotrueError turns an unexpected GoTrue answer into the API's envelope: GoTrue's own message when it has one.
func gotrueError(status int, body []byte, doing string) *Error {
	var in struct {
		Msg     string `json:"msg"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &in)
	msg := in.Msg
	if msg == "" {
		msg = in.Message
	}
	if status >= 500 || status < 400 {
		status = http.StatusBadGateway
	}
	if msg == "" {
		return errf(status, "GoTrue could not %s (it answered %d)", doing, status)
	}
	return errf(status, "GoTrue could not %s: %s", doing, msg)
}
