package api

import (
	_ "embed"
	"net/http"
)

// webhooksSQL turns Database Webhooks on in a project's database: pg_net, the supabase_functions
// schema with its hooks table and http_request() trigger function, and the roles and grants that
// function needs. It is upstream's docker/volumes/db/webhooks.sql with the changes its header
// lists (it can run again, it leaves pg_net's grants as the postgres artifact sets them, and it runs
// no code that a project wrote: see the rule in the header).
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
//
// A Developer-role member can run SQL as postgres, so the database may hold a schema, a table with a
// trigger or a role of that member's before this runs. The script is written so that none of it runs
// as supabase_admin: it refuses a supabase_functions schema that supabase_admin does not own and
// writes rows only into a table it creates itself (webhooks.sql, header points 4 to 6).
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
