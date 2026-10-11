package api

import (
	"errors"
	"net/http"
	"strconv"

	v1 "github.com/supavise/supavise/internal/api/gen/v1"
	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/registry"
)

// routesActions serves the action runs of a project: the record of every create, push, merge and
// reset of a branch, with its seven steps and its log. Studio's Branches and merge pages poll
// them, and the CLI and MCP clients that start an operation can follow it here. A run is listed
// under the project that owns it (the parent for a merge, the branch for the others) and answers
// 404 under any other ref, which is how Studio knows which of the two refs it polls is the owner.
func (s *Server) routesActions(add func(string, handlerFunc)) {
	add("GET /v1/projects/{ref}/actions", s.listActionRuns)
	add("HEAD /v1/projects/{ref}/actions", s.countActionRuns)
	add("GET /v1/projects/{ref}/actions/{run_id}", s.getActionRun)
	add("GET /v1/projects/{ref}/actions/{run_id}/logs", s.getActionRunLogs)
	add("PATCH /v1/projects/{ref}/actions/{run_id}/status", s.updateActionRunStatus)
}

const (
	defaultActionRuns = 50
	maxActionRuns     = 200
)

var errNoRun = errf(http.StatusNotFound, "Action run not found")

// mapRunErr turns the errors of the run readers into API errors.
func mapRunErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, branching.ErrNotFound), errors.Is(err, registry.ErrNotFound):
		return errNoRun
	}
	return mapBranchErr(err)
}

// actionRunOut is the spec's run with check_run_id as the integer it is: the generated field is a
// float32, which cannot hold a GitHub check run id.
type actionRunOut struct {
	*v1.ActionRunResponseOutput
	CheckRunID *int64 `json:"check_run_id"`
}

// runStepJSON is one element of ActionRunResponse.run_steps, spelled once.
type runStepJSON = struct {
	CreatedAt string                                   `json:"created_at"`
	Name      v1.ActionRunResponseOutputRunStepsName   `json:"name"`
	Status    v1.ActionRunResponseOutputRunStepsStatus `json:"status"`
	UpdatedAt string                                   `json:"updated_at"`
}

func actionRunJSON(rec *registry.BranchRun, branchID string) actionRunOut {
	stamp := ts
	out := &v1.ActionRunResponseOutput{
		BranchId: branchID, CreatedAt: stamp(rec.CreatedAt), UpdatedAt: stamp(rec.UpdatedAt), Id: rec.ID,
		RunSteps: make([]runStepJSON, 0, len(rec.Steps)),
	}
	for _, st := range rec.Steps {
		out.RunSteps = append(out.RunSteps, runStepJSON{CreatedAt: stamp(st.CreatedAt), Name: v1.ActionRunResponseOutputRunStepsName(st.Name),
			Status: v1.ActionRunResponseOutputRunStepsStatus(st.Status), UpdatedAt: stamp(st.UpdatedAt)})
	}
	if rec.Workdir != "" {
		w := rec.Workdir
		out.Workdir = &w
	}
	ar := actionRunOut{ActionRunResponseOutput: out}
	if rec.CheckRunID != 0 {
		c := rec.CheckRunID
		ar.CheckRunID = &c
	}
	return ar
}

// runBranchID is the id of the branch a run works on. A branch that is gone (a run outlives its
// branch when it is a merge) reads as the nil UUID.
func (s *Server) runBranchID(r *http.Request, ref string, cache map[string]string) string {
	if id, ok := cache[ref]; ok {
		return id
	}
	id := "00000000-0000-0000-0000-000000000000"
	if b, err := s.branches.Resolve(r.Context(), ref); err == nil {
		id = b.ID
	}
	cache[ref] = id
	return id
}

// actionsOwner checks that the project exists and that this node keeps runs. Without a branching
// service a project has no runs: the list is empty and a single run is not found.
func (s *Server) actionsOwner(r *http.Request) (string, error) {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return "", err
	}
	return p.Ref, nil
}

// enumStr is the string of a generated enum value, nil when it was not sent.
func enumStr[T ~string](v *T) *string {
	if v == nil {
		return nil
	}
	s := string(*v)
	return &s
}

func (s *Server) listActionRuns(w http.ResponseWriter, r *http.Request) error {
	owner, err := s.actionsOwner(r)
	if err != nil {
		return err
	}
	offset := queryInt(r, "offset", 0)
	limit := min(max(queryInt(r, "limit", defaultActionRuns), 1), maxActionRuns)
	out := []actionRunOut{}
	total := 0
	if s.branches != nil {
		var runs []registry.BranchRun
		if runs, total, err = s.branches.ListRuns(r.Context(), owner, offset, limit); err != nil {
			return mapRunErr(err)
		}
		cache := map[string]string{}
		for i := range runs {
			out = append(out, actionRunJSON(&runs[i], s.runBranchID(r, runs[i].BranchRef, cache)))
		}
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	writeJSON(w, http.StatusOK, out)
	return nil
}

// countActionRuns is HEAD on the list: the number of runs in X-Total-Count.
func (s *Server) countActionRuns(w http.ResponseWriter, r *http.Request) error {
	owner, err := s.actionsOwner(r)
	if err != nil {
		return err
	}
	total := 0
	if s.branches != nil {
		if _, total, err = s.branches.ListRuns(r.Context(), owner, 0, 0); err != nil {
			return mapRunErr(err)
		}
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) getActionRun(w http.ResponseWriter, r *http.Request) error {
	owner, err := s.actionsOwner(r)
	if err != nil {
		return err
	}
	if s.branches == nil {
		return errNoRun
	}
	rec, err := s.branches.GetRun(r.Context(), owner, r.PathValue("run_id"))
	if err != nil {
		return mapRunErr(err)
	}
	writeJSON(w, http.StatusOK, actionRunJSON(rec, s.runBranchID(r, rec.BranchRef, map[string]string{})))
	return nil
}

func (s *Server) getActionRunLogs(w http.ResponseWriter, r *http.Request) error {
	owner, err := s.actionsOwner(r)
	if err != nil {
		return err
	}
	if s.branches == nil {
		return errNoRun
	}
	logs, err := s.branches.RunLogs(r.Context(), owner, r.PathValue("run_id"))
	if err != nil {
		return mapRunErr(err)
	}
	writeRaw(w, http.StatusOK, "text/plain; charset=utf-8", []byte(logs))
	return nil
}

// updateActionRunStatus sets the status of steps of a run that this node is not working on: the
// job of a CI system that runs the steps itself and reports them. The run's own status follows
// the steps.
func (s *Server) updateActionRunStatus(w http.ResponseWriter, r *http.Request) error {
	done, err := s.beginOp()
	if err != nil {
		return err
	}
	defer done()
	var in v1.UpdateRunStatusBody
	if err := decode(r, &in); err != nil {
		return err
	}
	owner, err := s.actionsOwner(r)
	if err != nil {
		return err
	}
	if s.branches == nil {
		return errNoRun
	}
	steps := map[string]string{}
	for name, v := range map[string]*string{
		branching.StepClone: enumStr(in.Clone), branching.StepPull: enumStr(in.Pull), branching.StepHealth: enumStr(in.Health),
		branching.StepConfigure: enumStr(in.Configure), branching.StepMigrate: enumStr(in.Migrate), branching.StepSeed: enumStr(in.Seed),
		branching.StepDeploy: enumStr(in.Deploy),
	} {
		if v != nil {
			steps[name] = *v
		}
	}
	// The spec lists seven statuses; REMOVING and RESTARTING are container words this node's steps never have.
	for name, st := range steps {
		switch st {
		case registry.StepCreated, registry.StepRunning, registry.StepExited, registry.StepPaused, registry.StepDead:
		default:
			return errf(http.StatusBadRequest, "step %s: status %s is not used by this node (CREATED, RUNNING, EXITED, PAUSED or DEAD)", name, st)
		}
	}
	if _, err := s.branches.SetRunSteps(r.Context(), owner, r.PathValue("run_id"), steps); err != nil {
		return mapRunErr(err)
	}
	writeJSON(w, http.StatusOK, &v1.UpdateRunStatusResponseOutput{Message: "ok"})
	return nil
}
