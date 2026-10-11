package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/supavise/supavise/internal/secrets"
)

const (
	keysTestRef    = "abcdefghijklmnopqrst"
	keysTestSecret = "hosted-legacy-secret-0123456789-abcdefghij"
)

func writeKeyFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func hostedLegacyKey(t *testing.T, secret, role string) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "supabase", "ref": "zzzzzzzzzzzzzzzzzzzz", "role": role,
		"iat": time.Now().Add(-time.Hour).Unix(), "exp": time.Now().Add(24 * time.Hour).Unix(),
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProjectKeysFromFilesWithoutFilesGeneratesEverything(t *testing.T) {
	k, err := projectKeysFromFiles(keysTestRef, time.Now(), "", "", "")
	if err != nil || k != nil {
		t.Fatalf("keys = %v, err = %v: Create must generate the credentials itself", k, err)
	}
}

func TestProjectKeysFromFilesKeepsTheSecret(t *testing.T) {
	// A file written by an editor or `echo` ends in a line break.
	for _, content := range []string{keysTestSecret, keysTestSecret + "\n", keysTestSecret + "\r\n"} {
		k, err := projectKeysFromFiles(keysTestRef, time.Now(), writeKeyFile(t, content), "", "")
		if err != nil {
			t.Fatalf("%q: %v", content, err)
		}
		if k.JWTSecret != keysTestSecret {
			t.Fatalf("%q: JWTSecret = %q", content, k.JWTSecret)
		}
		// The project's own legacy keys verify under it and name the project.
		claims, err := secrets.ParseHS256(k.ServiceRoleKey, keysTestSecret)
		if err != nil || claims["ref"] != keysTestRef || claims["role"] != secrets.RoleServiceRole {
			t.Fatalf("service_role key: %v %v", claims, err)
		}
		// Everything else is new.
		if k.DBPassword == "" || k.PublishableKey == "" || k.SecretKey == "" || k.PGSodiumRootKey == "" {
			t.Fatalf("credentials not generated: %+v", k)
		}
	}
}

func TestProjectKeysFromFilesKeepsGivenKeysAsTheyAre(t *testing.T) {
	anon, svc := hostedLegacyKey(t, keysTestSecret, secrets.RoleAnon), hostedLegacyKey(t, keysTestSecret, secrets.RoleServiceRole)
	k, err := projectKeysFromFiles(keysTestRef, time.Now(), writeKeyFile(t, keysTestSecret), writeKeyFile(t, anon+"\n"), writeKeyFile(t, svc))
	if err != nil {
		t.Fatal(err)
	}
	if k.AnonKey != anon || k.ServiceRoleKey != svc {
		t.Fatal("the keys of the files were not kept byte for byte")
	}
	// One key alone is fine; the other stays the project's own.
	k, err = projectKeysFromFiles(keysTestRef, time.Now(), writeKeyFile(t, keysTestSecret), writeKeyFile(t, anon), "")
	if err != nil || k.AnonKey != anon || k.ServiceRoleKey == svc {
		t.Fatalf("anon only: %v", err)
	}
}

func TestProjectKeysFromFilesRefusals(t *testing.T) {
	secretFile := writeKeyFile(t, keysTestSecret)
	otherAnon := hostedLegacyKey(t, "another-secret-0123456789-abcdefghijklmnop", secrets.RoleAnon)
	svc := hostedLegacyKey(t, keysTestSecret, secrets.RoleServiceRole)
	missing := filepath.Join(t.TempDir(), "nope")
	cases := []struct {
		name                string
		secret, anon, svcFl string
		want                string
	}{
		{"keys without a secret", "", writeKeyFile(t, svc), "", "need --jwt-secret-file"},
		{"missing secret file", missing, "", "", "--jwt-secret-file"},
		{"short secret", writeKeyFile(t, "too-short"), "", "", "at least 32"},
		{"empty secret file", writeKeyFile(t, ""), "", "", "at least 32"},
		{"secret with trailing space", writeKeyFile(t, keysTestSecret+" \n"), "", "", "end with a space"},
		{"file reference", writeKeyFile(t, "@"+strings.Repeat("a", 40)), "", "", "PostgREST"},
		{"anon key of another secret", secretFile, writeKeyFile(t, otherAnon), "", "--anon-key-file"},
		{"service key in the anon file", secretFile, writeKeyFile(t, svc), "", "role"},
		{"missing key file", secretFile, "", missing, "--service-role-key-file"},
	}
	for _, c := range cases {
		k, err := projectKeysFromFiles(keysTestRef, time.Now(), c.secret, c.anon, c.svcFl)
		if err == nil || !strings.Contains(err.Error(), c.want) || k != nil {
			t.Errorf("%s: keys %v, err %v, want an error containing %q", c.name, k, err, c.want)
		}
	}
}
