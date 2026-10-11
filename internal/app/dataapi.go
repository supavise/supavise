package app

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
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
// deleted project, or an in-place restore whose converge did not go through). A project that stays undone is tried less often each time (5 minutes, 15
// minutes, then every hour), and its failure is logged when the message changes or once an hour.
const (
	dataAPIRetry = 5 * time.Minute
	dataAPIIdle  = time.Hour
	// dataAPIUpgradePoll is how often it asks whether the upgrade that is running has ended.
	dataAPIUpgradePoll = 15 * time.Second
	// dataAPILogEvery is the longest a failure with the same message goes unlogged.
	dataAPILogEvery = time.Hour
)

// dataAPIBackoff is the wait after the attempt-th pass in a row that left a project undone: retry,
// three times retry, then twelve times retry.
func dataAPIBackoff(retry time.Duration, attempt int) time.Duration {
	switch {
	case attempt <= 0:
		return retry
	case attempt == 1:
		return 3 * retry
	}
	return 12 * retry
}

// dataAPIUpgradeRunning reports whether a node upgrade is running whose process is alive, in any
// phase. The rolling_back phase counts: the old release is about to return, and a change that it does
// not know (and a registry that may be restored from before it) should not be made by then.
func dataAPIUpgradeRunning(p config.Paths, now time.Time) bool {
	u, running := notice.UpgradeRunning(p, now)
	return running && u.ProcessAlive()
}

// dataAPIFailedHold is how long the pass stays back after an upgrade ended in phase "failed". That phase
// is how an upgrade ends when it could not put the node back (nodeupgrade: the registry holds a
// migration the old release does not know, which is every upgrade to a release that adds one once its
// daemon has started), and the operator then decides between fixing forward and restoring the system
// cluster for `supavise rollback`. A revoke made in between would outlast a rollback: the restored
// registry loses the row, and the project databases keep the revoked defaults. A day is the time to
// decide; a marker the operator never replaces (a fix that is not a new `supavise upgrade`) would
// otherwise hold the step for good, so the hold ends by itself, and a later upgrade that ends replaces
// the marker and ends it at once.
const dataAPIFailedHold = 24 * time.Hour

// dataAPIUpgradeFailed reports whether the last upgrade ended in phase "failed" less than
// dataAPIFailedHold before now. The marker's modification time dates it: the upgrade writes it at each
// phase change, the failing one last.
func dataAPIUpgradeFailed(p config.Paths, now time.Time) bool {
	u := notice.ReadUpgrade(p)
	if u == nil || strings.ToLower(strings.TrimSpace(u.Phase)) != "failed" {
		return false
	}
	at := u.Heartbeat
	if at.IsZero() {
		at = u.StartedAt
	}
	return !at.IsZero() && now.Sub(at) < dataAPIFailedHold
}

// convergeDataAPI revokes the default privileges of "public" for the Data API roles of every project
// that existed before the release that does so (hosted Supabase does the same to all its projects on
// 2026-10-30). It waits for the projects to be started, while a node upgrade is running, and for a day
// after one ended as failed: an upgrade that fails rolls the node back to the release before, or ends
// with the operator deciding whether to, and a change the old release does not know should not have
// been made by then. The leader runs it; a follower's Engine would have nothing to write the decision
// to.
func convergeDataAPI(ctx context.Context, n *lifecycle.Node, log *slog.Logger) {
	paths := n.Cfg.Paths()
	var holding bool // the failed-upgrade hold was logged for the marker that is there now
	upgrading := func() bool {
		now := time.Now()
		if dataAPIUpgradeRunning(paths, now) {
			return true
		}
		failed := dataAPIUpgradeFailed(paths, now)
		if failed && !holding {
			log.Warn("the last node upgrade ended as failed, so the Data API step (revoking the default privileges of the projects) waits; it goes ahead after a later upgrade ends, or a day after the failure", "marker", "system/upgrade.json")
		}
		holding = failed
		return failed
	}
	convergeDataAPILoop(ctx, n.Engine, upgrading, log, dataAPIUpgradePoll, dataAPIRetry, dataAPIIdle)
}

func convergeDataAPILoop(ctx context.Context, c dataAPIConverger, upgrading func() bool, log *slog.Logger, poll, retry, idle time.Duration) {
	type logged struct {
		msg string
		at  time.Time
	}
	var (
		attempts int // passes in a row that left a project undone
		seen     = map[string]logged{}
	)
	for {
		wait := idle
		if upgrading() {
			wait = poll
		} else if failed := c.ConvergeDataAPI(ctx); len(failed) > 0 {
			wait = dataAPIBackoff(retry, attempts)
			attempts++
			now := time.Now()
			for ref, err := range failed {
				msg := err.Error()
				if last, ok := seen[ref]; ok && last.msg == msg && now.Sub(last.at) < dataAPILogEvery {
					continue
				}
				seen[ref] = logged{msg: msg, at: now}
				log.Warn("could not revoke the default privileges of a project; trying again later", "ref", ref, "error", err)
			}
			for ref := range seen {
				if _, still := failed[ref]; !still {
					delete(seen, ref)
				}
			}
		} else {
			attempts = 0
			clear(seen)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
