package secrets

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Bounds of a JWT secret that a person supplies. PostgREST refuses a secret under 32
// characters; the upper bound keeps the value sane in the env files, the tenant bodies
// sent to Realtime and Storage, and the sealed copy in the registry.
const (
	MinJWTSecretLen = 32
	MaxJWTSecretLen = 512
)

// CheckJWTSecret reports why s cannot be a project's JWT secret, or nil. It is the rule for
// a secret that comes from outside (a project moved from hosted Supabase, the dashboard's
// "Create my own secret"); NewJWTSecret always passes it.
//
// The dashboard asks for 32 or more characters and refuses "@" and "$". Here the first
// character may not be "@", "{" or "[" because PostgREST reads a secret that starts with "@"
// as the name of a file and one that is a JSON key (set) as that key; control characters
// and surrounding whitespace are refused because they do not survive an env file, a shell
// or a copy from a terminal. Every other character is accepted, "$" included: no consumer
// here expands it.
func CheckJWTSecret(s string) error {
	switch {
	case len(s) < MinJWTSecretLen:
		return fmt.Errorf("the JWT secret must be at least %d characters long", MinJWTSecretLen)
	case len(s) > MaxJWTSecretLen:
		return fmt.Errorf("the JWT secret must be at most %d bytes long", MaxJWTSecretLen)
	case !utf8.ValidString(s):
		return errors.New("the JWT secret must be valid UTF-8")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("the JWT secret must not contain control characters or line breaks")
		}
	}
	first, _ := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return errors.New("the JWT secret must not start or end with a space")
	}
	if strings.ContainsRune("@{[", first) {
		return fmt.Errorf("the JWT secret must not start with %q (PostgREST reads such a value as a file name or a key)", first)
	}
	return nil
}

// NewProjectKeysWithSecret is NewProjectKeys with a JWT secret that is given: the legacy
// anon and service_role keys are signed with it, every other credential is generated.
func NewProjectKeysWithSecret(ref, jwtSecret string, now time.Time) (*ProjectKeys, error) {
	if err := CheckJWTSecret(jwtSecret); err != nil {
		return nil, err
	}
	k, err := NewProjectKeys(ref, now)
	if err != nil {
		return nil, err
	}
	k.JWTSecret = jwtSecret
	return k, k.ResignLegacy(ref, now)
}

// UseLegacyKey makes key the project's legacy key for role (RoleAnon or RoleServiceRole)
// instead of a key signed here, so that an app which already holds the key (from a project
// moved here from hosted Supabase) keeps working byte for byte. The key must verify under
// the project's JWT secret, carry that role and not be expired. Its ref claim is not
// compared: the proxy accepts a stored key whatever ref it names, and the signature binds
// it to this secret.
func (k *ProjectKeys) UseLegacyKey(role, key string) error {
	if role != RoleAnon && role != RoleServiceRole {
		return fmt.Errorf("secrets: unknown key role %q", role)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("the %s key is empty", role)
	}
	claims, err := ParseHS256(key, k.JWTSecret)
	if err != nil {
		return fmt.Errorf("the %s key is not a valid HS256 JWT signed with the JWT secret: %w", role, err)
	}
	if got, _ := claims["role"].(string); got != role {
		return fmt.Errorf("the %s key carries the role %q", role, got)
	}
	if role == RoleAnon {
		k.AnonKey = key
	} else {
		k.ServiceRoleKey = key
	}
	return nil
}
