package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/secrets"
)

// SetJWTSecret makes fakeManager a lifecycle.JWTSecretSetter.
func (m *fakeManager) SetJWTSecret(_ context.Context, ref, secret string) (*secrets.ProjectKeys, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jwtErr != nil {
		return nil, m.jwtErr
	}
	m.jwtSecrets = append(m.jwtSecrets, ref+":"+secret)
	k, ok := m.keys[ref]
	if !ok {
		return nil, errors.New("no keys")
	}
	k.JWTSecret = secret
	return k, k.ResignLegacy(ref, time.Now())
}

const (
	jwtSecretPath = "/platform/projects/" + testRef + "/config/secrets"
	jwtStatusPath = jwtSecretPath + "/update-status"
	hostedSecret  = "hosted-legacy-secret-0123456789-abcdefghij"
)

func jwtStatus(t *testing.T, f *fixture, path string) any {
	t.Helper()
	return bodyMap(t, f.mustDo("GET", path, nil, 200))["update_status"]
}

func TestJWTSecretUpdate(t *testing.T) {
	f := newFixture(t)
	hook := &recordingHook{}
	withHook(t, f, hook)

	if jwtStatus(t, f, jwtStatusPath) != nil {
		t.Fatal("update-status is not null before a change")
	}

	// A secret of the caller's own ("Create my own secret").
	rec := f.mustDo("PATCH", jwtSecretPath, map[string]any{"jwt_secret": hostedSecret, "change_tracking_id": "track-1"}, 200)
	if bodyMap(t, rec)["message"] == "" {
		t.Fatalf("body = %s", rec.Body)
	}
	if got := f.mgr.jwtSecrets; len(got) != 1 || got[0] != testRef+":"+hostedSecret {
		t.Fatalf("manager got %v", got)
	}
	st, _ := jwtStatus(t, f, jwtStatusPath).(map[string]any)
	if st["change_tracking_id"] != "track-1" || st["status"] != float64(jwtStatusUpdated) || st["progress"] != float64(jwtProgressGatewayConfigUpdated) {
		t.Fatalf("update_status = %v", st)
	}
	if _, has := st["error"]; has {
		t.Fatalf("a finished change carries an error: %v", st)
	}
	if hook.count() != 1 {
		t.Fatalf("the Edge Functions were told %d times, want 1 (they read the secret from a file)", hook.count())
	}
	// The project's keys follow: the secret Studio reads back is the new one.
	pg := bodyMap(t, f.mustDo("GET", "/platform/projects/"+testRef+"/config/postgrest", nil, 200))
	if pg["jwt_secret"] != hostedSecret {
		t.Fatalf("config/postgrest jwt_secret = %v", pg["jwt_secret"])
	}

	// "Generate a random secret" sends ROLL.
	f.mustDo("PATCH", jwtSecretPath, map[string]any{"jwt_secret": "ROLL", "change_tracking_id": "track-2"}, 200)
	last := f.mgr.jwtSecrets[len(f.mgr.jwtSecrets)-1]
	rolled := strings.TrimPrefix(last, testRef+":")
	if rolled == "ROLL" || rolled == hostedSecret || secrets.CheckJWTSecret(rolled) != nil {
		t.Fatalf("ROLL passed %q to the manager", rolled)
	}
	if st, _ := jwtStatus(t, f, jwtStatusPath).(map[string]any); st["change_tracking_id"] != "track-2" {
		t.Fatalf("update_status = %v", st)
	}
}

func TestJWTSecretUpdateRefusals(t *testing.T) {
	f := newFixture(t)
	for name, body := range map[string]map[string]any{
		"no secret":      {"change_tracking_id": "t"},
		"empty secret":   {"jwt_secret": "", "change_tracking_id": "t"},
		"too short":      {"jwt_secret": "short", "change_tracking_id": "t"},
		"file reference": {"jwt_secret": "@" + strings.Repeat("a", 40), "change_tracking_id": "t"},
		"line break":     {"jwt_secret": strings.Repeat("a", 40) + "\n", "change_tracking_id": "t"},
	} {
		if rec := f.do("PATCH", jwtSecretPath, body); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := f.do("PATCH", jwtSecretPath, map[string]any{"jwt_secret": "short"}); !strings.Contains(rec.Body.String(), "32") {
		t.Errorf("the message of a short secret does not give the minimum: %s", rec.Body)
	}
	if len(f.mgr.jwtSecrets) != 0 {
		t.Fatalf("a refused secret reached the manager: %v", f.mgr.jwtSecrets)
	}
	if jwtStatus(t, f, jwtStatusPath) != nil {
		t.Fatal("a refused request left a status")
	}
	for _, path := range []string{"/platform/projects/zzzzzzzzzzzzzzzzzzzz/config/secrets", "/platform/projects/system/config/secrets"} {
		if rec := f.do("PATCH", path, map[string]any{"jwt_secret": hostedSecret, "change_tracking_id": "t"}); rec.Code != 404 {
			t.Errorf("PATCH %s: %d", path, rec.Code)
		}
		if rec := f.do("GET", path+"/update-status", nil); rec.Code != 404 {
			t.Errorf("GET %s/update-status: %d", path, rec.Code)
		}
	}
}

func TestJWTSecretUpdateFailures(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		code         int
		status, errc float64
		progress     float64
	}{
		{"store", &lifecycle.JWTSecretError{Step: lifecycle.KeyStepStore, Err: errors.New("boom")}, 500, jwtStatusFailed, jwtErrorConfigUpdate, jwtProgressStarted},
		{"restart", &lifecycle.JWTSecretError{Step: lifecycle.KeyStepRestart, Err: errors.New("boom")}, 500, jwtStatusFailed, jwtErrorServicesRestart, jwtProgressConfigUpdated},
		{"tenants", &lifecycle.JWTSecretError{Step: lifecycle.KeyStepTenants, Err: errors.New("boom")}, 500, jwtStatusFailed, jwtErrorGatewayUpdate, jwtProgressServicesRestarted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			hook := &recordingHook{}
			withHook(t, f, hook)
			f.mgr.jwtErr = c.err
			rec := f.do("PATCH", jwtSecretPath, map[string]any{"jwt_secret": hostedSecret, "change_tracking_id": "track-f"})
			if rec.Code != c.code || strings.Contains(rec.Body.String(), "boom") {
				t.Fatalf("%d %s: a failure answers a generic message", rec.Code, rec.Body)
			}
			st, _ := jwtStatus(t, f, jwtStatusPath).(map[string]any)
			if st["status"] != c.status || st["error"] != c.errc || st["progress"] != c.progress || st["change_tracking_id"] != "track-f" {
				t.Fatalf("update_status = %v", st)
			}
			if hook.count() != 0 {
				t.Fatal("the Edge Functions were told about a change that did not happen")
			}
		})
	}

	t.Run("refused keeps the last status", func(t *testing.T) {
		f := newFixture(t)
		f.mustDo("PATCH", jwtSecretPath, map[string]any{"jwt_secret": hostedSecret, "change_tracking_id": "first"}, 200)
		f.mgr.jwtErr = lifecycle.ErrInvalidState
		if rec := f.do("PATCH", jwtSecretPath, map[string]any{"jwt_secret": strings.Repeat("n", 40), "change_tracking_id": "second"}); rec.Code != http.StatusConflict {
			t.Fatalf("a project in a state that cannot restart: %d %s", rec.Code, rec.Body)
		}
		if st, _ := jwtStatus(t, f, jwtStatusPath).(map[string]any); st["change_tracking_id"] != "first" || st["status"] != float64(jwtStatusUpdated) {
			t.Fatalf("a refused change replaced the status: %v", st)
		}
	})
	t.Run("a refusal with no earlier change leaves nothing", func(t *testing.T) {
		f := newFixture(t)
		f.mgr.jwtErr = lifecycle.ErrInvalidState
		f.do("PATCH", jwtSecretPath, map[string]any{"jwt_secret": hostedSecret, "change_tracking_id": "x"})
		if jwtStatus(t, f, jwtStatusPath) != nil {
			t.Fatal("update-status is not null")
		}
	})
}

// Studio offers "Change legacy JWT secret" only when the legacy signing key call fails, and
// signing keys cannot be changed here: the stubs would report a key that nothing signs with.
func TestSigningKeysAreNotOffered(t *testing.T) {
	f := newFixture(t)
	const keys = "/v1/projects/" + testRef + "/config/auth/signing-keys"
	if rec := f.do("GET", keys+"/legacy", nil); rec.Code != 404 {
		t.Fatalf("GET legacy signing key: %d %s", rec.Code, rec.Body)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", keys}, {"POST", keys + "/legacy"},
		{"PATCH", keys + "/3e0d1b6a-6b4f-4a5b-9d0e-0c1d2e3f4a5b"}, {"DELETE", keys + "/3e0d1b6a-6b4f-4a5b-9d0e-0c1d2e3f4a5b"},
	} {
		rec := f.do(c.method, c.path, map[string]any{"algorithm": "ES256", "status": "standby"})
		if rec.Code != 501 || !strings.Contains(rec.Body.String(), "legacy JWT secret") {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	// The list stays what the spec-derived stub answers.
	if rec := f.do("GET", keys, nil); rec.Code != 200 {
		t.Errorf("GET signing keys: %d", rec.Code)
	}
	if rec := f.do("GET", "/v1/projects/zzzzzzzzzzzzzzzzzzzz/config/auth/signing-keys/legacy", nil); rec.Code != 404 {
		t.Errorf("unknown project: %d", rec.Code)
	}
}

func TestJWTSecretUpdateNeedsALifecycleThatCanDoIt(t *testing.T) {
	f := newFixture(t)
	f.srv.mgr = managerWithoutJWT{f.mgr}
	if rec := f.do("PATCH", jwtSecretPath, map[string]any{"jwt_secret": hostedSecret, "change_tracking_id": "t"}); rec.Code != 501 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// managerWithoutJWT hides the optional SetJWTSecret of the fake.
type managerWithoutJWT struct{ lifecycle.Manager }
