package api

import "net/http"

// The pgsodium root key. Supabase's guide to moving a project between projects copies it with
//
//	GET /v1/projects/{old}/pgsodium | PUT /v1/projects/{new}/pgsodium
//
// so that Vault secrets and columns encrypted under it can be read after a restore. Here the
// key is each cluster's own (project_secrets, written to the cluster's key file when it is
// created) and GET returns it. PUT is refused: replacing the key of a running cluster needs a
// restart and leaves what the old key encrypted unreadable, and the spec-derived stub that
// stood in for it answered 200 and changed nothing, so a project moved with Supabase's guide
// looked as if its Vault had come along. (The data dump of `supabase db dump` leaves the
// vault and pgsodium schemas out in any case.)
const pgsodiumPutMessage = "Supavise does not replace the pgsodium root key of a project, so Vault secrets and columns that were encrypted under another key cannot be read here. Create them again in this project."

func (s *Server) routesPgsodium(add func(string, handlerFunc)) {
	add("GET /v1/projects/{ref}/pgsodium", s.getPgsodiumKey)
	add("PUT /v1/projects/{ref}/pgsodium", s.putPgsodiumKey)
}

func (s *Server) getPgsodiumKey(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	keys, err := s.mgr.Keys(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	if keys.PGSodiumRootKey == "" {
		return errf(http.StatusNotFound, "This project has no pgsodium root key")
	}
	resp := base("GET /v1/projects/{ref}/pgsodium")
	set(resp, "root_key", keys.PGSodiumRootKey)
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) putPgsodiumKey(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.loadProject(r.Context(), r.PathValue("ref")); err != nil {
		return err
	}
	return errf(http.StatusNotImplemented, pgsodiumPutMessage)
}
