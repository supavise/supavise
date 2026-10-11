package secrets

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestCheckJWTSecret(t *testing.T) {
	good := []string{
		NewJWTSecret(),
		strings.Repeat("a", MinJWTSecretLen),
		strings.Repeat("a", MaxJWTSecretLen),
		// What hosted generates: base64 with padding, "+" and "/".
		"qRz4Dk0Wm1Yt9b3nVxP7sLhC5aJe8uGfN2oIcKwXrTyZ+/Ab6MdSvE1HqOpU3lBjF9gTnRk0WzY5cX8aVm2D7eQ==",
		// Studio refuses "$" and "@" in a custom secret; Supavise's consumers expand neither.
		"abc$def@ghi-0123456789-0123456789-0123",
		"with spaces inside is fine but long enough here",
	}
	for _, s := range good {
		if err := CheckJWTSecret(s); err != nil {
			t.Errorf("CheckJWTSecret(%q) = %v", s, err)
		}
	}
	bad := map[string]string{
		"too short":            strings.Repeat("a", MinJWTSecretLen-1),
		"too long":             strings.Repeat("a", MaxJWTSecretLen+1),
		"a newline":            strings.Repeat("a", 40) + "\n",
		"a tab inside":         strings.Repeat("a", 20) + "\t" + strings.Repeat("b", 20),
		"NUL":                  strings.Repeat("a", 20) + "\x00" + strings.Repeat("b", 20),
		"a leading space":      " " + strings.Repeat("a", 40),
		"a trailing space":     strings.Repeat("a", 40) + " ",
		"file syntax":          "@" + strings.Repeat("a", 40),
		"a JSON object":        "{" + strings.Repeat("a", 40),
		"a JSON array":         "[" + strings.Repeat("a", 40),
		"invalid UTF-8":        strings.Repeat("a", 40) + "\xff",
		"a line break (CR)":    strings.Repeat("a", 20) + "\r" + strings.Repeat("b", 20),
		"a DEL control symbol": strings.Repeat("a", 20) + "\x7f" + strings.Repeat("b", 20),
	}
	for name, s := range bad {
		if err := CheckJWTSecret(s); err == nil {
			t.Errorf("CheckJWTSecret accepted %s", name)
		}
	}
}

func TestNewProjectKeysWithSecret(t *testing.T) {
	const ref = "abcdefghijklmnopqrst"
	secret := "hosted-legacy-secret-0123456789-abcdefghij"
	now := time.Now()
	k, err := NewProjectKeysWithSecret(ref, secret, now)
	if err != nil {
		t.Fatal(err)
	}
	if k.JWTSecret != secret {
		t.Fatalf("JWTSecret = %q", k.JWTSecret)
	}
	for role, key := range map[string]string{RoleAnon: k.AnonKey, RoleServiceRole: k.ServiceRoleKey} {
		claims, err := ParseHS256(key, secret)
		if err != nil {
			t.Fatalf("%s key does not verify under the given secret: %v", role, err)
		}
		if claims["role"] != role || claims["ref"] != ref {
			t.Fatalf("%s key claims = %v", role, claims)
		}
	}
	// Everything else is generated.
	if k.DBPassword == "" || k.AdminPassword == "" || k.PublishableKey == "" || k.SecretKey == "" || k.PGSodiumRootKey == "" {
		t.Fatalf("a credential was not generated: %+v", k)
	}
	if _, err := NewProjectKeysWithSecret(ref, "short", now); err == nil {
		t.Fatal("a secret under 32 characters was accepted")
	}
}

// hostedKey signs a key the way hosted does, with a ref claim that is not this project's.
func hostedKey(t *testing.T, secret, ref, role string, exp time.Time) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "supabase", "ref": ref, "role": role, "iat": time.Now().Add(-time.Hour).Unix(), "exp": exp.Unix(),
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUseLegacyKey(t *testing.T) {
	secret := "hosted-legacy-secret-0123456789-abcdefghij"
	k, err := NewProjectKeysWithSecret("aaaaaaaaaaaaaaaaaaaa", secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(24 * time.Hour)
	anon := hostedKey(t, secret, "zzzzzzzzzzzzzzzzzzzz", RoleAnon, exp)
	svc := hostedKey(t, secret, "zzzzzzzzzzzzzzzzzzzz", RoleServiceRole, exp)
	if err := k.UseLegacyKey(RoleAnon, anon+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := k.UseLegacyKey(RoleServiceRole, svc); err != nil {
		t.Fatal(err)
	}
	if k.AnonKey != anon || k.ServiceRoleKey != svc {
		t.Fatal("the given keys were not stored byte for byte (a trailing newline is trimmed)")
	}

	// Refusals leave the stored keys alone.
	other := hostedKey(t, "another-secret-0123456789-abcdefghijklmnop", "zzzzzzzzzzzzzzzzzzzz", RoleAnon, exp)
	cases := map[string]struct{ role, key string }{
		"another secret": {RoleAnon, other},
		"wrong role":     {RoleAnon, svc},
		"expired":        {RoleAnon, hostedKey(t, secret, "z", RoleAnon, time.Now().Add(-time.Minute))},
		"not a JWT":      {RoleAnon, "sb_publishable_abc"},
		"empty":          {RoleServiceRole, "  "},
		"unknown role":   {"authenticated", anon},
	}
	for name, c := range cases {
		before := *k
		if err := k.UseLegacyKey(c.role, c.key); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if *k != before {
			t.Errorf("%s: a refused key changed the stored keys", name)
		}
	}
}

func TestJWTDigestNamesTheSecretAndNothingElse(t *testing.T) {
	a := &ProjectKeys{JWTSecret: strings.Repeat("a", MinJWTSecretLen), DBPassword: "one"}
	b := &ProjectKeys{JWTSecret: strings.Repeat("a", MinJWTSecretLen), DBPassword: "two", AnonKey: "x"}
	c := &ProjectKeys{JWTSecret: strings.Repeat("b", MinJWTSecretLen)}
	if a.JWTDigest() != b.JWTDigest() {
		t.Fatal("the digest depends on a field other than the JWT secret")
	}
	if a.JWTDigest() == c.JWTDigest() {
		t.Fatal("two secrets share a digest")
	}
	if d := a.JWTDigest(); len(d) != 16 || strings.Contains(d, a.JWTSecret) {
		t.Fatalf("digest = %q", d)
	}
}
