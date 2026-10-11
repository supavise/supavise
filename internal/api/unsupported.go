package api

import (
	"net/http"
)

// Hosted features that this server does not offer but whose dashboard pages are still there. A stub
// answers such a call with the smallest valid body, which Studio takes for success: it toasts "Successfully
// updated", shows a key that does not exist or reports a JWT secret that is "updating" for ever. These
// handlers answer what is true instead, in the way Studio already handles on hosted:
//
//   - a refusal with a message the dashboard shows as the reason (4xx: Studio does not retry those);
//   - for SSL enforcement, the "not allowed to configure SSL enforcements" answer that hosted gives to a
//     project without access to the feature, which Studio renders as a disabled switch;
//   - for the legacy signing key, 404, which Studio renders as "Start using JWT signing keys".
//
// docs/reference/studio-platform-calls.md lists them under "Hosted features this server refuses".

func (s *Server) routesUnsupported(add func(string, handlerFunc)) {
	// SSL enforcement. Studio matches this text (data/ssl-enforcement/ssl-enforcement-query.ts).
	sslMsg := "This project is not allowed to configure SSL enforcements: Supavise does not enforce SSL on database connections."
	add("GET /v1/projects/{ref}/ssl-enforcement", s.refuse(http.StatusBadRequest, sslMsg))
	add("PUT /v1/projects/{ref}/ssl-enforcement", s.refuse(http.StatusBadRequest, sslMsg))

	// JWT secret. The page's polling endpoint says that no change is running; a stub says "updating" (status 0).
	add("GET /platform/projects/{ref}/config/secrets/update-status", s.jwtSecretUpdateStatus)
	add("PATCH /platform/projects/{ref}/config/secrets", func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.loadProject(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		return errf(http.StatusBadRequest, "Supavise cannot change a project's JWT secret from the dashboard. "+
			"On the server, run `supavise projects rotate-keys %s` to issue a new secret and new API keys.", p.Ref)
	})

	// JWT signing keys. A project signs its tokens with the JWT secret (HS256): there are no signing keys, the
	// legacy secret has not been migrated, and asymmetric keys are not offered.
	const signingMsg = "Supavise signs a project's tokens with its JWT secret and does not support asymmetric JWT signing keys."
	add("GET /v1/projects/{ref}/config/auth/signing-keys/legacy", s.refuse(http.StatusNotFound, "Legacy signing key not found: this project signs tokens with its JWT secret."))
	add("POST /v1/projects/{ref}/config/auth/signing-keys/legacy", s.refuse(http.StatusBadRequest, signingMsg))
	add("POST /v1/projects/{ref}/config/auth/signing-keys", s.refuse(http.StatusBadRequest, signingMsg))
	for _, m := range []string{"GET", "PATCH", "DELETE"} {
		add(m+" /v1/projects/{ref}/config/auth/signing-keys/{id}", s.refuse(http.StatusNotFound, "Signing key not found."))
	}

	// Third-party auth integrations (Clerk, Auth0, Firebase and others): the services of a node accept the
	// project's own JWTs only, and Supavise has no code that hands a JWKS to them. The page is not hidden
	// (it could be built, and hosted offers it to every project): the list is empty, which is true, and an
	// add is refused with the reason, which Studio shows in the mutation's error toast.
	add("POST /v1/projects/{ref}/config/auth/third-party-auth", s.refuse(http.StatusBadRequest, "Supavise does not support third-party auth integrations: the project accepts tokens signed with its own JWT secret only."))
	for _, m := range []string{"GET", "DELETE"} {
		add(m+" /v1/projects/{ref}/config/auth/third-party-auth/{tpa_id}", s.refuse(http.StatusNotFound, "Third-party auth integration not found."))
	}

	// The Auth overview's usage numbers come from the logs. Studio shows "There was an error fetching the auth
	// metrics" for an error and zero sign-ups and zero active users for an empty answer.
	add("GET /platform/projects/{ref}/analytics/endpoints/auth.metrics", func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.loadProject(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		return errf(http.StatusBadRequest, "Supavise does not collect the Auth usage metrics, because it has no log store. "+
			"%s, run `supavise logs %s --service auth` to read the Auth log.", s.logsHost(r.Context(), p), p.Ref)
	})

	// The dashboard's Feedback button posts here; the message would go nowhere.
	add("POST /platform/feedback/send", s.refuse(http.StatusBadRequest, "This dashboard does not send feedback anywhere. To report a problem with Supavise, open an issue at https://github.com/supavise/supavise."))
}

// refuse answers every call of its route with status and message, after the project is known to exist.
func (s *Server) refuse(status int, msg string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if ref := r.PathValue("ref"); ref != "" {
			if _, err := s.loadProject(r.Context(), ref); err != nil {
				return err
			}
		}
		return errf(status, "%s", msg)
	}
}

// jwtSecretUpdateStatus answers the poll of the JWT secret page. No change of the secret runs on a node
// (PATCH config/secrets is refused), so update_status is null, as hosted answers a project that never had one.
func (s *Server) jwtSecretUpdateStatus(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.loadProject(r.Context(), r.PathValue("ref")); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"update_status": nil}) // plat.GetJwtSecretUpdateStatusOutput; the field is nullable
	return nil
}
