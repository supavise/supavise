package branching

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

type hook struct {
	header http.Header
	body   []byte
}

// receiver is a notify_url target that keeps what it was sent.
type receiver struct {
	mu   sync.Mutex
	got  []hook
	srv  *httptest.Server
	code int
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{code: 200}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, hook{header: req.Header.Clone(), body: b})
		r.mu.Unlock()
		w.WriteHeader(r.code)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) hooks() []hook {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hook(nil), r.got...)
}

// verifyStandardWebhook is the receiver side of https://www.standardwebhooks.com, written from the
// specification and sharing no code with the sender: the signed content is "{id}.{timestamp}.{body}",
// the key is the base64 decoding of the secret after "whsec_", and the header holds space-separated
// "v1,{base64 signature}" entries.
func verifyStandardWebhook(secret string, h http.Header, body []byte, now time.Time) bool {
	id, ts, sigs := h.Get("webhook-id"), h.Get("webhook-timestamp"), h.Get("webhook-signature")
	if id == "" || ts == "" || sigs == "" || !strings.HasPrefix(secret, "whsec_") {
		return false
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || now.Sub(time.Unix(sec, 0)).Abs() > 5*time.Minute {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(secret[len("whsec_"):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "." + string(body)))
	want := mac.Sum(nil)
	for _, entry := range strings.Fields(sigs) {
		ver, b64, ok := strings.Cut(entry, ",")
		if !ok || ver != "v1" {
			continue
		}
		if got, err := base64.StdEncoding.DecodeString(b64); err == nil && hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

// withSystem adds the system project, which holds the node's notify signing key.
func withSystem(t *testing.T, h *harness) {
	t.Helper()
	if err := h.reg.CreateProject(context.Background(), &registry.Project{Ref: config.SystemRef, Name: "system", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyEnvelopeMatchesHosted(t *testing.T) {
	rcv := newReceiver(t)
	h := newHarness(t, func(c *config.Config) { c.Branching.AllowPrivateNotifyURLs = true })
	withSystem(t, h)
	b := h.create("n", func(in *CreateInput) { in.NotifyURL = rcv.srv.URL; in.Persistent = true })
	h.svc.Drain(context.Background()) // the notification is sent after the state shows
	hooks := rcv.hooks()
	if len(hooks) != 1 {
		t.Fatalf("%d notifications", len(hooks))
	}
	var env map[string]any
	if err := json.Unmarshal(hooks[0].body, &env); err != nil {
		t.Fatal(err)
	}
	if env["type"] != "run.completed" {
		t.Fatalf("type = %v", env["type"])
	}
	if _, err := time.Parse(time.RFC3339, env["timestamp"].(string)); err != nil {
		t.Fatalf("timestamp: %v", err)
	}
	data := env["data"].(map[string]any)
	if data["project_ref"] != b.Ref || data["details_url"] != "https://studio.supavise.test/project/"+parentRef+"/branches" {
		t.Fatalf("data = %v", data)
	}
	ar := data["action_run"].(map[string]any)
	run := latestRun(t, h, b.Ref)
	if ar["id"] != run.ID {
		t.Fatalf("action_run.id = %v, want %s", ar["id"], run.ID)
	}
	if _, err := time.Parse(time.RFC3339, ar["created_at"].(string)); err != nil {
		t.Fatalf("created_at: %v", err)
	}
	if _, has := ar["failure"]; has {
		t.Fatalf("a successful run carries a failure: %v", ar["failure"])
	}
	steps := ar["steps"].([]any)
	var names, statuses []string
	for _, s := range steps {
		m := s.(map[string]any)
		names = append(names, m["name"].(string))
		statuses = append(statuses, m["status"].(string))
		if _, err := time.Parse(time.RFC3339, m["updated_at"].(string)); err != nil {
			t.Fatalf("step updated_at: %v", err)
		}
		if len(m) != 3 {
			t.Fatalf("a step has keys %v, want name, status, updated_at", m)
		}
	}
	// The documented example: {"name": "clone", "status": "exited", "updated_at": ...}, statuses in lowercase.
	if strings.Join(names, ",") != "clone,pull,health,configure,migrate,seed,deploy" {
		t.Fatalf("steps = %v", names)
	}
	if strings.Join(statuses, ",") != "paused,exited,exited,paused,exited,paused,exited" {
		t.Fatalf("statuses = %v", statuses)
	}
	// What was sent before the envelope is kept under "supavise".
	sv := env["supavise"].(map[string]any)
	if sv["branch_name"] != "n" || sv["operation"] != "create" || sv["status"] != "FUNCTIONS_DEPLOYED" || sv["parent_project_ref"] != parentRef ||
		sv["project_ref"] != b.Ref || sv["branch_id"] != b.ID || sv["project_status"] != "ACTIVE_HEALTHY" || !strings.Contains(sv["message"].(string), "3 migration(s)") {
		t.Fatalf("supavise = %v", sv)
	}
	// Nothing else at the top level: a receiver written for hosted sees hosted's shape.
	if len(env) != 4 || len(data) != 3 || len(ar) != 3 {
		t.Fatalf("shape: %d top-level keys, %d data keys, %d action_run keys", len(env), len(data), len(ar))
	}
}

func TestNotifyIsSignedWithStandardWebhooks(t *testing.T) {
	rcv := newReceiver(t)
	h := newHarness(t, func(c *config.Config) { c.Branching.AllowPrivateNotifyURLs = true })
	withSystem(t, h)
	h.create("n", func(in *CreateInput) { in.NotifyURL = rcv.srv.URL })
	h.svc.Drain(context.Background())
	hooks := rcv.hooks()
	if len(hooks) != 1 {
		t.Fatalf("%d notifications", len(hooks))
	}
	secret, err := h.svc.NotifySecret(context.Background())
	if err != nil || !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("secret = %q %v", secret, err)
	}
	if raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_")); err != nil || len(raw) != 32 {
		t.Fatalf("secret is not 32 base64 bytes: %v", err)
	}
	if !verifyStandardWebhook(secret, hooks[0].header, hooks[0].body, time.Now()) {
		t.Fatalf("the signature does not verify: %v", hooks[0].header)
	}
	if id := hooks[0].header.Get("webhook-id"); !strings.HasPrefix(id, "msg_") {
		t.Fatalf("webhook-id = %q", id)
	}
	if hooks[0].header.Get("Content-Type") != "application/json" {
		t.Fatalf("content type = %q", hooks[0].header.Get("Content-Type"))
	}
	// The check itself rejects what it should.
	tampered := append([]byte(nil), hooks[0].body...)
	tampered[len(tampered)-2] ^= 1
	if verifyStandardWebhook(secret, hooks[0].header, tampered, time.Now()) {
		t.Fatal("a changed body verifies")
	}
	if verifyStandardWebhook(secret, hooks[0].header, hooks[0].body, time.Now().Add(time.Hour)) {
		t.Fatal("an old timestamp verifies")
	}
	other, _ := newNotifySecret()
	if verifyStandardWebhook(other, hooks[0].header, hooks[0].body, time.Now()) {
		t.Fatal("another secret verifies")
	}
	// The secret is stable until it is rotated, and then the old one stops verifying.
	if again, _ := h.svc.NotifySecret(context.Background()); again != secret {
		t.Fatal("the secret changed between calls")
	}
	rotated, err := h.svc.RotateNotifySecret(context.Background())
	if err != nil || rotated == secret {
		t.Fatalf("rotate: %q %v", rotated, err)
	}
	if now, _ := h.svc.NotifySecret(context.Background()); now != rotated {
		t.Fatal("the rotated secret was not kept")
	}
}

func TestNotifyCarriesTheFailure(t *testing.T) {
	rcv := newReceiver(t)
	h := newHarness(t, func(c *config.Config) { c.Branching.AllowPrivateNotifyURLs = true })
	withSystem(t, h)
	b := h.create("n", func(in *CreateInput) { in.NotifyURL = rcv.srv.URL })
	h.db.migs[b.Ref] = append(h.db.migs[b.Ref], mig("20260110000000", "feature", "create table f (id int)"))
	h.db.migs[parentRef] = append(h.db.migs[parentRef], mig("20260105000000", "hotfix", "select 1"))
	if _, err := h.svc.Merge(context.Background(), b.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	h.wait(b.Ref)
	h.svc.Drain(context.Background())
	hooks := rcv.hooks()
	if len(hooks) != 2 {
		t.Fatalf("%d notifications", len(hooks))
	}
	var env struct {
		Data struct {
			ActionRun struct {
				Failure string `json:"failure"`
				Steps   []struct{ Name, Status string }
			} `json:"action_run"`
		} `json:"data"`
		Supavise map[string]any `json:"supavise"`
	}
	if err := json.Unmarshal(hooks[1].body, &env); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env.Data.ActionRun.Failure, "migrate: ") || !strings.Contains(env.Data.ActionRun.Failure, "diverged") {
		t.Fatalf("failure = %q", env.Data.ActionRun.Failure)
	}
	if env.Supavise["status"] != "MIGRATIONS_FAILED" || env.Supavise["operation"] != "merge" {
		t.Fatalf("supavise = %v", env.Supavise)
	}
	for _, s := range env.Data.ActionRun.Steps {
		if s.Name == "migrate" && s.Status != "dead" {
			t.Fatalf("migrate is %q, want dead", s.Status)
		}
	}
}

// Without the system project the node has no key; the notification still goes out, unsigned.
func TestNotifyWithoutAKeyIsUnsigned(t *testing.T) {
	rcv := newReceiver(t)
	h := newHarness(t, func(c *config.Config) { c.Branching.AllowPrivateNotifyURLs = true })
	h.create("n", func(in *CreateInput) { in.NotifyURL = rcv.srv.URL })
	h.svc.Drain(context.Background())
	hooks := rcv.hooks()
	if len(hooks) != 1 || hooks[0].header.Get("webhook-signature") != "" {
		t.Fatalf("hooks = %+v", hooks)
	}
}

// The SSRF guard still holds: a notify_url on a private address is not called.
func TestNotifyStillRefusesPrivateTargets(t *testing.T) {
	rcv := newReceiver(t)
	h := newHarness(t, nil) // allow_private_notify_urls is off: the receiver is on 127.0.0.1
	withSystem(t, h)
	h.create("n", func(in *CreateInput) { in.NotifyURL = rcv.srv.URL })
	h.svc.Drain(context.Background())
	if hooks := rcv.hooks(); len(hooks) != 0 {
		t.Fatalf("a loopback target was called: %d time(s)", len(hooks))
	}
	if err := validNotifyURL("ftp://example.com/x"); err == nil {
		t.Fatal("an ftp notify_url is accepted")
	}
}
