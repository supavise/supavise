package api

import (
	"context"
	"strings"
	"testing"
)

func TestPgsodiumKeyIsReadAndNotReplaced(t *testing.T) {
	f := newFixture(t)
	const path = "/v1/projects/" + testRef + "/pgsodium"
	k, err := f.mgr.Keys(context.Background(), testRef)
	if err != nil || len(k.PGSodiumRootKey) != 64 {
		t.Fatalf("fixture keys: %v %q", err, k.PGSodiumRootKey)
	}
	got := bodyMap(t, f.mustDo("GET", path, nil, 200))
	if got["root_key"] != k.PGSodiumRootKey {
		t.Fatalf("GET returned %v, the project's key is %s", got["root_key"], k.PGSodiumRootKey)
	}
	// Supabase's guide pipes GET into PUT: a PUT that answered 200 and did nothing would make a
	// moved project look as if its Vault had come along.
	rec := f.do("PUT", path, map[string]any{"root_key": strings.Repeat("ab", 32)})
	if rec.Code != 501 || !strings.Contains(rec.Body.String(), "Vault") {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	if after, _ := f.mgr.Keys(context.Background(), testRef); after.PGSodiumRootKey != k.PGSodiumRootKey {
		t.Fatal("PUT changed the key")
	}
	for _, m := range []string{"GET", "PUT"} {
		if rec := f.do(m, "/v1/projects/zzzzzzzzzzzzzzzzzzzz/pgsodium", map[string]any{"root_key": strings.Repeat("ab", 32)}); rec.Code != 404 {
			t.Errorf("%s of an unknown project: %d", m, rec.Code)
		}
	}
}
