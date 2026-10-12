package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/registry"
)

// auditLogs reads the organization's audit log for the last day and returns its rows.
func (f *fixture) auditRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	now := time.Now().UTC()
	q := url.Values{"iso_timestamp_start": {now.Add(-24 * time.Hour).Format(time.RFC3339)}, "iso_timestamp_end": {now.Add(time.Minute).Format(time.RFC3339)}}
	rec := f.do("GET", path+"?"+q.Encode(), nil)
	if rec.Code != 200 {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Supavise-Stub") != "" {
		t.Error("served by the stub")
	}
	body := bodyMap(t, rec)
	if body["retention_period"] != float64(90) {
		t.Errorf("retention_period %v, want 90", body["retention_period"])
	}
	key := "GET /platform/profile/audit"
	if strings.Contains(path, "/organizations/") {
		key = "GET /platform/organizations/{slug}/audit"
	}
	validateAgainstSpec(t, key, rec.Body.Bytes())
	var rows []map[string]any
	for _, r := range body["result"].([]any) {
		rows = append(rows, r.(map[string]any))
	}
	return rows
}

const (
	orgAudit     = "/platform/organizations/default/audit"
	profileAudit = "/platform/profile/audit"
)

func rowFor(rows []map[string]any, method, route string) map[string]any {
	for _, r := range rows {
		a := r["action"].(map[string]any)
		if a["method"] == method && a["route"] == route {
			return r
		}
	}
	return nil
}

// A request that changed something is in the organization's log with who, from where, which route and how it
// ended; a read is not; and nothing of the request body is kept. A stub answered an empty list, and Studio said
// "Your organization does not have any audit logs available yet" while projects were being changed.
func TestAuditLogRecordsWhatChangedAndNotWhatWasRead(t *testing.T) {
	f := newFixture(t)
	f.do("GET", "/platform/projects/"+testRef+"/config/postgrest", nil) // a read
	rec := f.do("PATCH", "/platform/projects/"+testRef, map[string]any{"name": "Renamed by the audit test"}, "X-Request-Id", "req-rename-1")
	if rec.Code != 200 {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body)
	}

	rows := f.auditRows(t, orgAudit)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want only the PATCH: %v", len(rows), rows)
	}
	r := rows[0]
	act := r["action"].(map[string]any)
	actor := r["actor"].(map[string]any)
	if act["method"] != "PATCH" || act["route"] != "/platform/projects/{ref}" || act["status"] != float64(200) || act["name"] == "" || act["name"] == "PATCH /platform/projects/{ref}" {
		t.Errorf("action %v: want the PATCH of the template with its status and the spec's summary as its name", act)
	}
	if params, _ := act["params"].(map[string]any); params["ref"] != testRef {
		t.Errorf("params %v", act["params"])
	}
	if r["project_ref"] != testRef || r["organization_slug"] != "default" || r["request_id"] != "req-rename-1" {
		t.Errorf("target: project %v org %v request %v", r["project_ref"], r["organization_slug"], r["request_id"])
	}
	if actor["token_type"] != "jwt" || actor["user_id"] != f.userID || actor["email"] != "dev@example.test" || actor["ip"] == nil || actor["ip"] == "" {
		t.Errorf("actor %v", actor)
	}
	// Microseconds, as Studio divides by 1000 for the date.
	if ts := r["timestamp"].(float64); ts < float64(time.Now().Add(-time.Minute).UnixMicro()) || ts > float64(time.Now().Add(time.Minute).UnixMicro()) {
		t.Errorf("timestamp %v is not in microseconds", ts)
	}
	b, _ := json.Marshal(rows)
	if strings.Contains(string(b), "Renamed by the audit test") {
		t.Error("the request body is in the log")
	}

	// The caller's own page lists the same event.
	own := f.auditRows(t, profileAudit)
	if rowFor(own, "PATCH", "/platform/projects/{ref}") == nil {
		t.Errorf("the account page lacks the event: %v", own)
	}
}

// The data plane and the POSTs that only read stay out, so the table is not flooded.
func TestAuditLogSkipsTheDataPlane(t *testing.T) {
	f := newFixture(t)
	f.do("POST", "/platform/pg-meta/"+testRef+"/query", map[string]any{"query": "select 1"})
	f.do("POST", "/platform/telemetry/event", map[string]any{})
	f.do("POST", "/platform/projects/"+testRef+"/api-keys/temporary", map[string]any{})
	f.do("PUT", "/platform/projects/"+testRef+"/content", map[string]any{"id": "x"})
	if rows := f.auditRows(t, profileAudit); len(rows) != 0 {
		t.Errorf("recorded the data plane: %v", rows)
	}
	for _, key := range []string{
		"POST /platform/pg-meta/{ref}/tables", "POST /v1/projects/{ref}/database/query", "PUT /platform/projects/{ref}/content/{id}",
		"POST /v1/snippets", "POST /platform/projects/{ref}/analytics/endpoints/logs.all", "POST /platform/storage/{ref}/buckets/{id}/objects/list",
		"POST /platform/projects/{ref}/api/graphql", "POST /platform/auth/{ref}/validate/spam", "POST /platform/telemetry/event",
	} {
		if !auditSkip.MatchString(key) {
			t.Errorf("%s is recorded", key)
		}
	}
	for _, key := range []string{
		"POST /platform/projects", "PATCH /platform/projects/{ref}", "DELETE /v1/projects/{ref}", "POST /platform/projects/{ref}/pause",
		"POST /platform/storage/{ref}/buckets", "PATCH /platform/auth/{ref}/config", "POST /v1/projects/{ref}/branches", "DELETE /v1/branches/{branch_id_or_ref}",
		"POST /platform/organizations/{slug}/members/invitations", "POST /v1/projects/{ref}/api-keys",
	} {
		if auditSkip.MatchString(key) {
			t.Errorf("%s is not recorded", key)
		}
	}
}

// A request the role check refuses is recorded too, with the 403: who tried is part of the trail.
func TestAuditLogRecordsARefusedAttempt(t *testing.T) {
	f := newFixture(t)
	ro := "22222222-3333-4444-8555-666666666666"
	f.addMember(ro, members.RoleReadOnly)
	tok := f.signJWT(map[string]any{"sub": ro, "email": "ro@example.test", "role": "authenticated"})
	if rec := f.doAs(tok, "PATCH", "/platform/projects/"+testRef, map[string]any{"name": "nope"}); rec.Code != http.StatusForbidden {
		t.Fatalf("read-only rename: %d", rec.Code)
	}
	rows := f.auditRows(t, orgAudit)
	r := rowFor(rows, "PATCH", "/platform/projects/{ref}")
	if r == nil {
		t.Fatalf("the refused attempt is not in the log: %v", rows)
	}
	if r["action"].(map[string]any)["status"] != float64(403) || r["actor"].(map[string]any)["user_id"] != ro {
		t.Errorf("row %v", r)
	}
}

// One organization's log never lists another's, and an account lists only its own events.
func TestAuditLogIsScopedToTheOrganizationAndTheUser(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	other, err := f.reg.CreateOrganization(ctx, "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	otherRef := "cccccccccccccccccccc"
	f.mgr.addProject(t, otherRef, "Elsewhere", other.ID, registry.StatusActiveHealthy)
	if err := f.srv.members.EnsureOwner(ctx, members.OrgRef{ID: other.ID, Slug: other.Slug}, f.userID); err != nil {
		t.Fatal(err)
	}
	f.do("PATCH", "/platform/projects/"+testRef, map[string]any{"name": "first"})
	f.do("PATCH", "/platform/projects/"+otherRef, map[string]any{"name": "second"})

	mine := f.auditRows(t, orgAudit)
	theirs := f.auditRows(t, "/platform/organizations/other/audit")
	if len(mine) != 1 || mine[0]["project_ref"] != testRef {
		t.Errorf("the default organization's log: %v", mine)
	}
	if len(theirs) != 1 || theirs[0]["project_ref"] != otherRef || theirs[0]["organization_slug"] != "other" {
		t.Errorf("the other organization's log: %v", theirs)
	}
	if own := f.auditRows(t, profileAudit); len(own) != 2 {
		t.Errorf("the account lists %d events, want both of its own", len(own))
	}

	// Somebody else's account: another user lists nothing of this one's.
	stranger := "55555555-6666-4777-8888-999999999999"
	tok := f.signJWT(map[string]any{"sub": stranger, "email": "s@example.test", "role": "authenticated"})
	now := time.Now().UTC()
	q := url.Values{"iso_timestamp_start": {now.Add(-time.Hour).Format(time.RFC3339)}, "iso_timestamp_end": {now.Add(time.Minute).Format(time.RFC3339)}}
	rec := f.doAs(tok, "GET", profileAudit+"?"+q.Encode(), nil)
	if rec.Code != 200 || len(bodyMap(t, rec)["result"].([]any)) != 0 {
		t.Errorf("another account's page: %d %s", rec.Code, rec.Body)
	}
	// And a member of no organization is refused the organization's page.
	if rec := f.doAs(tok, "GET", orgAudit+"?"+q.Encode(), nil); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("a stranger reads the organization's log: %d", rec.Code)
	}
}

type rangeResp struct {
	code int
	body string
}

func TestAuditRangeIsChecked(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()
	get := func(from, to string) *rangeResp {
		q := url.Values{}
		if from != "" {
			q.Set("iso_timestamp_start", from)
		}
		if to != "" {
			q.Set("iso_timestamp_end", to)
		}
		rec := f.do("GET", orgAudit+"?"+q.Encode(), nil)
		return &rangeResp{rec.Code, rec.Body.String()}
	}
	fmtT := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	for _, tc := range []struct {
		name     string
		from, to string
		want     int
		msg      string
	}{
		{"missing start", "", fmtT(0), 400, "iso_timestamp_start is required"},
		{"missing end", fmtT(-time.Hour), "", 400, "iso_timestamp_end is required"},
		{"not a timestamp", "yesterday", fmtT(0), 400, "ISO 8601"},
		{"end before start", fmtT(0), fmtT(-time.Hour), 400, "before"},
		{"62 days and a few minutes", fmtT(-62*24*time.Hour - 4*time.Minute), fmtT(0), 200, ""},
		{"63 days", fmtT(-63 * 24 * time.Hour), fmtT(0), 400, "range exceeded"}, // Studio tests the message for this
	} {
		resp := get(tc.from, tc.to)
		if resp.code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, resp.code, tc.want)
			continue
		}
		if tc.msg != "" && !strings.Contains(resp.body, tc.msg) {
			t.Errorf("%s: body %q lacks %q", tc.name, resp.body, tc.msg)
		}
	}
}

// Rows past the retention are deleted by the process, about once an hour, as the next event is recorded.
func TestAuditLogIsBounded(t *testing.T) {
	f := newFixture(t)
	t0 := time.Now().UTC()
	clock := t0
	f.srv.now = func() time.Time { return clock }
	f.do("PATCH", "/platform/projects/"+testRef, map[string]any{"name": "old"}, "X-Request-Id", "old")
	clock = t0.Add(91 * 24 * time.Hour)
	f.do("PATCH", "/platform/projects/"+testRef, map[string]any{"name": "new"}, "X-Request-Id", "new")
	events, err := f.srv.audit.List(context.Background(), AuditQuery{OrgID: f.org.ID, From: t0.Add(-time.Hour), To: clock.Add(time.Hour), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].RequestID != "new" {
		t.Errorf("events after the retention: %+v", events)
	}
	// [api] audit_retention_days
	f.cfg.API.AuditRetentionDays = 7
	if got := f.cfg.API.AuditRetention(); got != 7*24*time.Hour {
		t.Errorf("retention %v", got)
	}
}

// How a request was authenticated is in the row, and a credential in the path is not.
func TestAuditLogTokenTypesAndSecretParams(t *testing.T) {
	f := newFixture(t)
	rec := f.do("POST", "/platform/profile/access-tokens", map[string]any{"name": "ci"})
	pat := bodyMap(t, rec)["token"].(string)
	if rec := f.doAs(pat, "PATCH", "/v1/projects/"+testRef+"/config/auth", map[string]any{"site_url": "https://app.example.com"}); rec.Code != 200 {
		t.Fatalf("PATCH with a PAT: %d %s", rec.Code, rec.Body)
	}
	rows := f.auditRows(t, orgAudit)
	r := rowFor(rows, "PATCH", "/v1/projects/{ref}/config/auth")
	if r == nil || r["actor"].(map[string]any)["token_type"] != "v1" {
		t.Errorf("a personal access token's request: %v", r)
	}
	for _, name := range []string{"token", "invite_token", "client_secret", "password"} {
		if !auditSecretParam(name) {
			t.Errorf("%s is stored", name)
		}
	}
	for _, name := range []string{"ref", "slug", "id", "branch_id_or_ref"} {
		if auditSecretParam(name) {
			t.Errorf("%s is not stored", name)
		}
	}
}

// Studio keys its table rows by request id, so a client that repeats one gets distinct ids in the listing.
func TestAuditLogRequestIDsAreUnique(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 3; i++ {
		f.do("PATCH", "/platform/projects/"+testRef, map[string]any{"name": "n"}, "X-Request-Id", "same")
	}
	seen := map[string]bool{}
	for _, r := range f.auditRows(t, orgAudit) {
		id := r["request_id"].(string)
		if seen[id] {
			t.Errorf("request_id %q repeats", id)
		}
		seen[id] = true
	}
	if len(seen) != 3 {
		t.Errorf("%d rows, want 3", len(seen))
	}
}

// A project created through the API is in the log of the organization its body names.
func TestAuditLogOfAProjectCreate(t *testing.T) {
	f := newFixture(t)
	rec := f.do("POST", "/platform/projects", map[string]any{"name": "audited", "organization_slug": "default"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	rows := f.auditRows(t, orgAudit)
	r := rowFor(rows, "POST", "/platform/projects")
	if r == nil || r["organization_slug"] != "default" {
		t.Errorf("the create is not in the organization's log: %v", rows)
	}
}

// strangerToken signs in a user who belongs to no organization, or only to the one given.
func (f *fixture) strangerToken(t *testing.T, id, email string, ownerOf *registry.Organization) string {
	t.Helper()
	if ownerOf != nil {
		if err := f.srv.members.EnsureOwner(context.Background(), members.OrgRef{ID: ownerOf.ID, Slug: ownerOf.Slug}, id); err != nil {
			t.Fatal(err)
		}
	}
	return f.signJWT(map[string]any{"sub": id, "email": email, "role": "authenticated"})
}

// The audit runs before the role check, and the slug or the project ref in the path is whatever the caller
// typed. A user who is not a member of an organization must not write into its log: the attempt is in the
// caller's own page, without the organization.
func TestAuditLogIgnoresTheOrganizationOfAStranger(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	other, err := f.reg.CreateOrganization(ctx, "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	stranger := "55555555-6666-4777-8888-999999999999"
	// A member of another organization only, and a member of none.
	for name, tok := range map[string]string{
		"a member of another organization": f.strangerToken(t, stranger, "s@example.test", other),
		"a member of none":                 f.strangerToken(t, "66666666-7777-4888-8999-000000000000", "n@example.test", nil),
	} {
		for i := 0; i < 3; i++ {
			if rec := f.doAs(tok, "PATCH", "/platform/projects/"+testRef, map[string]any{"name": "x"}); rec.Code != http.StatusForbidden {
				t.Fatalf("%s: rename: %d %s", name, rec.Code, rec.Body)
			}
			if rec := f.doAs(tok, "DELETE", "/platform/organizations/default", nil); rec.Code != http.StatusForbidden {
				t.Fatalf("%s: delete the organization: %d %s", name, rec.Code, rec.Body)
			}
		}
		if rec := f.doAs(tok, "POST", "/platform/projects", map[string]any{"name": "mine", "organization_slug": "default"}); rec.Code != http.StatusForbidden {
			t.Fatalf("%s: create in the organization: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rows := f.auditRows(t, orgAudit); len(rows) != 0 {
		t.Fatalf("the organization's log lists the attempts of strangers: %v", rows)
	}
	// They are in the caller's own page, without the organization or the project the lookup found.
	now := time.Now().UTC()
	q := url.Values{"iso_timestamp_start": {now.Add(-time.Hour).Format(time.RFC3339)}, "iso_timestamp_end": {now.Add(time.Minute).Format(time.RFC3339)}}
	rec := f.doAs(f.strangerToken(t, stranger, "s@example.test", nil), "GET", profileAudit+"?"+q.Encode(), nil)
	if rec.Code != 200 {
		t.Fatalf("the stranger's page: %d %s", rec.Code, rec.Body)
	}
	own := bodyMap(t, rec)["result"].([]any)
	if len(own) != 7 {
		t.Fatalf("the stranger's own page lists %d events, want the 7 attempts", len(own))
	}
	for _, r := range own {
		row := r.(map[string]any)
		if row["organization_slug"] != nil {
			t.Errorf("the attempt names the organization: %v", row)
		}
		act := row["action"].(map[string]any)
		if act["status"] != float64(403) {
			t.Errorf("status %v", act["status"])
		}
	}
	// The rows are not under the organization's id in the store either.
	events, err := f.srv.audit.List(ctx, AuditQuery{OrgID: f.org.ID, From: now.Add(-time.Hour), To: now.Add(time.Minute), Limit: 100})
	if err != nil || len(events) != 0 {
		t.Errorf("events of the organization: %v %v", events, err)
	}
}

// A member's refused attempt stays in the organization's log, whatever the role, and so does a stranger's
// request naming a project that does not exist (nothing to attribute).
func TestAuditLogKeepsTheRefusedAttemptOfAMember(t *testing.T) {
	f := newFixture(t)
	dev := "77777777-8888-4999-8aaa-bbbbbbbbbbbb"
	f.addMember(dev, members.RoleReadOnly)
	tok := f.signJWT(map[string]any{"sub": dev, "email": "dev2@example.test", "role": "authenticated"})
	if rec := f.doAs(tok, "DELETE", "/platform/organizations/default", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("a read-only member deletes the organization: %d", rec.Code)
	}
	r := rowFor(f.auditRows(t, orgAudit), "DELETE", "/platform/organizations/{slug}")
	if r == nil || r["organization_slug"] != "default" || r["actor"].(map[string]any)["user_id"] != dev {
		t.Errorf("the member's refused attempt: %v", r)
	}
}

// The client chooses the request id, the path parameters and the address, so the log bounds them: a header of
// 200 kB was stored and returned whole.
func TestAuditLogBoundsWhatTheClientChooses(t *testing.T) {
	f := newFixture(t)
	huge := strings.Repeat("a", 200_000)
	rec := f.do("PATCH", "/platform/projects/"+testRef, map[string]any{"name": "n"}, "X-Request-Id", huge)
	if got := rec.Header().Get("X-Request-Id"); got == huge || got == "" || len(got) > maxRequestID {
		t.Errorf("the response echoes a request id of %d bytes", len(got))
	}
	for _, id := range []string{"has space", "semi;colon", "ünï", strings.Repeat("b", maxRequestID+1)} {
		rec := f.do("PATCH", "/platform/projects/"+testRef, map[string]any{"name": "n"}, "X-Request-Id", id)
		if got := rec.Header().Get("X-Request-Id"); got == id || got == "" {
			t.Errorf("request id %q is kept: %q", id, got)
		}
	}
	ok := strings.Repeat("c", maxRequestID)
	if rec := f.do("PATCH", "/platform/projects/"+testRef, map[string]any{"name": "n"}, "X-Request-Id", ok); rec.Header().Get("X-Request-Id") != ok {
		t.Errorf("a valid id of %d bytes is replaced: %q", maxRequestID, rec.Header().Get("X-Request-Id"))
	}
	// A path parameter as long as the request line allows.
	longRef := strings.Repeat("r", 5000)
	if rec := f.do("PATCH", "/platform/projects/"+longRef, map[string]any{"name": "n"}); rec.Code != http.StatusNotFound {
		t.Fatalf("a project that does not exist: %d", rec.Code)
	}
	for _, r := range f.auditRows(t, profileAudit) {
		if id := r["request_id"].(string); len(id) > maxRequestID+8 { // +8: the suffix of a repeated id
			t.Errorf("a stored request id of %d bytes", len(id))
		}
		if params, _ := r["action"].(map[string]any)["params"].(map[string]any); params != nil {
			for k, v := range params {
				if len(v.(string)) > auditValueMax {
					t.Errorf("param %s of %d bytes", k, len(v.(string)))
				}
			}
		}
	}
}

func TestAuditClip(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		max      int
		want     string
	}{
		{"short", "abc", 10, "abc"},
		{"cut", "abcdef", 3, "abc"},
		{"NUL removed", "a\x00b", 10, "ab"},
		{"invalid UTF-8 replaced", "a\xffb", 10, "a\uFFFDb"},
		{"cut at a character", "aé", 2, "a"}, // é is two bytes: one byte would be half of it
		{"empty", "", 5, ""},
	} {
		if got := auditClip(tc.in, tc.max); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// One user adds at most auditRefusedPerMinute turned-away requests a minute, so a script that probes cannot
// push the real events out of a listing; a successful change is never dropped.
func TestAuditLogCapsRefusedAttempts(t *testing.T) {
	var th auditThrottle
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= auditRefusedPerMinute+5; i++ {
		if n := th.count("u", t0.Add(time.Duration(i)*time.Millisecond)); n != i {
			t.Fatalf("count %d, want %d", n, i)
		}
	}
	if n := th.count("other", t0); n != 1 {
		t.Errorf("another user's count %d", n)
	}
	if n := th.count("u", t0.Add(time.Minute+time.Second)); n != 1 {
		t.Errorf("the count after a minute %d, want 1", n)
	}

	f := newFixture(t)
	clock := time.Now().UTC()
	f.srv.now = func() time.Time { return clock }
	tok := f.strangerToken(t, "88888888-9999-4aaa-8bbb-cccccccccccc", "p@example.test", nil)
	for i := 0; i < auditRefusedPerMinute+10; i++ {
		f.doAs(tok, "PATCH", "/platform/projects/"+testRef, map[string]any{"name": "x"})
	}
	list := func() int {
		events, err := f.srv.audit.List(context.Background(), AuditQuery{UserID: "88888888-9999-4aaa-8bbb-cccccccccccc", From: clock.Add(-time.Hour), To: clock.Add(time.Hour), Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		return len(events)
	}
	if n := list(); n != auditRefusedPerMinute {
		t.Errorf("%d refused events stored, want %d", n, auditRefusedPerMinute)
	}
	// The owner's changes are not refused attempts: always stored.
	for i := 0; i < auditRefusedPerMinute+10; i++ {
		f.do("PATCH", "/platform/projects/"+testRef, map[string]any{"name": "ok"})
	}
	events, _ := f.srv.audit.List(context.Background(), AuditQuery{UserID: f.userID, From: clock.Add(-time.Hour), To: clock.Add(time.Hour), Limit: 1000})
	if len(events) != auditRefusedPerMinute+10 {
		t.Errorf("%d changes stored, want all %d", len(events), auditRefusedPerMinute+10)
	}
	clock = clock.Add(2 * time.Minute)
	f.doAs(tok, "PATCH", "/platform/projects/"+testRef, map[string]any{"name": "x"})
	if n := list(); n != auditRefusedPerMinute+1 {
		t.Errorf("%d refused events after the minute, want %d", n, auditRefusedPerMinute+1)
	}
}

// testAuditStore is the contract of an AuditStore, run on the memory store and, when CI provides a
// database, on the one over supavise.audit_events.
func testAuditStore(t *testing.T, st AuditStore, orgA, orgB int64) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Millisecond)
	ev := func(at time.Time, org int64, user, req string) AuditEvent {
		return AuditEvent{At: at, RequestID: req, OrgID: org, OrgSlug: "slug", ProjectRef: "abcdefghijklmnopqrst", UserID: user, Email: user + "@example.test",
			TokenType: "jwt", IP: "203.0.113.7", Method: "PATCH", Route: "/platform/projects/{ref}", Name: "Updates the project", Status: 200,
			Params: map[string]string{"ref": "abcdefghijklmnopqrst"}}
	}
	for _, e := range []AuditEvent{
		ev(base.Add(-3*time.Hour), orgA, "u1", "r1"), ev(base.Add(-2*time.Hour), orgA, "u2", "r2"),
		ev(base.Add(-time.Hour), orgB, "u1", "r3"), ev(base.Add(-10*24*time.Hour), orgA, "u1", "r-old"),
		{At: base, RequestID: "r-noorg", UserID: "u1", TokenType: "v1", Method: "PATCH", Route: "/platform/profile", Name: "n", Status: 200},
	} {
		if err := st.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(q AuditQuery) string {
		got, err := st.List(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range got {
			out = append(out, e.RequestID)
		}
		return strings.Join(out, ",")
	}
	from, to := base.Add(-24*time.Hour), base.Add(time.Minute)
	if got := ids(AuditQuery{OrgID: orgA, From: from, To: to, Limit: 10}); got != "r2,r1" {
		t.Errorf("organization A: %q, want its two events of the last day, newest first", got)
	}
	if got := ids(AuditQuery{OrgID: orgB, From: from, To: to, Limit: 10}); got != "r3" {
		t.Errorf("organization B: %q", got)
	}
	if got := ids(AuditQuery{UserID: "u1", From: from, To: to, Limit: 10}); got != "r-noorg,r3,r1" {
		t.Errorf("user u1: %q, want every organization's events and the one without", got)
	}
	if got := ids(AuditQuery{OrgID: orgA, From: from, To: to, Limit: 1}); got != "r2" {
		t.Errorf("limit 1: %q", got)
	}
	if got := ids(AuditQuery{From: from, To: to, Limit: 10}); got != "" {
		t.Errorf("a query with neither organization nor user lists %q", got)
	}
	got, _ := st.List(ctx, AuditQuery{OrgID: orgA, From: from, To: to, Limit: 10})
	if len(got) != 2 || got[0].Params["ref"] != "abcdefghijklmnopqrst" || got[0].IP != "203.0.113.7" || got[0].Status != 200 || got[0].Email != "u2@example.test" || !got[0].At.Equal(base.Add(-2*time.Hour)) {
		t.Errorf("round trip: %+v", got)
	}
	n, err := st.Prune(ctx, base.Add(-5*24*time.Hour))
	if err != nil || n != 1 {
		t.Errorf("Prune deleted %d (%v), want the one event older than five days", n, err)
	}
	if got := ids(AuditQuery{OrgID: orgA, From: base.Add(-30 * 24 * time.Hour), To: to, Limit: 10}); got != "r2,r1" {
		t.Errorf("after Prune: %q", got)
	}
}

func TestMemoryAuditStore(t *testing.T) { testAuditStore(t, NewMemoryAuditStore(), 11, 12) }

func TestPGAuditStore(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	r, err := registry.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := registry.Migrate(ctx, r.Pool()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Pool().Exec(ctx, `truncate supavise.audit_events`); err != nil {
		t.Fatal(err)
	}
	testAuditStore(t, NewPGAuditStore(r.Pool()), 11, 12)
}
