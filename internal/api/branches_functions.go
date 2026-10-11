package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/secrets"
)

// functionDigest is the content digest of one function deployment: its settings (verify_jwt, the
// entrypoint, the import map) and the SHA-256 of each stored file, in path order. Two deployments with
// the same digest serve the same function. It is the ezbr_sha256 of the function API, which Studio's
// merge page compares to tell equal functions from changed ones, and what branching tracks to know
// which side changed a function. (The formula is also the one FunctionDigests uses; a test pins them.)
func functionDigest(f *Function, files []FunctionFileHash) string {
	sorted := append([]FunctionFileHash(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	fmt.Fprintf(h, "%t\x00%s\x00%s\x00", f.VerifyJWT, f.EntrypointPath, f.ImportMapPath)
	for _, file := range sorted {
		fmt.Fprintf(h, "%s\x00%s\x00", file.Path, file.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// digestsOf returns the digest of each of fns (slug to digest). The file hashes are made by the
// store, so the bodies of the functions are not read.
func digestsOf(ctx context.Context, st Store, ref string, fns []Function) (map[string]string, error) {
	out := make(map[string]string, len(fns))
	if len(fns) == 0 {
		return out, nil
	}
	var slugs []string
	if len(fns) < 64 { // for a long list one pass over the project is cheaper than a long filter
		for i := range fns {
			slugs = append(slugs, fns[i].Slug)
		}
	}
	hashes, err := st.FunctionFileHashes(ctx, ref, slugs)
	if err != nil {
		return nil, err
	}
	for i := range fns {
		out[fns[i].Slug] = functionDigest(&fns[i], hashes[fns[i].Slug])
	}
	return out, nil
}

// withDigest adds ezbr_sha256 to a function's JSON.
func withDigest(m map[string]any, digest string) map[string]any {
	if digest != "" {
		m["ezbr_sha256"] = digest
	}
	return m
}

// BranchFunctions is the branching.FunctionCarrier over the API's store of Edge Function
// deployments: it copies a project's deployments to another, keeping what makes them equal, and
// stores the secrets of a branch's create body.
type BranchFunctions struct {
	st  Store
	sec secrets.Secrets
	now func() time.Time

	mu   sync.Mutex
	hook FunctionsHook
}

var _ branching.FunctionCarrier = (*BranchFunctions)(nil)

// NewBranchFunctions returns the carrier over st; sec seals the secrets it stores.
func NewBranchFunctions(st Store, sec secrets.Secrets) *BranchFunctions {
	return &BranchFunctions{st: st, sec: sec, now: time.Now}
}

// SetHook sets the hook that is told after a copy or a secret write (the Edge Functions syncer), so
// that the runtime serves the copied functions at once and not at its next reconcile. It may be set
// after the carrier is handed to the branching service.
func (b *BranchFunctions) SetHook(h FunctionsHook) {
	b.mu.Lock()
	b.hook = h
	b.mu.Unlock()
}

// Digests implements branching.FunctionSource.
func (b *BranchFunctions) Digests(ctx context.Context, ref string) (map[string]string, error) {
	fns, err := b.st.ListFunctions(ctx, ref)
	if err != nil {
		return nil, err
	}
	return digestsOf(ctx, b.st, ref, fns)
}

// Copy implements branching.FunctionCarrier. A function the target lacks is created with a new id and
// the source's version and update time; one it has keeps its id and creation time and gets the next
// version. Files and settings are copied as stored (a bundle stays a bundle), so the digests agree.
func (b *BranchFunctions) Copy(ctx context.Context, from, to string, slugs []string) (map[string]string, error) {
	fns, err := b.st.ListFunctions(ctx, from)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, s := range slugs {
		want[s] = true
	}
	var copied []Function
	for i := range fns {
		f := fns[i]
		if slugs != nil && !want[f.Slug] {
			continue
		}
		files, err := b.st.FunctionFiles(ctx, from, f.Slug)
		if err != nil {
			return nil, err
		}
		nf := Function{Ref: to, Slug: f.Slug, Name: f.Name, Status: f.Status, VerifyJWT: f.VerifyJWT,
			EntrypointPath: f.EntrypointPath, ImportMapPath: f.ImportMapPath}
		cur, err := b.st.GetFunction(ctx, to, f.Slug)
		switch {
		case errors.Is(err, ErrNotFound):
			nf.ID, nf.Version, nf.CreatedAt, nf.UpdatedAt = secrets.NewUUID(), f.Version, b.now().UTC(), f.UpdatedAt
		case err != nil:
			return nil, err
		default:
			nf.ID, nf.Version, nf.CreatedAt, nf.UpdatedAt = cur.ID, cur.Version+1, cur.CreatedAt, b.now().UTC()
		}
		if err := b.st.RestoreFunction(ctx, nf, files); err != nil {
			return nil, err
		}
		copied = append(copied, nf)
	}
	return digestsOf(ctx, b.st, to, copied)
}

// PutSecrets implements branching.FunctionCarrier.
func (b *BranchFunctions) PutSecrets(ctx context.Context, ref string, in map[string]string) error {
	sealed := make(map[string][]byte, len(in))
	for name, v := range in {
		s, err := b.sec.Seal([]byte(v))
		if err != nil {
			return err
		}
		sealed[name] = s
	}
	return b.st.PutFunctionSecrets(ctx, ref, sealed)
}

// Applied implements branching.FunctionCarrier.
func (b *BranchFunctions) Applied(ctx context.Context, ref string) error {
	b.mu.Lock()
	h := b.hook
	b.mu.Unlock()
	if h == nil {
		return nil
	}
	return h.FunctionsChanged(ctx, ref)
}
