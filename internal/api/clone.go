package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// Restore to a new project: Database > Backups > Restore to new project. The dialog lists the project's
// backups (GET .../clone), starts a clone (POST .../clone) and follows it (GET .../clone/status). The
// engine is the backup service's restore as a new project (internal/backup: RestoreWith with a new ref),
// the one `supavise backups restore --as` runs.
//
//   - It is asynchronous. The restore unpacks a base backup and replays WAL, which takes minutes to hours,
//     so the POST answers 201 as soon as the request is checked and the work runs on without the request,
//     registered for Drain like every lifecycle operation. The status comes from the events of the source
//     project (clone.requested, then clone.completed or clone.failed), which outlive the process; a
//     request whose outcome was never recorded, because the process stopped, reads as FAILED unless this
//     process is still running it.
//   - What the new project is. A copy of the database as of the backup, or of the time asked for, with the
//     source's roles and Vault, and new JWT secret and API keys. Like hosted's, it does not carry the
//     source's Storage objects, Edge Functions or Auth settings: the dialog says so. The new project is in
//     the organization the backup recorded (the source's), on the server that served the request, with the
//     name and the database password the dialog asked for.
//   - Who may. Owners and Administrators, like a restore in place (authz.go).

// ProjectCloner is the part of the backup service that restores a project as a new one. *backup.Service
// implements it; without it, or without a backup service, a clone is refused.
type ProjectCloner interface {
	RestoreWith(ctx context.Context, ref string, target time.Time, newRef string, opts backup.RestoreOptions) (*registry.Project, error)
}

var _ ProjectCloner = (*backup.Service)(nil)

// Events of the source project that carry a clone.
const (
	eventCloneRequested = "clone.requested"
	eventCloneCompleted = "clone.completed"
	eventCloneFailed    = "clone.failed"
)

// cloneTracker holds the clones this process is running, by target ref. The zero value is usable.
type cloneTracker struct {
	mu      sync.Mutex
	running map[string]string // target ref -> source ref
}

func (t *cloneTracker) begin(source, target string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, src := range t.running {
		if src == source {
			return false
		}
	}
	if t.running == nil {
		t.running = map[string]string{}
	}
	t.running[target] = source
	return true
}

func (t *cloneTracker) end(target string) {
	t.mu.Lock()
	delete(t.running, target)
	t.mu.Unlock()
}

func (t *cloneTracker) has(target string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.running[target]
	return ok
}

// cloner is the backup service as a ProjectCloner, nil on a node that has none.
func (s *Server) cloner() ProjectCloner {
	if s.backups == nil {
		return nil
	}
	c, _ := s.backups.(ProjectCloner)
	return c
}

func (s *Server) routesClone(add func(string, handlerFunc)) {
	add("GET /platform/database/{ref}/clone", s.cloneBackups)
	add("POST /platform/database/{ref}/clone", s.cloneProject)
	add("GET /platform/database/{ref}/clone/status", s.cloneStatus)
}

// cloneBackups answers GET /platform/database/{ref}/clone: the backups to clone from, as the Backups page
// lists them, and the size the new project will have.
func (s *Server) cloneBackups(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	bi, err := s.backupInfoOf(r.Context(), p)
	if err != nil {
		return err
	}
	resp := s.backupList("GET /platform/database/{ref}/clone", p, bi)
	set(resp, "target_compute_size", sizeOf(p).Name)
	size := 0
	if d, err := s.diskOf(p); err == nil {
		size = d.sizeGB
	}
	set(resp, "target_volume_size_gb", size)
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// cloneBody is CloneProject of the platform spec.
type cloneBody struct {
	CloneBackupID      *float64 `json:"cloneBackupId"`
	NewProjectName     string   `json:"newProjectName"`
	NewDBPass          string   `json:"newDbPass"`
	RecoveryTimeTarget *float64 `json:"recoveryTimeTarget"`
}

// cloneProject answers POST /platform/database/{ref}/clone.
func (s *Server) cloneProject(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p, err := s.loadProject(ctx, r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in cloneBody
	if err := decode(r, &in); err != nil {
		return err
	}
	name := strings.TrimSpace(in.NewProjectName)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return errf(http.StatusBadRequest, "newProjectName must be between 1 and 64 characters")
	}
	if n := utf8.RuneCountInString(in.NewDBPass); n < minDBPassword || n > maxDBPassword || strings.ContainsRune(in.NewDBPass, 0) {
		return errf(http.StatusBadRequest, "The password must be between %d and %d characters", minDBPassword, maxDBPassword)
	}
	cloner := s.cloner()
	if cloner == nil {
		return errNoBackups
	}
	bi, err := s.backupInfoOf(ctx, p)
	if err != nil {
		return err
	}
	// earliest is also zero in the second after the only backup ended (no whole second is restorable yet), and a
	// clone of that backup does not depend on a second: only a time target does.
	if len(bi.window.Backups) == 0 {
		return errf(http.StatusBadRequest, "The project has no backup to restore from yet; the first base backup is taken by the nightly timer, or by `supavise backups create`")
	}
	errTooSoon := errf(http.StatusBadRequest, "The project's only backup ended less than a second ago; pick a time again in a moment, or clone the backup itself")
	opts := backup.RestoreOptions{SkipFiles: true}
	var target time.Time
	var backupID string
	switch {
	case in.CloneBackupID != nil:
		if *in.CloneBackupID != math.Trunc(*in.CloneBackupID) || *in.CloneBackupID < 1 {
			return errf(http.StatusBadRequest, "cloneBackupId must be a backup id")
		}
		picked, err := s.pickBackup(ctx, p, bi, int64(*in.CloneBackupID))
		if err != nil {
			return err
		}
		backupID = picked.ID
		opts.BackupID, opts.ToBackup = picked.ID, true
		if in.RecoveryTimeTarget != nil {
			// A time after that backup: replay its WAL up to the time.
			if bi.earliest.IsZero() {
				return errTooSoon
			}
			target = time.Unix(int64(*in.RecoveryTimeTarget), 0).UTC()
			if target.Before(picked.StopTime) || target.After(bi.latest) {
				return errf(http.StatusBadRequest, "The recovery time must be between the end of backup %d (%s) and %s", int64(*in.CloneBackupID), ts(picked.StopTime), ts(bi.latest))
			}
			opts.ToBackup = false
		}
	case in.RecoveryTimeTarget != nil:
		if bi.earliest.IsZero() {
			return errTooSoon
		}
		target = time.Unix(int64(*in.RecoveryTimeTarget), 0).UTC()
		if target.Before(bi.earliest) || target.After(bi.latest) {
			return errf(http.StatusBadRequest, "The recovery time must be between %s and %s", ts(bi.earliest), ts(bi.latest))
		}
	default:
		return errf(http.StatusBadRequest, "Pick a backup (cloneBackupId) or a point in time (recoveryTimeTarget) to restore from")
	}

	newRef := secrets.NewRef()
	if !s.clones.begin(p.Ref, newRef) {
		return errf(http.StatusConflict, "A clone of project %s is already running", p.Ref)
	}
	end, err := s.beginOp()
	if err != nil {
		s.clones.end(newRef)
		return err
	}
	rctx := context.WithoutCancel(ctx)
	user := ""
	if pr := principalFrom(ctx); pr != nil {
		user = pr.UserID
	}
	s.recordEvent(rctx, p.Ref, eventCloneRequested, map[string]any{"as": newRef, "name": name, "backup": backupID, "target": nilTime(target), "by": user})
	go func() {
		defer end()
		defer s.clones.end(newRef)
		s.runClone(rctx, cloner, p, newRef, name, in.NewDBPass, target, opts)
	}()

	resp := base("POST /platform/database/{ref}/clone")
	set(resp, "source_project_ref", p.Ref)
	set(resp, "target_project_ref", newRef)
	set(resp, "target_instance_size", sizeOf(p).Name)
	size := 0
	if d, err := s.diskOf(p); err == nil {
		size = d.sizeGB
	}
	set(resp, "target_disk_size_gb", size)
	writeStatusFor(w, "POST /platform/database/{ref}/clone", resp)
	return nil
}

func nilTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// runClone restores the source into the new project and gives it the name and the database password asked
// for. Its outcome is an event of the source project.
func (s *Server) runClone(ctx context.Context, cloner ProjectCloner, src *registry.Project, newRef, name, password string, target time.Time, opts backup.RestoreOptions) {
	fail := func(err error) {
		s.log.Error("clone failed", "source", src.Ref, "target", newRef, "err", err)
		s.recordEvent(ctx, src.Ref, eventCloneFailed, map[string]any{"as": newRef, "error": err.Error()})
	}
	p, err := cloner.RestoreWith(ctx, src.Ref, target, newRef, opts)
	if err != nil {
		fail(err)
		return
	}
	// The restore names the project after the backup's: the dialog's name wins. It places the project in the
	// organization the backup recorded (the source's, unless the organization has been renamed since, which
	// fails the restore before anything exists).
	if p.OrgID != src.OrgID {
		s.log.Warn("the cloned project is not in the organization of its source", "source", src.Ref, "target", p.Ref, "org", p.OrgID, "source_org", src.OrgID)
	}
	p.Name = name
	if err := s.reg.UpdateProject(ctx, p); err != nil {
		fail(err)
		return
	}
	if rc, ok := s.mgr.(lifecycle.Reconfigurer); ok {
		if err := rc.SetDatabasePassword(ctx, p.Ref, password); err != nil {
			// The project is there and works with the source's password; say so instead of calling it failed.
			s.log.Error("the cloned project keeps the source's database password", "source", src.Ref, "target", p.Ref, "err", err)
			s.recordEvent(ctx, src.Ref, "clone.password_failed", map[string]any{"as": p.Ref, "error": err.Error()})
		}
	}
	s.recordEvent(ctx, src.Ref, eventCloneCompleted, map[string]any{"as": p.Ref})
	s.log.Info("clone finished", "source", src.Ref, "target", p.Ref)
}

// cloneStatus answers GET /platform/database/{ref}/clone/status: the clones of this project, newest first.
func (s *Server) cloneStatus(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p, err := s.loadProject(ctx, r.PathValue("ref"))
	if err != nil {
		return err
	}
	events, err := s.reg.ListEvents(ctx, p.Ref, 500)
	if err != nil {
		return err
	}
	type clone struct {
		requested, finished time.Time
		name                string
		outcome             string // "", "completed" or "failed"
	}
	byRef := map[string]*clone{}
	var order []string
	// Newest first: the outcome of a clone is seen before its request.
	for _, e := range events {
		var pl struct {
			As   string `json:"as"`
			Name string `json:"name"`
		}
		if json.Unmarshal(e.Payload, &pl) != nil || pl.As == "" {
			continue
		}
		switch e.Kind {
		case eventCloneRequested, eventCloneCompleted, eventCloneFailed:
		default:
			continue
		}
		c := byRef[pl.As]
		if c == nil {
			c = &clone{}
			byRef[pl.As] = c
			order = append(order, pl.As)
		}
		switch e.Kind {
		case eventCloneRequested:
			c.requested, c.name = e.CreatedAt, pl.Name
		case eventCloneCompleted:
			c.outcome, c.finished = "completed", e.CreatedAt
		case eventCloneFailed:
			c.outcome, c.finished = "failed", e.CreatedAt
		}
	}
	rows := make([]any, 0, len(order))
	for _, ref := range order {
		c := byRef[ref]
		if c.requested.IsZero() {
			continue // an outcome whose request fell out of the window
		}
		status := "FAILED"
		var targetID int64
		name := c.name
		tp, err := s.reg.GetProject(ctx, ref)
		switch {
		case err == nil:
			targetID, name = int64(tp.Seq), tp.Name
		case !errors.Is(err, registry.ErrNotFound):
			return err
		}
		switch {
		case s.clones.has(ref):
			status = "IN_PROGRESS"
		case c.outcome == "completed" && err == nil:
			status = "COMPLETED"
		case c.outcome == "completed":
			status = "REMOVED"
		}
		updated := c.requested
		if !c.finished.IsZero() {
			updated = c.finished
		}
		rows = append(rows, map[string]any{
			"project_id": projectNumID(p), "inserted_at": ts(c.requested), "updated_at": ts(updated),
			"target_project_id": targetID, "target_project": map[string]any{"ref": ref, "name": name}, "status": status,
		})
	}
	resp := base("GET /platform/database/{ref}/clone/status")
	set(resp, "id", projectNumID(p))
	set(resp, "ref", p.Ref)
	set(resp, "clones", rows)
	writeJSON(w, http.StatusOK, resp)
	return nil
}
