package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	v1 "github.com/supavise/supavise/internal/api/gen/v1"
	v2 "github.com/supavise/supavise/internal/api/gen/v2"
	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/registry"
)

// routesBranches serves the branch endpoints of the Management API (v1). Studio calls them
// directly (there is no /platform twin of the branch routes in the spec); the Supabase CLI
// (`branches ...`) and the MCP server's branch tools use the same paths. Without a
// branching service (Deps.Branching nil) a project has only its default branch.
func (s *Server) routesBranches(add func(string, handlerFunc)) {
	add("GET /v1/projects/{ref}/branches", s.listBranches)
	add("POST /v1/projects/{ref}/branches", s.createBranch)
	add("POST /v2/projects/{ref}/branches", s.createBranchV2)
	add("DELETE /v1/projects/{ref}/branches", s.disableBranching)
	add("GET /v1/projects/{ref}/branches/{name}", s.getBranchByName)
	add("GET /v1/branches/{branch_id_or_ref}", s.getBranch)
	add("PATCH /v1/branches/{branch_id_or_ref}", s.updateBranch)
	add("DELETE /v1/branches/{branch_id_or_ref}", s.deleteBranch)
	add("POST /v1/branches/{branch_id_or_ref}/merge", s.branchAction("merge"))
	add("POST /v1/branches/{branch_id_or_ref}/reset", s.branchAction("reset"))
	add("POST /v1/branches/{branch_id_or_ref}/push", s.branchAction("push"))
	add("POST /v1/branches/{branch_id_or_ref}/restore", s.restoreBranch)
	add("GET /v1/branches/{branch_id_or_ref}/diff", s.diffBranch)
}

// FunctionDigests is the branching.FunctionSource over the API's store of Edge Function
// deployments.
func FunctionDigests(st Store) branching.FunctionSource { return functionDigests{st} }

type functionDigests struct{ st Store }

// Digests maps each function slug of ref to a digest of its settings and source files.
func (f functionDigests) Digests(ctx context.Context, ref string) (map[string]string, error) {
	fns, err := f.st.ListFunctions(ctx, ref)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(fns))
	for _, fn := range fns {
		files, err := f.st.FunctionFiles(ctx, ref, fn.Slug)
		if err != nil {
			return nil, err
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		h := sha256.New()
		fmt.Fprintf(h, "%t\x00%s\x00%s\x00", fn.VerifyJWT, fn.EntrypointPath, fn.ImportMapPath)
		for _, file := range files {
			fc := sha256.Sum256(file.Content)
			fmt.Fprintf(h, "%s\x00%x\x00", file.Path, fc)
		}
		out[fn.Slug] = hex.EncodeToString(h.Sum(nil))
	}
	return out, nil
}

// mapBranchErr turns branching errors into API errors.
func mapBranchErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, branching.ErrNotFound), errors.Is(err, registry.ErrNotFound):
		return errf(http.StatusNotFound, "Branch not found")
	case errors.Is(err, branching.ErrInvalid), errors.Is(err, branching.ErrLimit):
		return errf(http.StatusBadRequest, "%s", trimSentinel(err))
	case errors.Is(err, branching.ErrConflict):
		return errf(http.StatusConflict, "%s", trimSentinel(err))
	}
	return mapErr(err)
}

// trimSentinel drops the package-level sentinel's text from an error message that wraps it.
func trimSentinel(err error) string {
	msg := err.Error()
	for _, s := range []error{branching.ErrInvalid, branching.ErrConflict, branching.ErrLimit} {
		if errors.Is(err, s) {
			if p := s.Error() + ": "; len(msg) > len(p) && msg[:len(p)] == p {
				return msg[len(p):]
			}
		}
	}
	return msg
}

func (s *Server) noBranching() error {
	return errf(http.StatusBadRequest, "Branching is not enabled on this node")
}

// branchOut is the spec's branch response plus supavise's own fields: the branch's outbound
// network policy (denied, pending, allowed or unenforced), so a client can see whether a branch
// with data can reach the outside world; what the last operation said (the reason, after a
// failure); when the sweeper deletes the branch; and the region it runs in (its parent's).
// Clients that decode the spec's shape ignore them.
type branchOut struct {
	*v1.BranchResponseOutput
	SupaviseEgress    string     `json:"supavise_egress,omitempty"`
	SupaviseDetail    string     `json:"supavise_detail,omitempty"`
	SupaviseExpiresAt *time.Time `json:"supavise_expires_at,omitempty"`
	SupaviseRegion    string     `json:"supavise_region,omitempty"`
}

func branchJSON(b *branching.Branch) (*branchOut, error) {
	var id openapi_types.UUID
	if err := id.UnmarshalText([]byte(b.ID)); err != nil {
		return nil, err
	}
	out := &v1.BranchResponseOutput{
		Id: id, Name: b.Name, ProjectRef: b.Ref, ParentProjectRef: b.ParentRef, IsDefault: b.IsDefault,
		Persistent: b.Persistent, Status: v1.BranchResponseOutputStatus(b.State), CreatedAt: b.CreatedAt.UTC(), UpdatedAt: b.UpdatedAt.UTC(),
		WithData: b.WithData,
	}
	if b.GitBranch != "" {
		g := b.GitBranch
		out.GitBranch = &g
	}
	if b.NotifyURL != "" {
		n := b.NotifyURL
		out.NotifyUrl = &n
	}
	if b.ProjectStatus != "" {
		st := v1.BranchResponseOutputPreviewProjectStatus(b.ProjectStatus)
		out.PreviewProjectStatus = &st
	}
	if b.DeletionScheduledAt != nil {
		t := b.DeletionScheduledAt.UTC()
		out.DeletionScheduledAt = &t
	}
	if b.ReviewRequestedAt != nil {
		t := b.ReviewRequestedAt.UTC()
		out.ReviewRequestedAt = &t
	}
	bo := &branchOut{BranchResponseOutput: out, SupaviseEgress: b.Egress, SupaviseDetail: b.Detail, SupaviseRegion: b.Region}
	if b.ExpiresAt != nil && !b.Persistent {
		t := b.ExpiresAt.UTC()
		bo.SupaviseExpiresAt = &t
	}
	return bo, nil
}

func (s *Server) writeBranch(w http.ResponseWriter, status int, b *branching.Branch) error {
	out, err := branchJSON(b)
	if err != nil {
		return err
	}
	writeJSON(w, status, out)
	return nil
}

func (s *Server) listBranches(w http.ResponseWriter, r *http.Request) error {
	if s.branches == nil {
		if _, err := s.loadProject(r.Context(), r.PathValue("ref")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, []any{})
		return nil
	}
	list, err := s.branches.List(r.Context(), r.PathValue("ref"))
	if err != nil {
		return s.branchProjectErr(err)
	}
	out := make([]*branchOut, 0, len(list))
	for i := range list {
		j, err := branchJSON(&list[i])
		if err != nil {
			return err
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// branchProjectErr is mapBranchErr for routes whose path names a project: a missing
// project is the usual project 404.
func (s *Server) branchProjectErr(err error) error {
	if errors.Is(err, branching.ErrNotFound) {
		return errNoProject
	}
	return mapBranchErr(err)
}

// branchCreate is a branch creation in the terms both create endpoints share: the v1 body
// (what the CLI, the MCP server and Studio send) and the v2 JSON:API body.
type branchCreate struct {
	Name, GitBranch, NotifyURL, Size string
	IsDefault, Persistent, WithData  bool
	Secrets                          map[string]string
	// ReleaseChannel and PostgresEngine are v1 fields; empty when not sent.
	ReleaseChannel, PostgresEngine string
}

// maxBranchSecrets bounds the secrets map of a create body.
const maxBranchSecrets = 100

func (s *Server) createBranch(w http.ResponseWriter, r *http.Request) error {
	done, err := s.beginOp()
	if err != nil {
		return err
	}
	defer done()
	var in v1.CreateBranchBody
	if err := decode(r, &in); err != nil {
		return err
	}
	q := branchCreate{Name: in.BranchName}
	if in.GitBranch != nil {
		q.GitBranch = *in.GitBranch
	}
	if in.IsDefault != nil {
		q.IsDefault = *in.IsDefault
	}
	if in.Persistent != nil {
		q.Persistent = *in.Persistent
	}
	if in.WithData != nil {
		q.WithData = *in.WithData
	}
	if in.NotifyUrl != nil {
		q.NotifyURL = *in.NotifyUrl
	}
	if in.DesiredInstanceSize != nil {
		q.Size = string(*in.DesiredInstanceSize)
	}
	if in.Secrets != nil {
		q.Secrets = *in.Secrets
	}
	if in.ReleaseChannel != nil {
		q.ReleaseChannel = string(*in.ReleaseChannel)
	}
	if in.PostgresEngine != nil {
		q.PostgresEngine = string(*in.PostgresEngine)
	}
	// region is accepted: a branch runs where its parent runs, and the answer says where (supavise_region).
	b, err := s.createBranchFrom(r, r.PathValue("ref"), q)
	if err != nil {
		return err
	}
	return s.writeBranch(w, http.StatusCreated, b)
}

// createBranchV2 is POST /v2/projects/{ref}/branches: the same creation with a JSON:API body and
// answer. Nothing in the spec's body or answer carries the status of the branch; a client reads
// it from the v1 endpoints, as for v1.
func (s *Server) createBranchV2(w http.ResponseWriter, r *http.Request) error {
	done, err := s.beginOp()
	if err != nil {
		return err
	}
	defer done()
	var in v2.V2CreateBranchRequest
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.Data.Type != "branch" {
		return errf(http.StatusBadRequest, `data.type must be "branch"`)
	}
	at := in.Data.Attributes
	if at.DesiredDiskSizeGb != nil {
		return errf(http.StatusBadRequest, "desired_disk_size_gb is not supported: a branch uses the disk of the node it runs on, which is its parent's")
	}
	q := branchCreate{Name: at.Name}
	if at.GitBranch != nil {
		q.GitBranch = *at.GitBranch
	}
	if at.Persistent != nil {
		q.Persistent = *at.Persistent
	}
	if at.WithData != nil {
		q.WithData = *at.WithData
	}
	if at.NotifyUrl != nil {
		q.NotifyURL = *at.NotifyUrl
	}
	if at.DesiredInstanceSize != nil {
		q.Size = string(*at.DesiredInstanceSize)
	}
	b, err := s.createBranchFrom(r, r.PathValue("ref"), q)
	if err != nil {
		return err
	}
	var id openapi_types.UUID
	if err := id.UnmarshalText([]byte(b.ID)); err != nil {
		return err
	}
	var out v2.V2BranchResponseOutput
	out.Data.Type, out.Data.Id = v2.V2BranchResponseOutputDataTypeBranch, id
	a := &out.Data.Attributes
	a.Name, a.ProjectRef, a.ParentProjectRef, a.IsDefault = b.Name, b.Ref, b.ParentRef, b.IsDefault
	a.Persistent, a.WithData, a.CreatedAt, a.UpdatedAt = b.Persistent, b.WithData, b.CreatedAt.UTC(), b.UpdatedAt.UTC()
	if b.GitBranch != "" {
		a.GitBranch = &b.GitBranch
	}
	if b.NotifyURL != "" {
		a.NotifyUrl = &b.NotifyURL
	}
	writeJSON(w, http.StatusCreated, &out)
	return nil
}

// createBranchFrom is the creation both endpoints share: it refuses what this node cannot honor
// instead of dropping it silently (the client would believe it got what it asked for), and
// starts the branch.
func (s *Server) createBranchFrom(r *http.Request, ref string, q branchCreate) (*branching.Branch, error) {
	// A branch with data is a copy of production data: Owner or Administrator only. Schema-only
	// branches stay with the route's own rule (Developer and up).
	if q.WithData {
		p, err := s.loadProject(r.Context(), ref)
		if err != nil {
			return nil, err
		}
		if err := s.requireBranchData(r, p); err != nil {
			return nil, err
		}
	}
	if s.branches == nil {
		return nil, s.noBranching()
	}
	if len(q.Secrets) > maxBranchSecrets {
		return nil, errf(http.StatusBadRequest, "A branch takes at most %d secrets in its create body", maxBranchSecrets)
	}
	for name := range q.Secrets {
		if !secretNameRe.MatchString(name) || strings.HasPrefix(strings.ToUpper(name), "SUPABASE_") {
			return nil, errf(http.StatusBadRequest, "Invalid secret name %q: letters, digits and underscores, not starting with SUPABASE_", name)
		}
	}
	if q.ReleaseChannel != "" && q.ReleaseChannel != "ga" {
		return nil, errf(http.StatusBadRequest, "release_channel %q is not available on this node (only ga)", q.ReleaseChannel)
	}
	if q.PostgresEngine != "" {
		// A branch is a clone of its parent's cluster, so it runs the parent's Postgres.
		p, err := s.loadProject(r.Context(), ref)
		if err != nil {
			return nil, err
		}
		if want := pgEngine(p); q.PostgresEngine != want {
			return nil, errf(http.StatusBadRequest, "postgres_engine %q is not available: a branch runs its parent's Postgres %s", q.PostgresEngine, want)
		}
	}
	// with_data makes the branch a copy of production: users' sessions are removed and the keys supavise
	// issued are replaced, but node-local credentials users stored in their own tables, function
	// bodies or Vault entries are not detected and loopback is open on every port. It is for trusted
	// users and agents (branches are schema-only by default); the branch's detail says so.
	ci := branching.CreateInput{
		Name: q.Name, GitBranch: q.GitBranch, IsDefault: q.IsDefault, Persistent: q.Persistent, WithData: q.WithData,
		NotifyURL: q.NotifyURL, DesiredInstanceSize: q.Size, Secrets: q.Secrets,
		// The Management API has no field for this; the query parameter is ours (like force on merge).
		AllowEgress: r.URL.Query().Get("allow_egress") == "true",
	}
	b, err := s.branches.Create(r.Context(), ref, ci)
	if err != nil {
		return nil, s.branchProjectErr(err)
	}
	if len(q.Secrets) > 0 && !b.IsDefault {
		if err := s.storeBranchSecrets(r.Context(), b.Ref, q.Secrets); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// storeBranchSecrets writes the secrets map of a create body as the branch's Edge Function
// secrets. Secrets are per branch: nothing is copied from the parent. The branch's project row
// exists a moment after Create returns on a node that is slow to start it, so the write waits
// for it. (The map also reaches the service in CreateInput.Secrets.)
func (s *Server) storeBranchSecrets(ctx context.Context, ref string, in map[string]string) error {
	sealed := make(map[string][]byte, len(in))
	for name, v := range in {
		b, err := s.sec.Seal([]byte(v))
		if err != nil {
			return err
		}
		sealed[name] = b
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		err := s.store.PutFunctionSecrets(ctx, ref, sealed)
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return errf(http.StatusInternalServerError, "The branch was created but its secrets could not be stored (set them with: supabase secrets set --project-ref %s): %v", ref, err)
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	return s.functionsChanged(ctx, ref)
}

func (s *Server) disableBranching(w http.ResponseWriter, r *http.Request) error {
	done, err := s.beginOp()
	if err != nil {
		return err
	}
	defer done()
	if s.branches == nil {
		return s.noBranching()
	}
	refs, _ := s.branches.Children(r.Context(), r.PathValue("ref")) // before they go
	err = s.branches.DisableBranching(r.Context(), r.PathValue("ref"))
	for _, ref := range refs {
		if _, gerr := s.reg.GetProject(r.Context(), ref); errors.Is(gerr, registry.ErrNotFound) {
			s.projectGone(r.Context(), ref)
		}
	}
	if err != nil {
		return s.branchProjectErr(err)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) getBranchByName(w http.ResponseWriter, r *http.Request) error {
	if s.branches == nil {
		return errf(http.StatusNotFound, "Branch not found")
	}
	b, err := s.branches.Get(r.Context(), r.PathValue("ref"), r.PathValue("name"))
	if err != nil {
		if errors.Is(err, branching.ErrNotFound) {
			// An unknown project and an unknown branch name are both "branch not found" here.
			return errf(http.StatusNotFound, "Branch not found")
		}
		return mapBranchErr(err)
	}
	return s.writeBranch(w, http.StatusOK, b)
}

// getBranch returns the connection details of a branch (BranchDetailResponse).
func (s *Server) getBranch(w http.ResponseWriter, r *http.Request) error {
	if s.branches == nil {
		return errf(http.StatusNotFound, "Branch not found")
	}
	b, err := s.branches.Resolve(r.Context(), r.PathValue("branch_id_or_ref"))
	if err != nil {
		return mapBranchErr(err)
	}
	p, err := s.loadProject(r.Context(), b.Ref)
	if err != nil {
		return errf(http.StatusNotFound, "Branch not found")
	}
	user := "postgres." + b.Ref
	out := &v1.BranchDetailResponseOutput{
		Ref: b.Ref, PostgresVersion: pgVersion(p), PostgresEngine: pgEngine(p), ReleaseChannel: "ga",
		Status: v1.BranchDetailResponseOutputStatus(p.Status),
		// The pooler is the one public Postgres endpoint; it routes on the user name.
		DbHost: s.cfg.PoolerHost(), DbPort: s.cfg.Ports.SupavisorSession, DbUser: &user,
	}
	// The password and JWT secret of a branch belong to the branch. The default branch is the
	// project itself: its secrets stay in the secret store, as they do for the pooler route.
	// A caller whose role cannot read the project's keys (Read-only) does not get them here either.
	showKeys := false
	if !b.IsDefault {
		if showKeys, err = s.canReadSecrets(r, p); err != nil {
			return err
		}
	}
	if showKeys {
		keys, err := s.mgr.Keys(r.Context(), b.Ref)
		switch {
		case err == nil:
			out.DbPass, out.JwtSecret = &keys.DBPassword, &keys.JWTSecret
		case p.Status == registry.StatusComingUp || b.State == registry.BranchCreatingProject:
			// The row exists a moment before its credentials are stored: answer without them;
			// the next read has them.
		default:
			return err
		}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func pgEngine(p *registry.Project) string { return itoa(int64(pgMajor(p))) }

func (s *Server) updateBranch(w http.ResponseWriter, r *http.Request) error {
	done, err := s.beginOp()
	if err != nil {
		return err
	}
	defer done()
	var in v1.UpdateBranchBody
	if err := decode(r, &in); err != nil {
		return err
	}
	if s.branches == nil {
		return errf(http.StatusNotFound, "Branch not found")
	}
	ui := branching.UpdateInput{
		Name: in.BranchName, GitBranch: in.GitBranch, Persistent: in.Persistent, NotifyURL: in.NotifyUrl, RequestReview: in.RequestReview,
	}
	if in.Status != nil {
		// The six statuses of the spec; the service refuses one while an operation owns the status.
		st := registry.BranchState(*in.Status)
		ui.State = &st
	}
	// reset_on_push is deprecated and "ignored" by the spec itself.
	b, err := s.branches.Update(r.Context(), r.PathValue("branch_id_or_ref"), ui)
	if err != nil {
		return mapBranchErr(err)
	}
	return s.writeBranch(w, http.StatusOK, b)
}

func (s *Server) deleteBranch(w http.ResponseWriter, r *http.Request) error {
	done, err := s.beginOp()
	if err != nil {
		return err
	}
	defer done()
	if s.branches == nil {
		return errf(http.StatusNotFound, "Branch not found")
	}
	// force=false asks for the soft delete with a grace period; anything else is immediate.
	schedule := r.URL.Query().Get("force") == "false"
	b, err := s.branches.Delete(r.Context(), r.PathValue("branch_id_or_ref"), branching.DeleteOptions{Schedule: schedule})
	if err != nil {
		return mapBranchErr(err)
	}
	if !schedule && b != nil { // a scheduled delete only marks the branch, which goes on running
		s.projectGone(r.Context(), b.Ref)
	}
	writeJSON(w, http.StatusOK, &v1.BranchDeleteResponseOutput{Message: "ok"})
	return nil
}

// branchAction serves merge, reset and push: the work runs in the background and the
// answer is the run id; the branch's status (GET) follows it.
func (s *Server) branchAction(op string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		done, err := s.beginOp()
		if err != nil {
			return err
		}
		defer done()
		var in v1.BranchActionBody
		if err := decode(r, &in); err != nil {
			return err
		}
		if s.branches == nil {
			return errf(http.StatusNotFound, "Branch not found")
		}
		ai := branching.ActionInput{}
		if in.MigrationVersion != nil {
			ai.MigrationVersion = *in.MigrationVersion
		}
		// The Management API has no force flag; the query parameter is ours.
		ai.Force = r.URL.Query().Get("force") == "true"
		id := r.PathValue("branch_id_or_ref")
		if op == "reset" {
			// A reset clones the parent's current data again into a branch that has data: the same
			// copy of production that creating it needs the Owner or Administrator role for.
			b, err := s.branches.Resolve(r.Context(), id)
			if err != nil {
				return mapBranchErr(err)
			}
			if b.WithData {
				p, err := s.branchParent(r.Context(), id)
				if err != nil {
					return err
				}
				if err := s.requireBranchData(r, p); err != nil {
					return err
				}
			}
		}
		var run string
		switch op {
		case "merge":
			run, err = s.branches.Merge(r.Context(), id, ai)
		case "reset":
			run, err = s.branches.Reset(r.Context(), id, ai)
		default:
			run, err = s.branches.Push(r.Context(), id, ai)
		}
		if err != nil {
			return mapBranchErr(err)
		}
		writeJSON(w, http.StatusCreated, &v1.BranchUpdateResponseOutput{Message: "ok", WorkflowRunId: run})
		return nil
	}
}

func (s *Server) restoreBranch(w http.ResponseWriter, r *http.Request) error {
	done, err := s.beginOp()
	if err != nil {
		return err
	}
	defer done()
	if s.branches == nil {
		return errf(http.StatusNotFound, "Branch not found")
	}
	if _, err := s.branches.Restore(r.Context(), r.PathValue("branch_id_or_ref")); err != nil {
		return mapBranchErr(err)
	}
	writeJSON(w, http.StatusCreated, &v1.BranchRestoreResponseOutput{Message: "Branch restoration initiated"})
	return nil
}

func (s *Server) diffBranch(w http.ResponseWriter, r *http.Request) error {
	if s.branches == nil {
		return errf(http.StatusNotFound, "Branch not found")
	}
	diff, err := s.branches.Diff(r.Context(), r.PathValue("branch_id_or_ref"))
	if err != nil {
		return mapBranchErr(err)
	}
	writeRaw(w, http.StatusOK, "text/plain; charset=utf-8", []byte(diff))
	return nil
}

// branchRefs returns the refs of the branches of a project, for Studio's project cards.
func (s *Server) branchRefs(ps []registry.Project, ref string) []string {
	out := []string{}
	for _, q := range ps {
		if q.Branch != nil && q.Branch.ParentRef == ref {
			out = append(out, q.Ref)
		}
	}
	return out
}
