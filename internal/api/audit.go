package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/supavise/supavise/internal/registry"
)

// The audit log. Studio's organization Audit Logs page (and Account > Audit Logs for a person's own history)
// lists the requests that changed something, with the time, who made them, from where, the route and how it
// ended. wrap() records one event for each request that passed authentication and used a method that changes
// state, except the routes in auditSkip, and the two endpoints below list them in the shape of the platform
// spec.
//
//   - What is stored: the route template, its path parameters (never a parameter that names a token or a
//     secret), the status, the caller (user id and email, how the request was authenticated, and for an OAuth
//     client its app), the client address and the request id. Never a request body or a query string: they
//     carry data and secrets.
//   - Which requests: a POST, PUT, PATCH or DELETE of an authenticated caller. Reads are not recorded. Neither
//     are the calls of the data plane, which hosted does not list either and which would flood the table: SQL
//     (pg-meta, the database query routes), saved snippets and reports (autosaved), log queries, storage
//     object operations, GraphQL, telemetry, and the POSTs that only read (auditSkip). A request refused by the
//     role check (403) is recorded: who tried is part of the trail.
//   - Scope: an organization's page lists the events of requests that concern it (a path with its slug, a
//     project of it, a branch of one of its projects, a project or organization created in it), matched on the
//     organization's id; the account page lists the caller's own events. A request that concerns no
//     organization (a profile change) is in the account page only.
//   - Retention: [api] audit_retention_days (90 by default). The API process deletes older rows about once an
//     hour (Prune).
//
// The failure of a write is logged and does not fail the request: the change is made.

// AuditEvent is one recorded request.
type AuditEvent struct {
	ID        int64
	At        time.Time
	RequestID string
	OrgID     int64 // 0: the request concerns no organization
	OrgSlug   string
	// ProjectRef is the project the route names, "" for none. A branch is the ref of the branch.
	ProjectRef string
	UserID     string
	Email      string
	// TokenType is how the request was authenticated, in the words of hosted's spec: "jwt" (a dashboard
	// session), "v1" (a personal access token) or "oauth".
	TokenType    string
	OAuthAppID   string
	OAuthAppName string
	IP           string
	Method       string
	Route        string // the path template
	Name         string
	Status       int
	Params       map[string]string
}

// AuditQuery selects events: those of one organization (OrgID) or of one user (UserID), within [From, To],
// newest first, at most Limit.
type AuditQuery struct {
	OrgID    int64
	UserID   string
	From, To time.Time
	Limit    int
}

// AuditStore keeps the audit log.
type AuditStore interface {
	Record(ctx context.Context, e AuditEvent) error
	List(ctx context.Context, q AuditQuery) ([]AuditEvent, error)
	// Prune deletes the events older than before and returns how many.
	Prune(ctx context.Context, before time.Time) (int64, error)
}

// auditLimit bounds a listing: the newest events of the range. Studio draws them all in one table.
const auditLimit = 5000

// auditMaxRange is the longest span a listing covers, with the five minutes the page adds while it stays
// open (AuditLogs.tsx: "the API limits the logs to maximum of 62 days and 5 minutes"). Studio tests the
// error message for "range exceeded".
const auditMaxRange = 62*24*time.Hour + 5*time.Minute

// auditSkip lists the route templates ("METHOD /path") whose requests are not recorded.
var auditSkip = regexp.MustCompile(`^(?:[A-Z]+ )?(?:` + strings.Join([]string{
	// SQL and schema editing through the dashboard's pg-meta, and the same through the Management API.
	`/platform/pg-meta/`,
	`/v1/projects/\{ref\}/database/query`,
	`/v1/projects/\{ref\}/database/context`,
	// Saved snippets, reports and notebooks are autosaved while someone types.
	`/platform/projects/\{ref\}/content`, `/v1/snippets`, `/v2/projects/\{ref\}/notebooks`,
	// Log and metric queries are POSTs that read.
	`/platform/projects/\{ref\}/analytics/`, `/v1/projects/\{ref\}/analytics/`, `/v2/projects/\{ref\}/analytics/`,
	// The GraphQL explorer and the Storage explorer work on the project's data.
	`/platform/projects/\{ref\}/api/graphql`, `/platform/storage/\{ref\}/buckets/\{id\}/objects`,
	// Telemetry, the feedback form, notifications and the device login of the CLI.
	`/platform/telemetry/`, `/platform/feedback/`, `/platform/notifications`, `/platform/cli/login`,
	// POSTs that only read or check.
	`/platform/projects/\{ref\}/api-keys/temporary`, `/platform/auth/\{ref\}/validate/spam`,
	`/v2/projects/\{ref\}/advisors/run`, `/v1/projects/\{ref\}/network-bans/retrieve`,
	`/v1/projects/\{ref\}/vanity-subdomain/check-availability`, `/platform/organizations/\{slug\}/available-versions`,
	`/platform/organizations/preview-creation`, `/platform/profile/audit-login`,
}, "|") + `)`)

// auditMethods are the methods that change state.
func auditMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// auditSecretParam reports whether a path parameter is not to be stored: an invitation token, for one, is a
// credential that sits in the path.
func auditSecretParam(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "token") || strings.Contains(n, "secret") || strings.Contains(n, "password")
}

// auditBegin decides whether the request is recorded and gathers what must be known before the handler runs
// (a deleted project is gone afterwards). The returned func records the event with the final status; nil means
// the request is not recorded.
func (s *Server) auditBegin(r *http.Request, key string, p *Principal) func(status int) {
	if s.audit == nil || p == nil || key == "" || !auditMutating(r.Method) || auditSkip.MatchString(key) {
		return nil
	}
	ctx := context.WithoutCancel(r.Context())
	_, tmpl, _ := strings.Cut(key, " ")
	e := AuditEvent{
		At: s.now(), RequestID: requestID(r), UserID: p.UserID, Email: p.Email, TokenType: auditTokenType(p),
		IP: auditClientIP(r, s.trustForwarded(r)), Method: r.Method, Route: tmpl, Name: s.auditName(key),
	}
	if p.OAuth != nil {
		e.OAuthAppID, e.OAuthAppName = p.OAuth.AppID, p.OAuth.AppName
		e.Email = "" // an OAuth principal's address is loaded on demand; the user id says who
	}
	e.Params = map[string]string{}
	for _, name := range pathParams(tmpl) {
		if v := r.PathValue(name); v != "" && !auditSecretParam(name) {
			e.Params[name] = v
		}
	}
	s.auditTarget(ctx, r, tmpl, &e)
	return func(status int) {
		e.Status = status
		s.auditRecord(ctx, e)
	}
}

// requestID is the id the middleware gave the request.
func requestID(r *http.Request) string {
	id, _ := r.Context().Value(ctxRequestID).(string)
	if id == "" {
		id = newRequestID()
	}
	return id
}

func auditTokenType(p *Principal) string {
	switch p.Via {
	case "pat":
		return "v1"
	case "oauth":
		return "oauth"
	}
	return "jwt"
}

// auditClientIP is the address the request came from: the peer, or the last entry of X-Forwarded-For when the
// peer is the node's own proxy (trustForwarded, as for the rate limits of the claim flow).
func auditClientIP(r *http.Request, trustForwarded bool) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); trustForwarded && ip != nil && ip.IsLoopback() {
		if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
			if i := strings.LastIndex(xff, ","); i >= 0 {
				xff = strings.TrimSpace(xff[i+1:])
			}
			host = xff
		}
	}
	return host
}

var pathParamRe = regexp.MustCompile(`\{([a-z_]+)\}`)

func pathParams(tmpl string) []string {
	var out []string
	for _, m := range pathParamRe.FindAllStringSubmatch(tmpl, -1) {
		out = append(out, m[1])
	}
	return out
}

// auditTarget fills in the organization and the project the request concerns, the way authorize finds them.
func (s *Server) auditTarget(ctx context.Context, r *http.Request, tmpl string, e *AuditEvent) {
	setOrg := func(o *registry.Organization) {
		if o != nil {
			e.OrgID, e.OrgSlug = o.ID, o.Slug
		}
	}
	setProject := func(p *registry.Project) {
		e.ProjectRef = p.Ref
		if o, err := s.orgOf(ctx, p); err == nil {
			setOrg(o)
		}
	}
	switch {
	case strings.Contains(tmpl, "{ref}"):
		if p, err := s.loadProject(ctx, r.PathValue("ref")); err == nil {
			setProject(p)
		}
	case strings.Contains(tmpl, "{branch_id_or_ref}"):
		if p, err := s.branchParent(ctx, r.PathValue("branch_id_or_ref")); err == nil {
			setProject(p)
			// The event names the branch when the path does by its ref; the parent otherwise.
			if v := r.PathValue("branch_id_or_ref"); len(v) == 20 {
				e.ProjectRef = v
			}
		}
	case strings.Contains(tmpl, "organizations/{slug}"):
		if o, err := s.orgBySlug(ctx, r.PathValue("slug")); err == nil {
			setOrg(o)
		}
	case tmpl == "/platform/projects" || tmpl == "/v1/projects":
		// A project is created in the organization its body names.
		if o := s.bodyOrganization(ctx, r); o != nil {
			setOrg(o)
		}
	}
}

// bodyOrganization reads the organization a create-project body names, and puts the body back.
func (s *Server) bodyOrganization(ctx context.Context, r *http.Request) *registry.Organization {
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), r.Body))
	if err != nil || len(b) > maxBody {
		return nil
	}
	var in struct {
		Slug string `json:"organization_slug"`
		ID   any    `json:"organization_id"`
	}
	if json.Unmarshal(b, &in) != nil {
		return nil
	}
	slug := in.Slug
	if slug == "" {
		switch v := in.ID.(type) {
		case string:
			slug = v
		case float64:
			slug = itoa(int64(v))
		}
	}
	if slug == "" {
		return nil
	}
	o, err := s.orgBySlug(ctx, slug)
	if err != nil {
		return nil
	}
	return o
}

// auditRecord stores the event and, about once an hour, deletes the ones past the retention.
func (s *Server) auditRecord(ctx context.Context, e AuditEvent) {
	if err := s.audit.Record(ctx, e); err != nil {
		s.log.Warn("an audit event was not recorded", "route", e.Method+" "+e.Route, "err", err)
		return
	}
	now := s.now()
	last := s.auditPruned.Load()
	if now.UnixNano()-last < int64(time.Hour) || !s.auditPruned.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	n, err := s.audit.Prune(ctx, now.Add(-s.cfg.API.AuditRetention()))
	if err != nil {
		s.log.Warn("the audit log was not pruned", "err", err)
	} else if n > 0 {
		s.log.Info("old audit events deleted", "count", n)
	}
}

// ---- action names ---------------------------------------------------------------------------------

var (
	summariesOnce sync.Once
	summaries     map[string]string
)

// auditName is the name of an action: the summary the platform spec gives the operation ("Pauses the
// project"), or the route when the spec has none. Studio shows it in the Action column.
func (s *Server) auditName(key string) string {
	summariesOnce.Do(func() {
		summaries = map[string]string{}
		for _, file := range []string{"v1", "v2", "platform"} {
			b, err := specFS.ReadFile("gen/specs/" + file + ".json")
			if err != nil {
				continue
			}
			var doc struct {
				Paths map[string]map[string]struct {
					Summary string `json:"summary"`
				} `json:"paths"`
			}
			if json.Unmarshal(b, &doc) != nil {
				continue
			}
			for path, ops := range doc.Paths {
				for method, op := range ops {
					if op.Summary != "" {
						summaries[strings.ToUpper(method)+" "+path] = op.Summary
					}
				}
			}
		}
	})
	if n := summaries[key]; n != "" {
		return n
	}
	return key
}

// ---- the two endpoints ----------------------------------------------------------------------------

func (s *Server) routesAudit(add func(string, handlerFunc)) {
	add("GET /platform/organizations/{slug}/audit", s.orgAuditLogs)
	add("GET /platform/profile/audit", s.profileAuditLogs)
}

// auditRange reads and checks iso_timestamp_start and iso_timestamp_end.
func auditRange(r *http.Request) (from, to time.Time, err error) {
	q := r.URL.Query()
	parse := func(name string) (time.Time, error) {
		v := q.Get(name)
		if v == "" {
			return time.Time{}, errf(http.StatusBadRequest, "%s is required", name)
		}
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return time.Time{}, errf(http.StatusBadRequest, "%s must be an ISO 8601 timestamp", name)
		}
		return t.UTC(), nil
	}
	if from, err = parse("iso_timestamp_start"); err != nil {
		return
	}
	if to, err = parse("iso_timestamp_end"); err != nil {
		return
	}
	if to.Before(from) {
		return from, to, errf(http.StatusBadRequest, "iso_timestamp_end is before iso_timestamp_start")
	}
	if to.Sub(from) > auditMaxRange {
		return from, to, errf(http.StatusBadRequest, "Time range exceeded: a listing covers at most 62 days")
	}
	return from, to, nil
}

func (s *Server) orgAuditLogs(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	from, to, err := auditRange(r)
	if err != nil {
		return err
	}
	events, err := s.audit.List(r.Context(), AuditQuery{OrgID: org.ID, From: from, To: to, Limit: auditLimit})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.auditResponse(events))
	return nil
}

// profileAuditLogs lists the caller's own events, in every organization.
func (s *Server) profileAuditLogs(w http.ResponseWriter, r *http.Request) error {
	p := principalFrom(r.Context())
	if p == nil {
		return errUnauthorized
	}
	from, to, err := auditRange(r)
	if err != nil {
		return err
	}
	events, err := s.audit.List(r.Context(), AuditQuery{UserID: p.UserID, From: from, To: to, Limit: auditLimit})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.auditResponse(events))
	return nil
}

// auditResponse is AuditLogsResponse and UserAuditLogsResponse of the platform spec. The timestamp is in
// microseconds (Studio divides it by 1000 for dayjs). Studio keys its table rows by request_id, so an id that
// repeats within the listing (a client that sends the same X-Request-Id for several calls) gets a suffix.
func (s *Server) auditResponse(events []AuditEvent) map[string]any {
	seen := map[string]int{}
	rows := make([]map[string]any, 0, len(events))
	for _, e := range events {
		id := e.RequestID
		if n := seen[id]; n > 0 {
			id += "-" + itoa(int64(n+1))
		}
		seen[e.RequestID]++
		actor := map[string]any{"token_type": e.TokenType, "ip": nilIfEmpty(e.IP), "user_id": nilIfEmpty(e.UserID), "email": nilIfEmpty(e.Email)}
		if e.OAuthAppID != "" {
			actor["oauth_app_id"], actor["oauth_app_name"] = e.OAuthAppID, nilIfEmpty(e.OAuthAppName)
		}
		var params any
		if len(e.Params) > 0 {
			params = e.Params
		}
		rows = append(rows, map[string]any{
			"timestamp": e.At.UnixMicro(), "request_id": id,
			"organization_slug": nilIfEmpty(e.OrgSlug), "project_ref": nilIfEmpty(e.ProjectRef),
			"action": map[string]any{"name": e.Name, "method": e.Method, "route": e.Route, "status": e.Status, "metadata": nil, "params": params},
			"actor":  actor,
		})
	}
	return map[string]any{"result": rows, "retention_period": int(s.cfg.API.AuditRetention() / (24 * time.Hour))}
}

// ---- stores ---------------------------------------------------------------------------------------

// PGAuditStore keeps the audit log in the registry's database (supavise.audit_events, migration 1531).
type PGAuditStore struct{ pool *pgxpool.Pool }

// NewPGAuditStore returns the store over pool.
func NewPGAuditStore(pool *pgxpool.Pool) *PGAuditStore { return &PGAuditStore{pool: pool} }

func (s *PGAuditStore) Record(ctx context.Context, e AuditEvent) error {
	var params any
	if len(e.Params) > 0 {
		b, _ := json.Marshal(e.Params)
		params = b
	}
	_, err := s.pool.Exec(ctx, `insert into supavise.audit_events
		(at, request_id, org_id, org_slug, project_ref, user_id, email, token_type, oauth_app_id, oauth_app_name, ip, method, route, name, status, params)
		values ($1, $2, nullif($3::bigint, 0), nullif($4, ''), nullif($5, ''), nullif($6, ''), nullif($7, ''), $8, nullif($9, ''), nullif($10, ''), nullif($11, ''), $12, $13, $14, $15, $16::jsonb)`,
		e.At, e.RequestID, e.OrgID, e.OrgSlug, e.ProjectRef, e.UserID, e.Email, e.TokenType, e.OAuthAppID, e.OAuthAppName, e.IP, e.Method, e.Route, e.Name, e.Status, params)
	return err
}

func (s *PGAuditStore) List(ctx context.Context, q AuditQuery) ([]AuditEvent, error) {
	rows, err := s.pool.Query(ctx, `select id, at, request_id, coalesce(org_id, 0), coalesce(org_slug, ''), coalesce(project_ref, ''), coalesce(user_id, ''),
			coalesce(email, ''), token_type, coalesce(oauth_app_id, ''), coalesce(oauth_app_name, ''), coalesce(ip, ''), method, route, name, status, coalesce(params, '{}'::jsonb)
		from supavise.audit_events
		where at >= $1 and at <= $2 and (($3::bigint <> 0 and org_id = $3) or ($4 <> '' and user_id = $4))
		order by at desc, id desc limit $5`, q.From, q.To, q.OrgID, q.UserID, q.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		var params []byte
		if err := rows.Scan(&e.ID, &e.At, &e.RequestID, &e.OrgID, &e.OrgSlug, &e.ProjectRef, &e.UserID, &e.Email, &e.TokenType,
			&e.OAuthAppID, &e.OAuthAppName, &e.IP, &e.Method, &e.Route, &e.Name, &e.Status, &params); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(params, &e.Params)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PGAuditStore) Prune(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `delete from supavise.audit_events where at < $1`, before)
	return tag.RowsAffected(), err
}

// MemoryAuditStore is the in-memory AuditStore for tests and a node without a Postgres registry.
type MemoryAuditStore struct {
	mu     sync.Mutex
	next   int64
	events []AuditEvent
}

// NewMemoryAuditStore returns an empty store.
func NewMemoryAuditStore() *MemoryAuditStore { return &MemoryAuditStore{} }

func (m *MemoryAuditStore) Record(_ context.Context, e AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	e.ID = m.next
	m.events = append(m.events, e)
	return nil
}

func (m *MemoryAuditStore) List(_ context.Context, q AuditQuery) ([]AuditEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []AuditEvent
	for _, e := range m.events {
		if e.At.Before(q.From) || e.At.After(q.To) {
			continue
		}
		if (q.OrgID != 0 && e.OrgID == q.OrgID) || (q.UserID != "" && e.UserID == q.UserID) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].ID > out[j].ID
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (m *MemoryAuditStore) Prune(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.events[:0]
	var n int64
	for _, e := range m.events {
		if e.At.Before(before) {
			n++
			continue
		}
		kept = append(kept, e)
	}
	m.events = kept
	return n, nil
}
