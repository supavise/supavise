package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/secrets"
)

// A project moved from hosted Supabase keeps its app working when it keeps the JWT secret:
// GoTrue, PostgREST, Realtime and Storage then accept the anon and service_role keys the app
// already holds. Hosted has no create-time way to choose the secret (its dashboard changes it
// afterwards, which Supavise serves too), so `projects create` reads it from a file. The proxy
// accepts a legacy key whose ref claim is the project's own, so create the project with the
// hosted project's ref (--ref), or give the two keys with --anon-key-file and
// --service-role-key-file to have them accepted whatever the ref.

// projectKeysFromFiles is the credential set of a project that keeps a JWT secret, and the
// legacy keys when files for them are given. It returns nil keys when none is given, so that
// Create generates everything. Passwords, the opaque keys and the pgsodium key are always new.
func projectKeysFromFiles(ref string, now time.Time, secretFile, anonFile, serviceFile string) (*secrets.ProjectKeys, error) {
	if secretFile == "" {
		if anonFile != "" || serviceFile != "" {
			return nil, fmt.Errorf("--anon-key-file and --service-role-key-file need --jwt-secret-file: the keys must verify under the secret")
		}
		return nil, nil
	}
	b, err := os.ReadFile(secretFile)
	if err != nil {
		return nil, fmt.Errorf("--jwt-secret-file: %w", err)
	}
	// The line break an editor or `echo` adds is not part of the secret; anything else is.
	secret := strings.TrimRight(string(b), "\r\n")
	if err := secrets.CheckJWTSecret(secret); err != nil {
		return nil, fmt.Errorf("--jwt-secret-file %s: %w", secretFile, err)
	}
	k, err := secrets.NewProjectKeysWithSecret(ref, secret, now)
	if err != nil {
		return nil, err
	}
	for _, f := range []struct{ flag, path, role string }{
		{"--anon-key-file", anonFile, secrets.RoleAnon},
		{"--service-role-key-file", serviceFile, secrets.RoleServiceRole},
	} {
		if f.path == "" {
			continue
		}
		kb, err := os.ReadFile(f.path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.flag, err)
		}
		if err := k.UseLegacyKey(f.role, string(kb)); err != nil {
			return nil, fmt.Errorf("%s %s: %w", f.flag, f.path, err)
		}
	}
	return k, nil
}
