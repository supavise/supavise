package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/advisors"
	"github.com/supavise/supavise/internal/registry"
)

// The rules of Studio's Advisor Rules page (a feature preview there): hosted keeps them as
// "advisor notification exceptions". A rule hides the lints it selects from the advisors of the
// whole project, or of one member, and from the notification emails hosted sends (Supavise sends
// none). Studio saves a rule and then reads the lints again, so the lints a rule hides are left
// out of run-lints and the v1 advisors here (applyAdvisorRules).

const (
	exceptionsPath   = "/platform/projects/{ref}/notifications/advisor/exceptions"
	maxExceptionsPer = 50 // rules in one POST
)

func (s *Server) routesAdvisorExceptions(add func(string, handlerFunc)) {
	add("GET "+exceptionsPath, s.listAdvisorExceptions)
	add("POST "+exceptionsPath, s.createAdvisorExceptions)
	add("DELETE "+exceptionsPath, s.deleteAdvisorExceptions)
	add("PATCH "+exceptionsPath+"/{id}", s.updateAdvisorException)
}

// advisorStore is the registry's store of rules; nil when the registry has none.
func (s *Server) advisorStore() registry.AdvisorStore {
	st, _ := s.reg.(registry.AdvisorStore)
	return st
}

const zeroUUID = "00000000-0000-0000-0000-000000000000"

// applyAdvisorRules leaves out the lints the project's rules hide from the caller. A failure to read
// the rules shows the lints rather than hiding the advisors.
func (s *Server) applyAdvisorRules(r *http.Request, ref string, lints []advisors.Lint) []advisors.Lint {
	st := s.advisorStore()
	if st == nil {
		return lints
	}
	rules, err := st.ListAdvisorExceptions(r.Context(), ref)
	if err != nil {
		s.log.Warn("advisors: rules unreadable, showing every lint", "ref", ref, "err", err)
		return lints
	}
	if len(rules) == 0 {
		return lints
	}
	selectors := make([]advisors.Exception, len(rules))
	for i, e := range rules {
		selectors[i] = advisors.Exception{
			LintCategory: e.LintCategory, LintName: e.LintName, LintMetadata: e.LintMetadata,
			AssignedTo: e.AssignedTo, IsDisabled: e.IsDisabled,
		}
	}
	user := ""
	if p := principalFrom(r.Context()); p != nil {
		user = p.UserID
	}
	return advisors.Visible(lints, selectors, user)
}

// exceptionJSON is a rule in the shape of the platform spec: the fields that can be unset are null.
func exceptionJSON(e registry.AdvisorException) map[string]any {
	null := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	var meta any
	if len(e.LintMetadata) > 0 {
		meta = e.LintMetadata
	}
	updatedBy := e.LastUpdatedBy
	if updatedBy == "" {
		updatedBy = zeroUUID
	}
	return map[string]any{
		"id": e.ID, "project_ref": e.Ref,
		"lint_category": null(e.LintCategory), "lint_name": null(e.LintName), "lint_metadata": meta,
		"assigned_to": null(e.AssignedTo), "is_disabled": e.IsDisabled,
		"last_updated_by": updatedBy, "note": null(e.Note),
		"inserted_at": e.InsertedAt.UTC().Format(time.RFC3339Nano),
	}
}

func exceptionsJSON(list []registry.AdvisorException) map[string]any {
	out := make([]map[string]any, len(list))
	for i, e := range list {
		out[i] = exceptionJSON(e)
	}
	return map[string]any{"exceptions": out}
}

// advisorRulesStore returns the store, or an error that explains that the registry has none.
func (s *Server) advisorRulesStore() (registry.AdvisorStore, error) {
	if st := s.advisorStore(); st != nil {
		return st, nil
	}
	return nil, errf(http.StatusNotImplemented, "Advisor rules are not available on this registry")
}

func (s *Server) listAdvisorExceptions(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	st, err := s.advisorRulesStore()
	if err != nil {
		return err
	}
	list, err := st.ListAdvisorExceptions(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, exceptionsJSON(list))
	return nil
}

// exceptionIn is one rule of a create or update body.
type exceptionIn struct {
	LintCategory *string        `json:"lint_category"`
	LintName     *string        `json:"lint_name"`
	LintMetadata map[string]any `json:"lint_metadata"`
	AssignedTo   *string        `json:"assigned_to"`
	IsDisabled   *bool          `json:"is_disabled"`
	Note         *string        `json:"note"`
}

// check validates the fields that are set.
func (in exceptionIn) check() error {
	if in.LintCategory != nil && *in.LintCategory != advisors.CategorySecurity && *in.LintCategory != advisors.CategoryPerformance {
		return errf(http.StatusBadRequest, "lint_category must be PERFORMANCE or SECURITY")
	}
	if in.LintName != nil && (*in.LintName == "" || len(*in.LintName) > 100 || !advisorNameRe.MatchString(*in.LintName)) {
		return errf(http.StatusBadRequest, "invalid lint_name")
	}
	if in.AssignedTo != nil && *in.AssignedTo != "" && !uuidRe.MatchString(*in.AssignedTo) {
		return errf(http.StatusBadRequest, "assigned_to must be a UUID")
	}
	if in.Note != nil && len(*in.Note) > 2000 {
		return errf(http.StatusBadRequest, "note is limited to 2000 characters")
	}
	if len(in.LintMetadata) > 50 {
		return errf(http.StatusBadRequest, "lint_metadata is limited to 50 keys")
	}
	if b, _ := json.Marshal(in.LintMetadata); len(b) > 4096 {
		return errf(http.StatusBadRequest, "lint_metadata is limited to 4096 bytes")
	}
	return nil
}

func (s *Server) createAdvisorExceptions(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Exceptions []exceptionIn `json:"exceptions"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if len(in.Exceptions) == 0 || len(in.Exceptions) > maxExceptionsPer {
		return errf(http.StatusBadRequest, "exceptions needs between 1 and %d rules", maxExceptionsPer)
	}
	user := ""
	if pr := principalFrom(r.Context()); pr != nil {
		user = pr.UserID
	}
	rules := make([]registry.AdvisorException, 0, len(in.Exceptions))
	for _, e := range in.Exceptions {
		if err := e.check(); err != nil {
			return err
		}
		if e.IsDisabled == nil {
			return errf(http.StatusBadRequest, "is_disabled is required")
		}
		rules = append(rules, registry.AdvisorException{
			LintCategory: deref(e.LintCategory), LintName: deref(e.LintName), LintMetadata: e.LintMetadata,
			AssignedTo: deref(e.AssignedTo), IsDisabled: *e.IsDisabled, LastUpdatedBy: user, Note: deref(e.Note),
		})
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	st, err := s.advisorRulesStore()
	if err != nil {
		return err
	}
	created, err := st.CreateAdvisorExceptions(r.Context(), p.Ref, rules)
	switch {
	case errors.Is(err, registry.ErrConflict):
		return errf(http.StatusBadRequest, "A project holds at most %d advisor rules", registry.MaxAdvisorExceptions)
	case err != nil:
		return err
	}
	writeJSON(w, http.StatusCreated, exceptionsJSON(created))
	return nil
}

// splitIDs reads the comma-separated UUIDs of the ids query parameter.
func splitIDs(raw string) ([]string, error) {
	var ids []string
	for _, id := range strings.Split(raw, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !uuidRe.MatchString(id) {
			return nil, errf(http.StatusBadRequest, "ids must be a comma separated list of UUIDs")
		}
		ids = append(ids, strings.ToLower(id))
	}
	if len(ids) == 0 {
		return nil, errf(http.StatusBadRequest, "ids must be a comma separated list of UUIDs")
	}
	if len(ids) > maxExceptionsPer*4 {
		return nil, errf(http.StatusBadRequest, "ids is limited to %d rules", maxExceptionsPer*4)
	}
	return ids, nil
}

func (s *Server) deleteAdvisorExceptions(w http.ResponseWriter, r *http.Request) error {
	ids, err := splitIDs(r.URL.Query().Get("ids"))
	if err != nil {
		return err
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	st, err := s.advisorRulesStore()
	if err != nil {
		return err
	}
	if _, err := st.DeleteAdvisorExceptions(r.Context(), p.Ref, ids); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) updateAdvisorException(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		return errf(http.StatusBadRequest, "id must be a UUID")
	}
	var in exceptionIn
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := in.check(); err != nil {
		return err
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	st, err := s.advisorRulesStore()
	if err != nil {
		return err
	}
	patch := registry.AdvisorExceptionPatch{
		LintCategory: in.LintCategory, LintName: in.LintName, AssignedTo: in.AssignedTo,
		IsDisabled: in.IsDisabled, Note: in.Note, LintMetadata: in.LintMetadata,
	}
	if pr := principalFrom(r.Context()); pr != nil {
		patch.LastUpdatedBy = pr.UserID
	}
	switch err := st.UpdateAdvisorException(r.Context(), p.Ref, strings.ToLower(id), patch); {
	case errors.Is(err, registry.ErrNotFound):
		return errf(http.StatusNotFound, "Advisor rule not found")
	case err != nil:
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}
