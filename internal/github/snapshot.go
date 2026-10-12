package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/gobwas/glob"
	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/branching"
)

// A Snapshot is the supabase/ directory of a repository at one commit, read through GitHub's
// tree and blob API: no git binary, no clone. It lists every file and reads a file only when
// something asks for it.
type Snapshot struct {
	Repo string // owner/name
	SHA  string // the commit
	Dir  string // the directory in the repository: <workdir>/supabase

	entries map[string]treeEntry // by path relative to Dir

	svc   *Service
	token string

	mu    sync.Mutex
	cache map[string][]byte
	// read counts the bytes fetched so far and budget is what the run may fetch in all
	// (maxSnapshotBytes): the cache keeps every blob until the run ends.
	read, budget int64
}

type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

type treeAnswer struct {
	SHA       string      `json:"sha"`
	Tree      []treeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

// Limits on what a commit may ask a node to fetch and store.
const (
	maxFileBytes      = 10 << 20  // one file
	maxMigrationBytes = 8 << 20   // one migration or seed
	maxFunctionBytes  = 40 << 20  // the files of one function
	maxSnapshotBytes  = 100 << 20 // everything one run fetches from the repository, all files together
)

// ErrNoSupabaseDir means the commit has no supabase directory at the working directory.
var ErrNoSupabaseDir = errors.New("github: the repository has no supabase directory at the connected working directory")

// loadSnapshot lists the supabase directory of the connection's repository at sha.
func (s *Service) loadSnapshot(ctx context.Context, repo string, installationID int64, workdir, sha string) (*Snapshot, error) {
	tok, err := s.installationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	sn := &Snapshot{Repo: repo, SHA: sha, Dir: path.Join(workdir, "supabase"), svc: s, token: tok, cache: map[string][]byte{}, entries: map[string]treeEntry{}, budget: maxSnapshotBytes}
	// GitHub's documentation lets the trees API take a tree sha or a ref name; the commit names its tree.
	var cm struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := s.cl.do(ctx, tok, http.MethodGet, "/repos/"+escapeSegments(repo)+"/git/commits/"+url.PathEscape(sha), nil, &cm); err != nil {
		if IsStatus(err, http.StatusNotFound) || IsStatus(err, http.StatusUnprocessableEntity) {
			return nil, fmt.Errorf("%w: commit %s of %s", ErrNotFound, short(sha), repo)
		}
		return nil, err
	}
	if cm.Tree.SHA == "" {
		return nil, fmt.Errorf("github: commit %s of %s names no tree", short(sha), repo)
	}
	var ta treeAnswer
	base := "/repos/" + escapeSegments(repo) + "/git/trees/"
	if err := s.cl.do(ctx, tok, http.MethodGet, base+url.PathEscape(cm.Tree.SHA)+"?recursive=1", nil, &ta); err != nil {
		return nil, err
	}
	prefix := sn.Dir + "/"
	if ta.Truncated {
		// The whole repository does not fit one answer: descend to the directory and list that.
		if ta, err = s.subtree(ctx, tok, repo, cm.Tree.SHA, sn.Dir); err != nil {
			return nil, err
		}
		prefix = ""
	}
	for _, e := range ta.Tree {
		if e.Type != "blob" {
			continue
		}
		if prefix != "" {
			if !strings.HasPrefix(e.Path, prefix) {
				continue
			}
			e.Path = strings.TrimPrefix(e.Path, prefix)
		}
		if !safeRel(e.Path) {
			continue // git does not allow such a name; a file that claims one is not read
		}
		sn.entries[e.Path] = e
	}
	if len(sn.entries) == 0 {
		return nil, ErrNoSupabaseDir
	}
	return sn, nil
}

// subtree reads the tree of a directory below the tree sha, recursively.
func (s *Service) subtree(ctx context.Context, tok, repo, sha, dir string) (treeAnswer, error) {
	base := "/repos/" + escapeSegments(repo) + "/git/trees/"
	cur := sha
	for _, seg := range strings.Split(dir, "/") {
		var ta treeAnswer
		if err := s.cl.do(ctx, tok, http.MethodGet, base+url.PathEscape(cur), nil, &ta); err != nil {
			return ta, err
		}
		next := ""
		for _, e := range ta.Tree {
			if e.Type == "tree" && e.Path == seg {
				next = e.SHA
			}
		}
		if next == "" {
			return treeAnswer{}, ErrNoSupabaseDir
		}
		cur = next
	}
	var ta treeAnswer
	if err := s.cl.do(ctx, tok, http.MethodGet, base+url.PathEscape(cur)+"?recursive=1", nil, &ta); err != nil {
		return ta, err
	}
	if ta.Truncated {
		return ta, fmt.Errorf("github: the supabase directory of %s is too large to list", repo)
	}
	return ta, nil
}

// safeRel reports whether a path from a tree is a plain relative path: no "..", no leading slash, no
// backslash or control character. The paths become file names in a bundler's sandbox.
func safeRel(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00\r\n") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return false
		}
	}
	return true
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Has reports whether the file exists.
func (sn *Snapshot) Has(rel string) bool {
	_, ok := sn.entries[rel]
	return ok
}

// Files returns the paths (relative to the supabase directory) that have prefix, sorted.
func (sn *Snapshot) Files(prefix string) []string {
	var out []string
	for p := range sn.entries {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// Read returns a file's content, at most limit bytes.
func (sn *Snapshot) Read(ctx context.Context, rel string, limit int64) ([]byte, error) {
	e, ok := sn.entries[rel]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, rel)
	}
	if e.Size > limit {
		return nil, fmt.Errorf("%s is %d bytes, over the limit of %d", rel, e.Size, limit)
	}
	sn.mu.Lock()
	b, ok := sn.cache[e.SHA]
	if !ok {
		// The whole run is held in memory, so the limit is on everything it reads and not on each file.
		if sn.read+e.Size > sn.budget {
			sn.mu.Unlock()
			return nil, fmt.Errorf("%s would take the run past the %d bytes it may read from the repository", rel, sn.budget)
		}
		sn.read += e.Size
	}
	sn.mu.Unlock()
	if ok {
		return b, nil
	}
	b, err := sn.svc.cl.raw(ctx, sn.token, "/repos/"+escapeSegments(sn.Repo)+"/git/blobs/"+url.PathEscape(e.SHA), limit)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	sn.mu.Lock()
	sn.cache[e.SHA] = b
	sn.mu.Unlock()
	return b, nil
}

// readAll reads the files in parallel, keeping the order.
func (sn *Snapshot) readAll(ctx context.Context, rels []string, limit int64) ([][]byte, error) {
	var sum int64
	for _, r := range rels {
		sum += sn.entries[r].Size
	}
	if sum > sn.budget {
		return nil, fmt.Errorf("%d files of %d bytes are more than a run reads (%d)", len(rels), sum, sn.budget)
	}
	out := make([][]byte, len(rels))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(6)
	for i, r := range rels {
		g.Go(func() error {
			b, err := sn.Read(gctx, r, limit)
			out[i] = b
			return err
		})
	}
	return out, g.Wait()
}

// Match returns the files that a glob of config.toml ("./seed.sql", "./seeds/*.sql") names, sorted.
func (sn *Snapshot) Match(pattern string) ([]string, error) {
	pat := strings.TrimPrefix(strings.TrimPrefix(pattern, "./"), "/")
	// "a/**/b" matches "a/b" too, as it does for the CLI's glob.
	pats := []string{pat}
	if strings.Contains(pat, "**/") {
		pats = append(pats, strings.ReplaceAll(pat, "**/", ""))
	}
	var globs []glob.Glob
	for _, p := range pats {
		g, err := glob.Compile(p, '/')
		if err != nil {
			return nil, fmt.Errorf("the pattern %q: %w", pattern, err)
		}
		globs = append(globs, g)
	}
	var out []string
	for p := range sn.entries {
		for _, g := range globs {
			if g.Match(p) {
				out = append(out, p)
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---- migrations --------------------------------------------------------------------------------

var migrationFileRE = regexp.MustCompile(`^([0-9]+)_(.*)\.sql$`)

// Migrations reads supabase/migrations/*.sql in version order, as `supabase db push` names them
// (<version>_<name>.sql). Files that do not follow the pattern are returned in skipped.
func (sn *Snapshot) Migrations(ctx context.Context) (ms []branching.Migration, skipped []string, err error) {
	var rels []string
	seen := map[string]string{}
	for _, p := range sn.Files("migrations/") {
		name := strings.TrimPrefix(p, "migrations/")
		if strings.Contains(name, "/") {
			continue
		}
		m := migrationFileRE.FindStringSubmatch(name)
		if m == nil {
			if strings.HasSuffix(name, ".sql") {
				skipped = append(skipped, name)
			}
			continue
		}
		if prev, dup := seen[m[1]]; dup {
			return nil, nil, fmt.Errorf("migrations %s and %s have the same version %s", prev, name, m[1])
		}
		seen[m[1]] = name
		rels = append(rels, p)
	}
	bodies, err := sn.readAll(ctx, rels, maxMigrationBytes)
	if err != nil {
		return nil, nil, err
	}
	for i, p := range rels {
		name := strings.TrimPrefix(p, "migrations/")
		m := migrationFileRE.FindStringSubmatch(name)
		sql := strings.TrimPrefix(string(bodies[i]), "\ufeff")
		if strings.TrimSpace(sql) == "" {
			skipped = append(skipped, name+" (empty)")
			continue
		}
		ms = append(ms, branching.Migration{Version: m[1], Name: m[2], Statements: []string{sql}})
	}
	sort.SliceStable(ms, func(i, j int) bool {
		if len(ms[i].Version) != len(ms[j].Version) {
			return len(ms[i].Version) < len(ms[j].Version)
		}
		return ms[i].Version < ms[j].Version
	})
	return ms, skipped, nil
}

// ---- functions -------------------------------------------------------------------------------

var slugRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// RepoFunction is an Edge Function of the repository, ready to deploy, with the hash of its
// source so that a later commit can tell whether it changed.
type RepoFunction struct {
	FunctionDeploy
	Hash string
}

// importMapNames are the files the CLI takes for a function's import map, in its order.
var importMapNames = []string{"deno.json", "deno.jsonc", "import_map.json"}

// Functions lists the functions of supabase/functions. A directory counts when it has its
// entrypoint (index.ts, or the one config.toml names); a directory that begins with _ is shared
// code that every function may import. Files are read only for a function whose hash differs from
// the one in have, so an unchanged function costs nothing. notes explain what was left out.
func (sn *Snapshot) Functions(ctx context.Context, cfg map[string]FunctionConfig, have map[string]string) (out []RepoFunction, notes []string, err error) {
	slugs := map[string]bool{}
	for _, p := range sn.Files("functions/") {
		rest := strings.TrimPrefix(p, "functions/")
		dir, _, nested := strings.Cut(rest, "/")
		if !nested || strings.HasPrefix(dir, "_") || strings.HasPrefix(dir, ".") {
			continue
		}
		slugs[dir] = true
	}
	for slug := range cfg {
		slugs[slug] = true
	}
	names := make([]string, 0, len(slugs))
	for s := range slugs {
		names = append(names, s)
	}
	sort.Strings(names)
	var shared, rootFiles []string
	for _, p := range sn.Files("functions/") {
		rest := strings.TrimPrefix(p, "functions/")
		dir, _, nested := strings.Cut(rest, "/")
		switch {
		case !nested && !strings.HasPrefix(dir, "."):
			rootFiles = append(rootFiles, p)
		case nested && strings.HasPrefix(dir, "_"):
			shared = append(shared, p)
		}
	}
	for _, slug := range names {
		fc := cfg[slug]
		if fc.Enabled != nil && !*fc.Enabled {
			notes = append(notes, fmt.Sprintf("function %s is disabled in config.toml; not deployed", slug))
			continue
		}
		if !slugRE.MatchString(slug) {
			notes = append(notes, fmt.Sprintf("functions/%s is not a valid function name (letters, digits, - and _, starting with a letter); not deployed", slug))
			continue
		}
		entry := strings.TrimPrefix(fc.Entrypoint, "./")
		if entry == "" {
			entry = "functions/" + slug + "/index.ts"
		}
		if !sn.Has(entry) {
			if _, configured := cfg[slug]; configured {
				notes = append(notes, fmt.Sprintf("function %s: the entrypoint %s is not in the repository; not deployed", slug, entry))
			} else {
				notes = append(notes, fmt.Sprintf("functions/%s has no index.ts; not deployed", slug))
			}
			continue
		}
		own := sn.Files("functions/" + slug + "/")
		importMap := strings.TrimPrefix(fc.ImportMap, "./")
		if importMap == "" {
			for _, n := range importMapNames {
				if sn.Has("functions/" + slug + "/" + n) {
					importMap = "functions/" + slug + "/" + n
					break
				}
			}
		}
		if importMap == "" {
			for _, n := range []string{"import_map.json", "deno.json", "deno.jsonc"} {
				if sn.Has("functions/" + n) {
					importMap = "functions/" + n
					break
				}
			}
		}
		if importMap != "" && !sn.Has(importMap) {
			notes = append(notes, fmt.Sprintf("function %s: the import map %s is not in the repository; deployed without it", slug, importMap))
			importMap = ""
		}
		files := append(append(append([]string{}, own...), shared...), rootFiles...)
		var static []string
		for _, pat := range fc.Static {
			matches, merr := sn.Match(pat)
			if merr != nil {
				notes = append(notes, fmt.Sprintf("function %s: static_files %v", slug, merr))
				continue
			}
			static = append(static, strings.TrimPrefix(pat, "./"))
			files = append(files, matches...)
		}
		files = dedupe(files)
		var total int64
		h := sha256.New()
		fmt.Fprintf(h, "entry=%s\nimport_map=%s\nverify_jwt=%v\nstatic=%s\n", entry, importMap, verifyJWT(fc), strings.Join(static, ","))
		for _, f := range files {
			e := sn.entries[f]
			total += e.Size
			fmt.Fprintf(h, "%s %s\n", f, e.SHA)
		}
		if total > maxFunctionBytes {
			notes = append(notes, fmt.Sprintf("function %s has %d bytes of files, over the limit of %d; not deployed", slug, total, int64(maxFunctionBytes)))
			continue
		}
		rf := RepoFunction{Hash: hex.EncodeToString(h.Sum(nil))}
		rf.Slug, rf.Entrypoint, rf.ImportMap, rf.VerifyJWT, rf.StaticPatterns = slug, entry, importMap, verifyJWT(fc), static
		if have[slug] != rf.Hash {
			bodies, rerr := sn.readAll(ctx, files, maxFileBytes)
			if rerr != nil {
				return nil, notes, fmt.Errorf("function %s: %w", slug, rerr)
			}
			for i, f := range files {
				rf.Files = append(rf.Files, FunctionFile{Path: f, Content: bodies[i]})
			}
		}
		out = append(out, rf)
	}
	return out, notes, nil
}

func verifyJWT(fc FunctionConfig) bool { return fc.VerifyJWT == nil || *fc.VerifyJWT }

func dedupe(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}
