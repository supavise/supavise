package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/notice"
)

// dataAPIConverger is the Engine's ConvergeDataAPI.
type dataAPIConverger interface {
	ConvergeDataAPI(ctx context.Context) map[string]error
}

// How often the leader looks for a running project that no release has put through the Data API
// step (lifecycle.Engine.ConvergeDataAPI): soon after a pass left one undone, and now and then
// after a clean one, for a project that appeared without a decision (a restore as new of a
// deleted project).
const (
	dataAPIRetry = 5 * time.Minute
	dataAPIIdle  = time.Hour
)

// convergeDataAPI revokes the default privileges of "public" for the Data API roles of every project
// that existed before the release that does so (hosted Supabase does the same to all its projects on
// 2026-10-30). It waits for the projects to be started, and while a node upgrade is running: an upgrade
// that fails rolls the node back to the release before, and a change the old release does not know
// should not have been made by then. The leader runs it; a follower's Engine would have nothing to
// write the decision to.
func convergeDataAPI(ctx context.Context, n *lifecycle.Node, log *slog.Logger) {
	upgrading := func() bool {
		u, running := notice.UpgradeRunning(n.Cfg.Paths(), time.Now())
		return running && u.Forward() && u.ProcessAlive()
	}
	convergeDataAPILoop(ctx, n.Engine, upgrading, log, dataAPIRetry, dataAPIIdle)
}

func convergeDataAPILoop(ctx context.Context, c dataAPIConverger, upgrading func() bool, log *slog.Logger, retry, idle time.Duration) {
	for {
		wait := idle
		if upgrading() {
			wait = retry
		} else if failed := c.ConvergeDataAPI(ctx); len(failed) > 0 {
			wait = retry
			for ref, err := range failed {
				log.Warn("could not revoke the default privileges of a project; trying again later", "ref", ref, "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
