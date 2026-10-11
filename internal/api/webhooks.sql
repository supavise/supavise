-- Database Webhooks: the supabase_functions schema, its http_request() trigger function and the
-- pg_net grants the function needs. POST /platform/database/{ref}/hook-enable and
-- POST /v1/projects/{ref}/database/webhooks/enable run this file as supabase_admin (internal/api/webhooks.go).
--
-- Provenance: supabase/supabase, docker/volumes/db/webhooks.sql at commit
-- 8617532ae1a41571d5d557cd24c6f5f2565d9230 (Apache-2.0, Copyright Supabase, Inc.). The file is changed
-- as follows; every other statement is upstream's:
--   1. It can run again. Upstream runs once on an empty database, so its CREATE SCHEMA, CREATE TABLE,
--      CREATE INDEX, CREATE FUNCTION and the two version inserts failed on a second run. They are
--      IF NOT EXISTS, OR REPLACE and ON CONFLICT DO NOTHING here (points 4 and 5 change two of them).
--   2. The pg_net block that sets net.http_get and net.http_post to SECURITY DEFINER, pins their
--      search_path and revokes them from PUBLIC runs only for the pg_net versions that migration
--      20250220051611_pg_net_perms_fix of the postgres artifact lists (0.11.0 and older). The pinned
--      artifact ships pg_net 0.20.4, where that migration undoes those changes on purpose: the
--      functions are SECURITY INVOKER and executable by PUBLIC, as on a hosted project.
--   3. It does not redefine extensions.grant_pg_net_access() or create the issue_pg_net_access event
--      trigger. The artifact already has both, and its function is newer than upstream's (it applies
--      the grants of point 2 only to the old versions). Replacing it would apply them again at the next
--      CREATE EXTENSION pg_net.
--   4. It refuses to run over a supabase_functions schema that supabase_admin does not own. Upstream runs
--      on a database nobody has touched; here a project member (the postgres role) can create a schema of
--      that name first, and everything below runs as a superuser.
--   5. It writes rows only into a migrations table that this run creates. In a table a project has already
--      had its hands on (the postgres role holds privileges on the tables of this schema once they exist),
--      an INSERT would run the project's triggers, column defaults and rules as a superuser.
--   6. It does not drop supabase_pg_net_admin. No migration of supabase/postgres (where the artifact's
--      migrations come from) mentions that role, so a project that has it made it itself, and upstream's
--      REASSIGN OWNED would give supabase_admin the SECURITY DEFINER functions the project wrote under it.
-- The rule behind 4 to 6: nothing in this file may execute code that a project could have written (a
-- trigger, a column default, a rule, a function it owns) while it runs as supabase_admin. Everything else
-- here is DDL and GRANT on names this file owns, which runs no project code.
-- Check this file against the artifact's migrations when the postgres pin or pg_net moves.
BEGIN;
  -- Create pg_net extension
  CREATE EXTENSION IF NOT EXISTS pg_net SCHEMA extensions;
  -- Create supabase_functions schema, or refuse one that a project made (header point 4). The CREATE is
  -- not IF NOT EXISTS: a schema that appears between the check and the CREATE fails the script.
  DO
  $$
  DECLARE
    schema_owner name;
  BEGIN
    SELECT pg_get_userbyid(nspowner) INTO schema_owner FROM pg_namespace WHERE nspname = 'supabase_functions';
    IF NOT FOUND THEN
      CREATE SCHEMA supabase_functions AUTHORIZATION supabase_admin;
    ELSIF schema_owner <> 'supabase_admin' THEN
      RAISE EXCEPTION 'The schema supabase_functions already exists and belongs to %, not to supabase_admin, so Supavise does not run the webhooks script over it. Drop or rename the schema, then enable webhooks again.', schema_owner;
    END IF;
  END
  $$;
  GRANT USAGE ON SCHEMA supabase_functions TO postgres, anon, authenticated, service_role;
  ALTER DEFAULT PRIVILEGES IN SCHEMA supabase_functions GRANT ALL ON TABLES TO postgres, anon, authenticated, service_role;
  ALTER DEFAULT PRIVILEGES IN SCHEMA supabase_functions GRANT ALL ON FUNCTIONS TO postgres, anon, authenticated, service_role;
  ALTER DEFAULT PRIVILEGES IN SCHEMA supabase_functions GRANT ALL ON SEQUENCES TO postgres, anon, authenticated, service_role;
  -- supabase_functions.migrations definition, with upstream's two version rows ('initial' here and
  -- '20210809183423_update_grants' where upstream inserts it, after the grants). The rows go in only when
  -- this run creates the table (header point 5); a table that exists is left as it is.
  DO
  $$
  BEGIN
    IF to_regclass('supabase_functions.migrations') IS NULL THEN
      CREATE TABLE supabase_functions.migrations (
        version text PRIMARY KEY,
        inserted_at timestamptz NOT NULL DEFAULT NOW()
      );
      INSERT INTO supabase_functions.migrations (version) VALUES ('initial'), ('20210809183423_update_grants');
    END IF;
  END
  $$;
  -- supabase_functions.hooks definition
  CREATE TABLE IF NOT EXISTS supabase_functions.hooks (
    id bigserial PRIMARY KEY,
    hook_table_id integer NOT NULL,
    hook_name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT NOW(),
    request_id bigint
  );
  CREATE INDEX IF NOT EXISTS supabase_functions_hooks_request_id_idx ON supabase_functions.hooks USING btree (request_id);
  CREATE INDEX IF NOT EXISTS supabase_functions_hooks_h_table_id_h_name_idx ON supabase_functions.hooks USING btree (hook_table_id, hook_name);
  COMMENT ON TABLE supabase_functions.hooks IS 'Supabase Functions Hooks: Audit trail for triggered hooks.';
  CREATE OR REPLACE FUNCTION supabase_functions.http_request()
    RETURNS trigger
    LANGUAGE plpgsql
    AS $function$
    DECLARE
      request_id bigint;
      payload jsonb;
      url text := TG_ARGV[0]::text;
      method text := TG_ARGV[1]::text;
      headers jsonb DEFAULT '{}'::jsonb;
      params jsonb DEFAULT '{}'::jsonb;
      timeout_ms integer DEFAULT 1000;
    BEGIN
      IF url IS NULL OR url = 'null' THEN
        RAISE EXCEPTION 'url argument is missing';
      END IF;

      IF method IS NULL OR method = 'null' THEN
        RAISE EXCEPTION 'method argument is missing';
      END IF;

      IF TG_ARGV[2] IS NULL OR TG_ARGV[2] = 'null' THEN
        headers = '{"Content-Type": "application/json"}'::jsonb;
      ELSE
        headers = TG_ARGV[2]::jsonb;
      END IF;

      IF TG_ARGV[3] IS NULL OR TG_ARGV[3] = 'null' THEN
        params = '{}'::jsonb;
      ELSE
        params = TG_ARGV[3]::jsonb;
      END IF;

      IF TG_ARGV[4] IS NULL OR TG_ARGV[4] = 'null' THEN
        timeout_ms = 1000;
      ELSE
        timeout_ms = TG_ARGV[4]::integer;
      END IF;

      CASE
        WHEN method = 'GET' THEN
          SELECT http_get INTO request_id FROM net.http_get(
            url,
            params,
            headers,
            timeout_ms
          );
        WHEN method = 'POST' THEN
          payload = jsonb_build_object(
            'old_record', OLD,
            'record', NEW,
            'type', TG_OP,
            'table', TG_TABLE_NAME,
            'schema', TG_TABLE_SCHEMA
          );

          SELECT http_post INTO request_id FROM net.http_post(
            url,
            payload,
            params,
            headers,
            timeout_ms
          );
        ELSE
          RAISE EXCEPTION 'method argument % is invalid', method;
      END CASE;

      INSERT INTO supabase_functions.hooks
        (hook_table_id, hook_name, request_id)
      VALUES
        (TG_RELID, TG_NAME, request_id);

      RETURN NEW;
    END
  $function$;
  -- Supabase super admin
  DO
  $$
  BEGIN
    IF NOT EXISTS (
      SELECT 1
      FROM pg_roles
      WHERE rolname = 'supabase_functions_admin'
    )
    THEN
      CREATE USER supabase_functions_admin NOINHERIT CREATEROLE LOGIN NOREPLICATION;
    END IF;
  END
  $$;
  GRANT ALL PRIVILEGES ON SCHEMA supabase_functions TO supabase_functions_admin;
  GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA supabase_functions TO supabase_functions_admin;
  GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA supabase_functions TO supabase_functions_admin;
  ALTER USER supabase_functions_admin SET search_path = "supabase_functions";
  ALTER table "supabase_functions".migrations OWNER TO supabase_functions_admin;
  ALTER table "supabase_functions".hooks OWNER TO supabase_functions_admin;
  ALTER function "supabase_functions".http_request() OWNER TO supabase_functions_admin;
  GRANT supabase_functions_admin TO postgres;
  -- http_request() is SECURITY DEFINER and runs as supabase_functions_admin. Serializing
  -- NEW/OLD can touch objects in the extensions schema (PostGIS geometry reads
  -- extensions.spatial_ref_sys), so the role needs USAGE on it.
  GRANT USAGE ON SCHEMA extensions TO supabase_functions_admin;
  -- Upstream removes the unused supabase_pg_net_admin role here; this file does not (header point 6).
  -- pg_net grants when extension is already enabled
  DO
  $$
  BEGIN
    IF EXISTS (
      SELECT 1
      FROM pg_extension
      WHERE extname = 'pg_net'
    )
    THEN
      GRANT USAGE ON SCHEMA net TO supabase_functions_admin, postgres, anon, authenticated, service_role;
    END IF;
    -- Only the pg_net versions that need them (see the header, point 2).
    IF EXISTS (
      SELECT 1
      FROM pg_extension
      WHERE extname = 'pg_net'
      AND extversion IN ('0.2', '0.6', '0.7', '0.7.1', '0.8', '0.10.0', '0.11.0')
    )
    THEN
      ALTER function net.http_get(url text, params jsonb, headers jsonb, timeout_milliseconds integer) SECURITY DEFINER;
      ALTER function net.http_post(url text, body jsonb, params jsonb, headers jsonb, timeout_milliseconds integer) SECURITY DEFINER;
      ALTER function net.http_get(url text, params jsonb, headers jsonb, timeout_milliseconds integer) SET search_path = net;
      ALTER function net.http_post(url text, body jsonb, params jsonb, headers jsonb, timeout_milliseconds integer) SET search_path = net;
      REVOKE ALL ON FUNCTION net.http_get(url text, params jsonb, headers jsonb, timeout_milliseconds integer) FROM PUBLIC;
      REVOKE ALL ON FUNCTION net.http_post(url text, body jsonb, params jsonb, headers jsonb, timeout_milliseconds integer) FROM PUBLIC;
      GRANT EXECUTE ON FUNCTION net.http_get(url text, params jsonb, headers jsonb, timeout_milliseconds integer) TO supabase_functions_admin, postgres, anon, authenticated, service_role;
      GRANT EXECUTE ON FUNCTION net.http_post(url text, body jsonb, params jsonb, headers jsonb, timeout_milliseconds integer) TO supabase_functions_admin, postgres, anon, authenticated, service_role;
    END IF;
  END
  $$;
  -- Upstream's migration row '20210809183423_update_grants' is inserted with the table (above).
  ALTER function supabase_functions.http_request() SECURITY DEFINER;
  ALTER function supabase_functions.http_request() SET search_path = supabase_functions;
  REVOKE ALL ON FUNCTION supabase_functions.http_request() FROM PUBLIC;
  GRANT EXECUTE ON FUNCTION supabase_functions.http_request() TO postgres, anon, authenticated, service_role;
COMMIT;
