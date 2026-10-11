package api

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/secrets"
)

// The legacy JWT secret. Studio's Project settings, JWT keys, Legacy JWT secret page changes a
// project's secret with PATCH /platform/projects/{ref}/config/secrets and follows the change
// with GET .../config/secrets/update-status. Hosted accepts a secret of the caller's own
// ("Create my own secret") or the word ROLL ("Generate a random secret"), signs new anon and
// service_role keys with it, and restarts the project's services in the background. Here
// lifecycle.Engine.SetJWTSecret does the same work before the request answers, so the status
// is already final when Studio first asks; a request that arrives while the work runs sees
// "Updating".
//
// This is also how a project moved from hosted Supabase gets the hosted secret when it was
// created without it (`supavise projects create --jwt-secret-file` is the create-time way).
// Hosted's JWT signing keys (asymmetric keys, importing a key) are not supported: the routes
// that would change them answer 501 and the legacy key answers 404, as on a hosted project
// that has not migrated its secret to a signing key. That answer is what makes Studio offer
// "Change legacy JWT secret".

// rollSecret is what Studio sends for "Generate a random secret".
const rollSecret = "ROLL"

// The numeric codes of Studio's JwtSecretUpdateStatus, JwtSecretUpdateProgress and
// JwtSecretUpdateError (@supabase/shared-types), the values update-status carries.
const (
	jwtStatusUpdating = 0
	jwtStatusUpdated  = 1
	jwtStatusFailed   = 2

	jwtProgressStarted              = 0 // Started
	jwtProgressConfigUpdated        = 2 // UpdatedAPIServicesConfiguration
	jwtProgressServicesRestarted    = 3 // RestartedAPIServices
	jwtProgressGatewayConfigUpdated = 5 // UpdatedAPIGatewayConfiguration

	jwtErrorConfigUpdate    = 1 // APIServicesConfigurationUpdateFailed
	jwtErrorServicesRestart = 2 // APIServicesRestartFailed
	jwtErrorGatewayUpdate   = 5 // APIGatewayUpdateFailed
)

const (
	jwtTrackingIDMaxLen = 128

	signingKeysUnsupportedMessage = "Supavise projects sign JWTs with the legacy JWT secret, so JWT signing keys are not available. To change the secret, use Project settings, JWT keys, Legacy JWT secret."
	legacyNotMigratedMessage      = "The legacy JWT secret has not been migrated to a signing key."
)

// jwtUpdate is the last JWT secret change of one project, as update-status reports it.
type jwtUpdate struct {
	trackingID string
	status     int
	progress   int
	errCode    *int
	at         time.Time
}

// jwtUpdates holds the last change of each project in memory. A restart of the API server
// forgets it, and update-status then answers null, which Studio reads as "nothing to show".
type jwtUpdates struct {
	mu sync.Mutex
	m  map[string]jwtUpdate
}

func (t *jwtUpdates) get(ref string) (jwtUpdate, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	u, ok := t.m[ref]
	return u, ok
}

func (t *jwtUpdates) put(ref string, u jwtUpdate) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]jwtUpdate{}
	}
	t.m[ref] = u
}

func (t *jwtUpdates) drop(ref string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, ref)
}

func (s *Server) routesJWTSecret(add func(string, handlerFunc)) {
	add("PATCH /platform/projects/{ref}/config/secrets", s.updateJWTSecret)
	add("GET /platform/projects/{ref}/config/secrets/update-status", s.jwtSecretUpdateStatus)
	add("GET /v1/projects/{ref}/config/auth/signing-keys/legacy", s.legacySigningKey)
	add("POST /v1/projects/{ref}/config/auth/signing-keys", s.signingKeysUnsupported)
	add("POST /v1/projects/{ref}/config/auth/signing-keys/legacy", s.signingKeysUnsupported)
	add("PATCH /v1/projects/{ref}/config/auth/signing-keys/{id}", s.signingKeysUnsupported)
	add("DELETE /v1/projects/{ref}/config/auth/signing-keys/{id}", s.signingKeysUnsupported)
}

// updateJWTSecret changes the project's legacy JWT secret. The body is
// {jwt_secret, change_tracking_id}: jwt_secret is a secret of at least 32 characters or ROLL.
// The anon and service_role keys are signed again with it and the project's GoTrue, PostgREST,
// Realtime and Storage tenants, its Edge Functions and the proxy follow; users' refresh tokens
// stay valid, their access tokens (signed with the old secret) do not.
func (s *Server) updateJWTSecret(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in struct {
		JWTSecret        string `json:"jwt_secret"`
		ChangeTrackingID string `json:"change_tracking_id"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.JWTSecret == "" {
		return errf(http.StatusBadRequest, "jwt_secret is required")
	}
	secret := in.JWTSecret
	if secret == rollSecret {
		secret = secrets.NewJWTSecret()
	} else if err := secrets.CheckJWTSecret(secret); err != nil {
		return errf(http.StatusBadRequest, "%v", err)
	}
	setter, ok := s.mgr.(lifecycle.JWTSecretSetter)
	if !ok {
		return errf(http.StatusNotImplemented, "This server cannot change the JWT secret")
	}
	done, err := s.beginOp()
	if err != nil {
		return err
	}
	defer done()
	ctx, cancel := detached(r, applyTimeout)
	defer cancel()

	tracking := in.ChangeTrackingID
	if len(tracking) > jwtTrackingIDMaxLen {
		tracking = tracking[:jwtTrackingIDMaxLen]
	}
	prev, hadPrev := s.jwtUpdates.get(p.Ref)
	s.jwtUpdates.put(p.Ref, jwtUpdate{trackingID: tracking, status: jwtStatusUpdating, progress: jwtProgressStarted, at: s.now()})
	_, err = setter.SetJWTSecret(ctx, p.Ref, secret)
	if err != nil {
		var je *lifecycle.JWTSecretError
		if !errors.As(err, &je) {
			// Refused before anything was applied (the project is paused for a move, being
			// upgraded, homed on another node): the last change stays what it was.
			if hadPrev {
				s.jwtUpdates.put(p.Ref, prev)
			} else {
				s.jwtUpdates.drop(p.Ref)
			}
			if errors.Is(err, lifecycle.ErrInvalidState) {
				return errf(http.StatusConflict, "%v", err)
			}
			s.log.Error("JWT secret change failed", "ref", p.Ref, "err", err)
			return errf(http.StatusInternalServerError, "Failed to update the JWT secret")
		}
		code, progress := jwtErrorServicesRestart, jwtProgressConfigUpdated
		switch je.Step {
		case lifecycle.KeyStepStore:
			code, progress = jwtErrorConfigUpdate, jwtProgressStarted
		case lifecycle.KeyStepTenants:
			code, progress = jwtErrorGatewayUpdate, jwtProgressServicesRestarted
		}
		s.jwtUpdates.put(p.Ref, jwtUpdate{trackingID: tracking, status: jwtStatusFailed, progress: progress, errCode: &code, at: s.now()})
		s.log.Error("JWT secret change failed", "ref", p.Ref, "step", je.Step, "err", je.Err)
		return errf(http.StatusInternalServerError, "Failed to update the JWT secret; the previous secret is back in use")
	}
	s.jwtUpdates.put(p.Ref, jwtUpdate{trackingID: tracking, status: jwtStatusUpdated, progress: jwtProgressGatewayConfigUpdated, at: s.now()})
	// The Edge Functions read the secret from a file: write it now instead of at the next
	// reconcile. A failure is logged and repeated by the reconcile; the change itself is done.
	s.functionsGone(ctx, p.Ref)
	s.log.Info("JWT secret changed", "ref", p.Ref)

	resp := base("PATCH /platform/projects/{ref}/config/secrets")
	set(resp, "message", "Successfully updated JWT secret")
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// jwtSecretUpdateStatus reports the last change of the project's JWT secret, or null.
func (s *Server) jwtSecretUpdateStatus(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	resp := map[string]any{"update_status": nil}
	if u, ok := s.jwtUpdates.get(p.Ref); ok {
		st := map[string]any{"change_tracking_id": u.trackingID, "status": u.status, "progress": u.progress}
		if u.errCode != nil {
			st["error"] = *u.errCode
		}
		resp["update_status"] = st
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// legacySigningKey answers 404: no project here has its legacy secret migrated to a JWT
// signing key. Studio shows "Change legacy JWT secret" only when this call fails.
func (s *Server) legacySigningKey(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.loadProject(r.Context(), r.PathValue("ref")); err != nil {
		return err
	}
	return errf(http.StatusNotFound, legacyNotMigratedMessage)
}

// signingKeysUnsupported refuses the writes on JWT signing keys instead of answering the
// spec-derived stub, which reports a key that nothing signs with.
func (s *Server) signingKeysUnsupported(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.loadProject(r.Context(), r.PathValue("ref")); err != nil {
		return err
	}
	return errf(http.StatusNotImplemented, signingKeysUnsupportedMessage)
}
