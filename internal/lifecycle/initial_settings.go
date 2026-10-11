package lifecycle

import (
	"context"

	"github.com/supavise/supavise/internal/projectconfig"
)

// SettingsCopier is the part of *projectconfig.Manager the Engine uses to give a branch the saved
// settings of its parent.
type SettingsCopier interface {
	CopyForBranch(ctx context.Context, from, to string) (*projectconfig.BranchCopy, error)
}

// SizingSettings returns the Postgres settings the compute class puts on the postmaster's command
// line. A branch never takes them from its parent: it has a class of its own.
func SizingSettings() []string { return append([]string(nil), cmdlineSettings...) }

// copyInitialSettings gives a new project the saved settings of CreateRequest.SettingsFrom (its
// parent, for a branch) before its units first start, so that GoTrue, PostgREST, Storage and Realtime
// begin on the copied values: Auth providers and SMTP, MFA, hooks, PostgREST's schemas and row cap,
// Storage's limits, Realtime's and the Postgres settings that do not size the cluster. What
// identifies the parent or names its host, and what sizes it, is left out
// (projectconfig.BranchExcluded). A project made again after a failure or a reset keeps what it
// has. A failure is recorded and does not stop the project: it starts on the defaults.
func (e *Engine) copyInitialSettings(ctx context.Context, ref string, req CreateRequest) {
	if req.SettingsFrom == "" || req.Recreate {
		return
	}
	c, ok := e.opts.Settings.(SettingsCopier)
	if !ok {
		return
	}
	res, err := c.CopyForBranch(ctx, req.SettingsFrom, ref)
	payload := map[string]any{"from": req.SettingsFrom}
	if res != nil {
		copied := map[string]int{}
		for svc, names := range res.Copied {
			copied[string(svc)] = len(names)
		}
		payload["copied"] = copied
		if len(res.Failed) > 0 {
			failed := map[string]string{}
			for svc, msg := range res.Failed {
				failed[string(svc)] = msg
			}
			payload["failed"] = failed
		}
	}
	if err != nil {
		payload["error"] = err.Error()
		e.log.Error("could not copy the settings of the parent to a new project", "ref", ref, "from", req.SettingsFrom, "error", err)
	}
	e.event(ctx, ref, "project.settings_copied", payload)
}
