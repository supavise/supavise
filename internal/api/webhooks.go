package api

import (
	_ "embed"
	"net/http"
)

// webhooksSQL turns Database Webhooks on in a project's database: pg_net, the supabase_functions
// schema with its hooks table and http_request() trigger function, and the roles and grants that
// function needs. It is upstream's docker/volumes/db/webhooks.sql with the changes its header
// lists (it can run again, and it leaves pg_net's grants as the postgres artifact sets them).
//
//go:embed webhooks.sql
var webhooksSQL string

func (s *Server) routesWebhooks(add func(string, handlerFunc)) {
	add("POST /platform/database/{ref}/hook-enable", s.enableWebhooks)
	add("POST /v1/projects/{ref}/database/webhooks/enable", s.enableWebhooks)
}

// enableWebhooks is what the dashboard's "Enable webhooks" button and the Management API's
// enable-database-webhook call. Studio then finds the supabase_functions schema, offers the
// Create webhook form, and creates and drops the triggers through pg-meta as the postgres role.
//
// The script runs as supabase_admin because it creates a schema, a role and an extension and
// hands the trigger function to that role. It can run again (a second click, a retry after a
// timeout) and then only repairs what is missing.
func (s *Server) enableWebhooks(w http.ResponseWriter, r *http.Request) error {
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	if _, err := s.sqlRows(r.Context(), p.Ref, "supabase_admin", false, webhooksSQL); err != nil {
		return err
	}
	w.WriteHeader(http.StatusCreated)
	return nil
}
