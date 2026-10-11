package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// fakeGitHub is the parts of GitHub's API the integration uses, over httptest. Real GitHub is
// never contacted by a test.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	key *rsa.PrivateKey

	mu       sync.Mutex
	repoID   int64
	repo     string // owner/name
	instID   int64
	appID    int64
	files    map[string]map[string]string // commit sha -> path -> content
	prFiles  map[int][]string
	checks   map[int64]*fakeCheck
	comments map[int64]*fakeComment
	next     int64
	calls    []string
	// codes maps an OAuth code to the user token it yields.
	userToken     string
	refreshToken  string
	refreshed     int
	tokenTTL      int64
	manifestCodes map[string]bool
	webhookSecret string
	clientSecret  string
	missingInst   bool
	truncated     bool
	defaultBranch string
	branches      []string
}

type fakeCheck struct {
	ID         int64
	Name       string
	HeadSHA    string
	Status     string
	Conclusion string
	Title      string
	Summary    string
	Updates    int
}

type fakeComment struct {
	ID      int64
	PR      int
	Body    string
	Updates int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGitHub{t: t, key: key, repoID: 1001, repo: "acme/app", instID: 42, appID: 77, files: map[string]map[string]string{},
		prFiles: map[int][]string{}, checks: map[int64]*fakeCheck{}, comments: map[int64]*fakeComment{}, next: 1000,
		userToken: "ghu_user", refreshToken: "ghr_refresh", tokenTTL: 28800, manifestCodes: map[string]bool{"good-manifest-code": true},
		webhookSecret: "hook-secret", clientSecret: "client-secret", defaultBranch: "main", branches: []string{"main", "feature"}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", f.instToken)
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/trees/{sha}", f.tree)
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/blobs/{sha}", f.blob)
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}", f.contents)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{n}/files", f.pullFiles)
	mux.HandleFunc("POST /repos/{owner}/{repo}/check-runs", f.createCheck)
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/check-runs/{id}", f.updateCheck)
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{n}/comments", f.createComment)
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/issues/comments/{id}", f.updateComment)
	mux.HandleFunc("GET /repos/{owner}/{repo}/branches", f.listBranches)
	mux.HandleFunc("GET /repos/{owner}/{repo}/branches/{b...}", f.getBranch)
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{b...}", f.commit)
	mux.HandleFunc("POST /login/oauth/access_token", f.oauth)
	mux.HandleFunc("GET /user", f.user)
	mux.HandleFunc("GET /user/installations", f.userInstallations)
	mux.HandleFunc("GET /user/installations/{id}/repositories", f.userRepos)
	mux.HandleFunc("DELETE /applications/{client}/token", f.revoke)
	mux.HandleFunc("POST /app-manifests/{code}/conversions", f.convert)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) URL() string { return f.srv.URL }

func (f *fakeGitHub) id() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	return f.next
}

func (f *fakeGitHub) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGitHub) fail(w http.ResponseWriter, status int, msg string) {
	f.writeJSON(w, status, map[string]string{"message": msg})
}

func (f *fakeGitHub) pemKey() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(f.key)}))
}

// setCommit sets the files of a commit (paths from the repository root).
func (f *fakeGitHub) setCommit(sha string, files map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[sha] = files
}

func blobSHA(content string) string {
	h := sha1.Sum([]byte("blob " + strconv.Itoa(len(content)) + "\x00" + content))
	return hex.EncodeToString(h[:])
}

// callsTo counts the recorded requests that start with prefix ("POST /repos/acme/app/check-runs").
func (f *fakeGitHub) callsTo(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeGitHub) bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (f *fakeGitHub) needInstallation(w http.ResponseWriter, r *http.Request) bool {
	if f.bearer(r) != "inst-token" {
		f.fail(w, http.StatusUnauthorized, "Bad credentials")
		return false
	}
	if r.PathValue("owner")+"/"+r.PathValue("repo") != f.repo {
		f.fail(w, http.StatusNotFound, "Not Found")
		return false
	}
	return true
}

func (f *fakeGitHub) instToken(w http.ResponseWriter, r *http.Request) {
	tok, err := jwt.Parse(f.bearer(r), func(*jwt.Token) (any, error) { return &f.key.PublicKey, nil }, jwt.WithValidMethods([]string{"RS256"}))
	if err != nil || !tok.Valid {
		f.fail(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	if iss, _ := tok.Claims.GetIssuer(); iss != strconv.FormatInt(f.appID, 10) {
		f.fail(w, http.StatusUnauthorized, "wrong issuer")
		return
	}
	if r.PathValue("id") != strconv.FormatInt(f.instID, 10) || f.missingInst {
		f.fail(w, http.StatusNotFound, "Not Found")
		return
	}
	f.writeJSON(w, http.StatusCreated, map[string]any{"token": "inst-token", "expires_at": "2099-01-01T00:00:00Z"})
}

func (f *fakeGitHub) tree(w http.ResponseWriter, r *http.Request) {
	if !f.needInstallation(w, r) {
		return
	}
	f.mu.Lock()
	files, ok := f.files[r.PathValue("sha")]
	trunc := f.truncated
	f.mu.Unlock()
	if !ok {
		f.fail(w, http.StatusNotFound, "Not Found")
		return
	}
	var paths []string
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var tree []map[string]any
	for _, p := range paths {
		tree = append(tree, map[string]any{"path": p, "mode": "100644", "type": "blob", "sha": blobSHA(files[p]), "size": len(files[p])})
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"sha": r.PathValue("sha"), "tree": tree, "truncated": trunc})
}

func (f *fakeGitHub) blob(w http.ResponseWriter, r *http.Request) {
	if !f.needInstallation(w, r) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, files := range f.files {
		for _, c := range files {
			if blobSHA(c) == r.PathValue("sha") {
				w.Header().Set("Content-Type", "application/vnd.github.raw+json")
				_, _ = io.WriteString(w, c)
				return
			}
		}
	}
	f.fail(w, http.StatusNotFound, "Not Found")
}

func (f *fakeGitHub) contents(w http.ResponseWriter, r *http.Request) {
	if !f.needInstallation(w, r) {
		return
	}
	ref := r.URL.Query().Get("ref")
	f.mu.Lock()
	defer f.mu.Unlock()
	if ref == "" {
		ref = "head-" + f.defaultBranch
	}
	files, ok := f.files[ref]
	if !ok {
		f.fail(w, http.StatusNotFound, "No commit found for the ref "+ref)
		return
	}
	c, ok := files[r.PathValue("path")]
	if !ok {
		f.fail(w, http.StatusNotFound, "Not Found")
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"type": "file", "sha": blobSHA(c), "size": len(c), "encoding": "none", "content": ""})
}

func (f *fakeGitHub) pullFiles(w http.ResponseWriter, r *http.Request) {
	if !f.needInstallation(w, r) {
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]string
	for _, p := range f.prFiles[n] {
		out = append(out, map[string]string{"filename": p, "status": "modified"})
	}
	f.writeJSON(w, http.StatusOK, out)
}

func (f *fakeGitHub) createCheck(w http.ResponseWriter, r *http.Request) {
	if !f.needInstallation(w, r) {
		return
	}
	var in struct {
		Name    string `json:"name"`
		HeadSHA string `json:"head_sha"`
		Status  string `json:"status"`
		Output  struct {
			Title   string `json:"title"`
			Summary string `json:"summary"`
		} `json:"output"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	id := f.id()
	f.mu.Lock()
	f.checks[id] = &fakeCheck{ID: id, Name: in.Name, HeadSHA: in.HeadSHA, Status: in.Status, Title: in.Output.Title, Summary: in.Output.Summary}
	f.mu.Unlock()
	f.writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (f *fakeGitHub) updateCheck(w http.ResponseWriter, r *http.Request) {
	if !f.needInstallation(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		Output     struct {
			Title   string `json:"title"`
			Summary string `json:"summary"`
		} `json:"output"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.checks[id]
	if !ok {
		f.fail(w, http.StatusNotFound, "Not Found")
		return
	}
	c.Status, c.Conclusion, c.Title, c.Summary = in.Status, in.Conclusion, in.Output.Title, in.Output.Summary
	c.Updates++
	f.writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (f *fakeGitHub) createComment(w http.ResponseWriter, r *http.Request) {
	if !f.needInstallation(w, r) {
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	var in struct {
		Body string `json:"body"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	id := f.id()
	f.mu.Lock()
	f.comments[id] = &fakeComment{ID: id, PR: n, Body: in.Body}
	f.mu.Unlock()
	f.writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (f *fakeGitHub) updateComment(w http.ResponseWriter, r *http.Request) {
	if !f.needInstallation(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		Body string `json:"body"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.comments[id]
	if !ok {
		f.fail(w, http.StatusNotFound, "Not Found")
		return
	}
	c.Body = in.Body
	c.Updates++
	f.writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (f *fakeGitHub) listBranches(w http.ResponseWriter, r *http.Request) {
	if !f.needInstallation(w, r) {
		return
	}
	var out []map[string]string
	for _, b := range f.branches {
		out = append(out, map[string]string{"name": b})
	}
	f.writeJSON(w, http.StatusOK, out)
}

func (f *fakeGitHub) getBranch(w http.ResponseWriter, r *http.Request) {
	if !f.needInstallation(w, r) {
		return
	}
	for _, b := range f.branches {
		if b == r.PathValue("b") {
			f.writeJSON(w, http.StatusOK, map[string]string{"name": b})
			return
		}
	}
	f.fail(w, http.StatusNotFound, "Branch not found")
}

func (f *fakeGitHub) commit(w http.ResponseWriter, r *http.Request) {
	if !f.needInstallation(w, r) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sha := "head-" + r.PathValue("b")
	if _, ok := f.files[sha]; !ok {
		f.fail(w, http.StatusUnprocessableEntity, "No commit found for SHA: "+r.PathValue("b"))
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]string{"sha": sha})
}

func (f *fakeGitHub) oauth(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.Form.Get("client_secret") != f.clientSecret {
		f.writeJSON(w, http.StatusOK, map[string]string{"error": "incorrect_client_credentials"})
		return
	}
	switch {
	case r.Form.Get("grant_type") == "refresh_token":
		f.mu.Lock()
		ok := r.Form.Get("refresh_token") == f.refreshToken
		if ok {
			f.refreshed++
			f.userToken = fmt.Sprintf("ghu_refreshed_%d", f.refreshed)
			f.refreshToken = fmt.Sprintf("ghr_refresh_%d", f.refreshed)
		}
		ut, rt, ttl := f.userToken, f.refreshToken, f.tokenTTL
		f.mu.Unlock()
		if !ok {
			f.writeJSON(w, http.StatusOK, map[string]string{"error": "bad_refresh_token"})
			return
		}
		f.writeJSON(w, http.StatusOK, map[string]any{"access_token": ut, "refresh_token": rt, "expires_in": ttl, "refresh_token_expires_in": 15811200, "token_type": "bearer"})
	case r.Form.Get("code") == "good-code":
		f.mu.Lock()
		ut, rt, ttl := f.userToken, f.refreshToken, f.tokenTTL
		f.mu.Unlock()
		f.writeJSON(w, http.StatusOK, map[string]any{"access_token": ut, "refresh_token": rt, "expires_in": ttl, "refresh_token_expires_in": 15811200, "token_type": "bearer"})
	default:
		f.writeJSON(w, http.StatusOK, map[string]string{"error": "bad_verification_code", "error_description": "The code passed is incorrect or expired."})
	}
}

func (f *fakeGitHub) userOK(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	want := f.userToken
	f.mu.Unlock()
	if f.bearer(r) != want {
		f.fail(w, http.StatusUnauthorized, "Bad credentials")
		return false
	}
	return true
}

func (f *fakeGitHub) user(w http.ResponseWriter, r *http.Request) {
	if f.userOK(w, r) {
		f.writeJSON(w, http.StatusOK, map[string]any{"id": 7, "login": "octo"})
	}
}

func (f *fakeGitHub) userInstallations(w http.ResponseWriter, r *http.Request) {
	if f.userOK(w, r) {
		f.writeJSON(w, http.StatusOK, map[string]any{"total_count": 1, "installations": []map[string]any{{"id": f.instID, "app_id": f.appID}}})
	}
}

func (f *fakeGitHub) userRepos(w http.ResponseWriter, r *http.Request) {
	if !f.userOK(w, r) {
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"total_count": 1, "repositories": []map[string]any{
		{"id": f.repoID, "full_name": f.repo, "default_branch": f.defaultBranch, "archived": false}}})
}

func (f *fakeGitHub) revoke(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
		f.fail(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeGitHub) convert(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	ok := f.manifestCodes[r.PathValue("code")]
	delete(f.manifestCodes, r.PathValue("code"))
	f.mu.Unlock()
	if !ok {
		f.fail(w, http.StatusNotFound, "Not Found")
		return
	}
	f.writeJSON(w, http.StatusCreated, map[string]any{"id": f.appID, "slug": "supavise-test", "name": "Supavise test", "client_id": "Iv1.test",
		"client_secret": f.clientSecret, "webhook_secret": f.webhookSecret, "pem": f.pemKey(), "html_url": f.srv.URL + "/apps/supavise-test",
		"owner": map[string]string{"login": "acme"}})
}
