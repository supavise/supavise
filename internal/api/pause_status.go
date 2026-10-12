package api

import (
	"context"
	"net/http"
	"time"

	plat "github.com/supavise/supavise/internal/api/gen/platform"
	"github.com/supavise/supavise/internal/registry"
)

// restoreWindowDays is hosted's window for restoring a paused project, one year
// (https://supabase.com/docs/guides/platform/upgrading). It is what Studio shows as the longest a
// project can wait; this node enforces no limit, so remaining_days_till_restore_disabled is null.
const restoreWindowDays = 365

// platformPauseStatus answers GET /platform/projects/{ref}/pause/status, which Studio reads on the page
// of a paused project and of a paused branch (components/layouts/ProjectLayout/PausedState/
// ProjectPausedState.tsx): whether Resume is offered, until when, and whether a backup can be
// downloaded.
//
// Any project that Resume accepts can be restored, so can_restore is true. A null
// remaining_days_till_restore_disabled is Studio's "no expiry" (the page says "you can resume this project
// from the dashboard" without a date), because a node keeps a paused project's data until its owner
// deletes it. latest_downloadable_backup_id is null: there is no archive download here, and the page's
// "Export your data" menu then says "No backups available for download" (DownloadBackupsSection.tsx; the
// storage archive half is refused in backups.go). last_paused_on is when the pause began, from the
// lifecycle store; a project paused before the store existed has its last change.
func (s *Server) platformPauseStatus(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	out := &plat.PauseStatusResponseOutput{CanRestore: true, MaxDaysTillRestoreDisabled: restoreWindowDays}
	if p.Status == registry.StatusInactive {
		paused := p.UpdatedAt
		if st := registry.BranchLifecycleOf(s.reg); st != nil {
			if pz, err := st.GetPause(r.Context(), p.Ref); err == nil {
				paused = pz.PausedAt
			}
		}
		t := paused.UTC().Format(time.RFC3339)
		out.LastPausedOn = &t
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// refuseBranchRef answers 404 for a branch's ref where the v1 API takes a project: hosted lists the branches
// of a project, not of one of its branches, and the CLI reads the 404 as "this is not the project to branch
// from". Studio lists a branch's family through the parent (parent_project_ref).
func (s *Server) refuseBranchRef(ctx context.Context, ref string) error {
	if p, err := s.reg.GetProject(ctx, ref); err == nil && p.Branch != nil {
		return errNoProject
	}
	return nil
}

// wakeIdleBranch starts p if it is a branch the node paused for idleness, waiting up to the wake timeout,
// and returns it running. Any other paused project, and a start that fails, are not woken (false).
func (s *Server) wakeIdleBranch(ctx context.Context, p *registry.Project) (*registry.Project, bool) {
	if p.Branch == nil || s.branches == nil || s.branches.PauseReason(ctx, p.Ref) != registry.PauseIdle {
		return nil, false
	}
	wctx, cancel := context.WithTimeout(ctx, s.cfg.Branching.IdleWakeTimeout())
	defer cancel()
	if err := s.branches.Wake(wctx, p.Ref); err != nil {
		s.log.Warn("could not wake an idle branch for an API call", "ref", p.Ref, "err", err)
		return nil, false
	}
	np, err := s.reg.GetProject(ctx, p.Ref)
	if err != nil || !np.Status.Running() {
		return nil, false
	}
	return np, true
}
