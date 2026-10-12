package api

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// The Data API pages of Studio read the project's PostgREST through the Management API:
//
//   - GET /platform/projects/{ref}/api/rest returns PostgREST's OpenAPI (swagger) document. Integrations >
//     Data API > Docs, the API docs side panel and the table and function lists they draw all read its
//     "definitions" and "paths". A stub answers 200 with no body, which Studio reads as a TypeError.
//   - HEAD on the same route is Studio's readiness ping (lib/pingPostgrest.ts): 200 while PostgREST
//     answers, 503 when it does not, so a project whose PostgREST is down does not look ready.
//   - POST /platform/projects/{ref}/api/graphql is GraphiQL's endpoint (Integrations > GraphQL). It calls the
//     project's graphql_public.graphql() function through PostgREST, as the project's own /graphql/v1 does
//     (internal/proxy: /rpc/graphql with Content-Profile graphql_public).
//
// Both call PostgREST with the project's service_role key. GraphiQL asks for a role through
// x-graphql-authorization (the role-impersonation plugin signs a token with the project's JWT secret); when
// the plugin is idle Studio sends the dashboard user's own access token there, which belongs to the
// dashboard's GoTrue and which the project's PostgREST would refuse. So the header is used only when it is
// a token that verifies against the project's JWT secret; anything else runs as service_role, which is what
// hosted's dashboard does for a user who impersonates nobody.

// errNoPostgREST is what a call to a project's PostgREST gets when nothing answers.
var errNoPostgREST = errf(http.StatusServiceUnavailable, "PostgREST is not running for this project")

// restDocument answers GET and HEAD /platform/projects/{ref}/api/rest.
func (s *Server) restDocument(w http.ResponseWriter, r *http.Request) error {
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	keys, err := s.mgr.Keys(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, s.upstream(p, upPostgREST)+"/", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+keys.ServiceRoleKey)
	req.Header.Set("apikey", keys.ServiceRoleKey)
	req.Header.Set("Accept", "application/openapi+json")
	resp, err := s.hc.Do(req)
	if err != nil {
		s.log.Warn("PostgREST unreachable", "ref", p.Ref, "err", err)
		return errNoPostgREST
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		// 503 is what PostgREST answers while it loads the schema cache or cannot reach the database.
		return errf(http.StatusServiceUnavailable, "PostgREST is not ready for this project (it answered %d)", resp.StatusCode)
	}
	// PostgREST types the document application/openapi+json; Studio's client parses the body as JSON either way.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
	return nil
}

// graphql answers POST /platform/projects/{ref}/api/graphql.
func (s *Server) graphql(w http.ResponseWriter, r *http.Request) error {
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	keys, err := s.mgr.Keys(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return errf(http.StatusBadRequest, "The request body could not be read")
	}
	bearer := keys.ServiceRoleKey
	if tok := bearerToken(r.Header.Get("x-graphql-authorization")); tok != "" && verifiesWith(tok, keys.JWTSecret) {
		bearer = tok
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.upstream(p, upPostgREST)+"/rpc/graphql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Forced, never taken from the client: another profile would call graphql() in another schema.
	req.Header.Set("Content-Profile", "graphql_public")
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("apikey", bearer)
	resp, err := s.hc.Do(req)
	if err != nil {
		s.log.Warn("PostgREST unreachable", "ref", p.Ref, "err", err)
		return errNoPostgREST
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return nil
}

// bearerToken returns the token of an "Authorization: Bearer <token>" value, or "".
func bearerToken(v string) string {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(v), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

// verifiesWith reports whether tok is an unexpired HS256 token signed with secret. Nothing else is looked
// at: the claims (role, sub) are PostgREST's to read, and a token that verifies is one that whoever holds the
// project's JWT secret could have made anyway.
func verifiesWith(tok, secret string) bool {
	if secret == "" {
		return false
	}
	_, err := jwt.Parse(tok, func(*jwt.Token) (any, error) { return []byte(secret), nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	return err == nil
}

func (s *Server) routesRestGraphQL(add func(string, handlerFunc)) {
	add("GET /platform/projects/{ref}/api/rest", s.restDocument)
	add("POST /platform/projects/{ref}/api/graphql", s.graphql)
}
