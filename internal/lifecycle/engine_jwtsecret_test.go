package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

const hostedSecret = "hosted-legacy-secret-0123456789-abcdefghij"

func TestSetJWTSecret(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	old, _ := h.e.Keys(ctx, p.Ref)
	nk, err := h.e.SetJWTSecret(ctx, p.Ref, hostedSecret)
	if err != nil {
		t.Fatal(err)
	}
	if nk.JWTSecret != hostedSecret {
		t.Fatalf("JWTSecret = %q", nk.JWTSecret)
	}
	// The legacy keys are signed again, as on hosted; the old ones stop verifying.
	for role, key := range map[string]string{secrets.RoleAnon: nk.AnonKey, secrets.RoleServiceRole: nk.ServiceRoleKey} {
		claims, err := secrets.ParseHS256(key, hostedSecret)
		if err != nil || claims["role"] != role || claims["ref"] != p.Ref {
			t.Fatalf("%s key: claims %v, err %v", role, claims, err)
		}
	}
	if _, err := secrets.ParseHS256(old.AnonKey, hostedSecret); err == nil {
		t.Fatal("the old anon key verifies under the new secret")
	}
	// Nothing else changes: the opaque keys, the passwords and the pgsodium key.
	if nk.PublishableKey != old.PublishableKey || nk.SecretKey != old.SecretKey || nk.DBPassword != old.DBPassword ||
		nk.AdminPassword != old.AdminPassword || nk.AuthenticatorPassword != old.AuthenticatorPassword || nk.PGSodiumRootKey != old.PGSodiumRootKey {
		t.Fatal("a credential other than the JWT secret and the legacy keys changed")
	}
	if stored, _ := h.e.Keys(ctx, p.Ref); *stored != *nk {
		t.Fatal("the new keys were not stored")
	}
	// GoTrue and PostgREST restart on the new secret, then the fleet tenants get it.
	if len(h.plane.reconf) != 1 || h.plane.reconf[0] != hostedSecret {
		t.Fatalf("reconfigure got %v", h.plane.reconf)
	}
	last := h.tenant.ensured[len(h.tenant.ensured)-1]
	if last.JWTSecret != hostedSecret || last.AnonKey != nk.AnonKey || last.ServiceRoleKey != nk.ServiceRoleKey {
		t.Fatalf("fleet tenant spec = %+v", last)
	}
	evs, _ := h.reg.ListEvents(ctx, p.Ref, 10)
	if got := evs[0].Kind; got != "project.jwt_secret_changed" {
		t.Fatalf("latest event = %s", got)
	}
}

func TestSetJWTSecretSameSecretChangesNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	if _, err := h.e.SetJWTSecret(ctx, p.Ref, hostedSecret); err != nil {
		t.Fatal(err)
	}
	reconf, ensured := len(h.plane.reconf), len(h.tenant.ensured)
	before, _ := h.e.Keys(ctx, p.Ref)
	nk, err := h.e.SetJWTSecret(ctx, p.Ref, hostedSecret)
	if err != nil {
		t.Fatal(err)
	}
	if *nk != *before || len(h.plane.reconf) != reconf || len(h.tenant.ensured) != ensured {
		t.Fatal("setting the secret the project already has restarted something or changed a key")
	}
}

func TestSetJWTSecretRefusals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	old, _ := h.e.Keys(ctx, p.Ref)
	for name, secret := range map[string]string{
		"short":      "too-short",
		"file name":  "@" + strings.Repeat("a", 40),
		"line break": strings.Repeat("a", 40) + "\n",
	} {
		if _, err := h.e.SetJWTSecret(ctx, p.Ref, secret); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := h.e.SetJWTSecret(ctx, config.SystemRef, hostedSecret); !errors.Is(err, ErrInvalidState) {
		t.Errorf("system project: %v", err)
	}
	if _, err := h.e.SetJWTSecret(ctx, "zzzzzzzzzzzzzzzzzzzz", hostedSecret); err == nil {
		t.Error("unknown project: accepted")
	}
	if err := h.reg.SetProjectStatus(ctx, p.Ref, registry.StatusRestoring); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.SetJWTSecret(ctx, p.Ref, hostedSecret); !errors.Is(err, ErrInvalidState) {
		t.Errorf("restoring project: %v", err)
	}
	if got, _ := h.e.Keys(ctx, p.Ref); *got != *old || len(h.plane.reconf) != 0 {
		t.Fatal("a refused change touched the keys or restarted a unit")
	}
}

func TestSetJWTSecretRestoresOnFailure(t *testing.T) {
	steps := map[string]struct {
		fail func(h *harness)
		step KeyStep
	}{
		"restart": {func(h *harness) { h.plane.failOnce["Reconfigure"] = errors.New("gotrue will not start") }, KeyStepRestart},
		"tenants": {func(h *harness) { h.tenant.err = errors.New("realtime refused the tenant") }, KeyStepTenants},
	}
	for name, c := range steps {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			p := h.create(t)
			old, _ := h.e.Keys(ctx, p.Ref)
			c.fail(h)
			_, err := h.e.SetJWTSecret(ctx, p.Ref, hostedSecret)
			var je *JWTSecretError
			if !errors.As(err, &je) || je.Step != c.step {
				t.Fatalf("err = %v, want a JWTSecretError at %s", err, c.step)
			}
			if got, _ := h.e.Keys(ctx, p.Ref); *got != *old {
				t.Fatal("the previous keys were not restored")
			}
			// The units restarted once on the new secret and once back on the old one.
			if n := len(h.plane.reconf); name == "tenants" && (n != 2 || h.plane.reconf[1] != old.JWTSecret) {
				t.Fatalf("units not restarted on the old secret: %v", h.plane.reconf)
			}
			if name == "restart" && (len(h.plane.reconf) != 2 || h.plane.reconf[1] != old.JWTSecret) {
				t.Fatalf("units not restarted on the old secret: %v", h.plane.reconf)
			}
			if evs, _ := h.reg.ListEvents(ctx, p.Ref, 10); len(evs) != 1 || evs[0].Kind != "project.created" {
				t.Fatalf("a failed change recorded an event: %+v", evs)
			}
		})
	}
}

// A rollback that fails too is not reported as a restore: the error says the services may
// still be on the new secret, so the operator checks them.
func TestSetJWTSecretReportsAFailedRestore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	// Every restart fails: the one on the new secret and the one back on the old.
	h.plane.failOn["Reconfigure"] = errors.New("systemd cannot start postgrest")
	_, err := h.e.SetJWTSecret(ctx, p.Ref, hostedSecret)
	var je *JWTSecretError
	if !errors.As(err, &je) || je.Step != KeyStepRestart || !je.NotRestored {
		t.Fatalf("err = %v, want a JWTSecretError at restart with NotRestored", err)
	}
	if !strings.Contains(err.Error(), "could not be restored") || strings.Contains(err.Error(), "previous secret restored") {
		t.Fatalf("message = %q", err)
	}
	if n := len(h.plane.reconf); n != 2 {
		t.Fatalf("the restart on the previous keys was not tried: %v", h.plane.reconf)
	}
	// A rollback that works says so.
	h2 := newHarness(t)
	p2 := h2.create(t)
	h2.plane.failOnce["Reconfigure"] = errors.New("gotrue will not start")
	_, err = h2.e.SetJWTSecret(ctx, p2.Ref, hostedSecret)
	if !errors.As(err, &je) || je.NotRestored || !strings.Contains(err.Error(), "previous secret restored") {
		t.Fatalf("a restored change: %v", err)
	}
}

func TestRotateKeysReportsAFailedRestore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	h.plane.failOn["Reconfigure"] = errors.New("systemd cannot start postgrest")
	_, err := h.e.RotateKeys(ctx, p.Ref)
	if err == nil || !strings.Contains(err.Error(), "could not be restored") || strings.Contains(err.Error(), "previous keys restored") {
		t.Fatalf("err = %v", err)
	}
}

func TestSetJWTSecretPausedOnlyStoresKeys(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	if err := h.e.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	ensured := len(h.tenant.ensured)
	nk, err := h.e.SetJWTSecret(ctx, p.Ref, hostedSecret)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.plane.reconf) != 0 || len(h.tenant.ensured) != ensured {
		t.Fatal("a paused project must not be restarted")
	}
	if got, _ := h.e.Keys(ctx, p.Ref); got.JWTSecret != hostedSecret || *got != *nk {
		t.Fatal("keys not stored")
	}
}
