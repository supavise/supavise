package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/notice"
)

type fakeConverger struct {
	calls atomic.Int32
	fail  atomic.Bool
}

func (f *fakeConverger) ConvergeDataAPI(context.Context) map[string]error {
	f.calls.Add(1)
	if f.fail.Load() {
		return map[string]error{"aaaaaaaaaaaaaaaaaaaa": errors.New("down")}
	}
	return nil
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The pass runs at once, again soon after a failure, rarely after a clean one, and not while an
// upgrade runs.
func TestConvergeDataAPILoop(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &fakeConverger{}
	var upgrading atomic.Bool
	upgrading.Store(true)
	done := make(chan struct{})
	go func() {
		convergeDataAPILoop(ctx, c, upgrading.Load, log, 10*time.Millisecond, 10*time.Millisecond, time.Hour)
		close(done)
	}()
	time.Sleep(60 * time.Millisecond)
	if n := c.calls.Load(); n != 0 {
		t.Fatalf("%d passes while an upgrade was running", n)
	}
	c.fail.Store(true)
	upgrading.Store(false)
	waitFor(t, "a retry after a failure", func() bool { return c.calls.Load() >= 3 })
	c.fail.Store(false)
	var clean int32
	waitFor(t, "a clean pass", func() bool { clean = c.calls.Load(); time.Sleep(30 * time.Millisecond); return c.calls.Load() == clean })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop did not stop with its context")
	}
}

// A project that stays undone is tried less often each time, and its failure is logged once while
// the message stays the same.
func TestConvergeDataAPILoopBacksOffAndLogsOnce(t *testing.T) {
	for i, want := range []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, time.Hour} {
		if got := dataAPIBackoff(5*time.Minute, i); got != want {
			t.Errorf("backoff after %d failed passes = %v, want %v", i+1, got, want)
		}
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	ctx, cancel := context.WithCancel(context.Background())
	c := &fakeConverger{}
	c.fail.Store(true)
	done := make(chan struct{})
	go func() {
		convergeDataAPILoop(ctx, c, func() bool { return false }, log, time.Hour, 5*time.Millisecond, time.Hour)
		close(done)
	}()
	waitFor(t, "four passes", func() bool { return c.calls.Load() >= 4 })
	cancel()
	<-done
	if n := strings.Count(buf.String(), "could not revoke the default privileges"); n != 1 {
		t.Fatalf("%d warnings for one unchanged failure:\n%s", n, buf.String())
	}
}

// The pass waits for an upgrade in any phase while its process is alive, the rolling_back phase
// included, and goes ahead once the upgrade has ended.
func TestDataAPIWaitsForAnUpgradeThatRollsBack(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	p := cfg.Paths()
	now := time.Now()
	if dataAPIUpgradeRunning(p, now) {
		t.Fatal("no marker, and an upgrade is running")
	}
	for _, phase := range []string{"services", notice.PhaseRollingBack} {
		if err := notice.WriteUpgrade(p, notice.Upgrade{Phase: phase, From: "v0.2.1", To: "v0.3.0", StartedAt: now}); err != nil {
			t.Fatal(err)
		}
		if !dataAPIUpgradeRunning(p, now) {
			t.Errorf("phase %q does not hold the pass back", phase)
		}
	}
	// A marker whose process is gone, and a finished upgrade, do not.
	if err := notice.WriteUpgrade(p, notice.Upgrade{Phase: notice.PhaseRollingBack, StartedAt: now, PID: 2147483646}); err != nil {
		t.Fatal(err)
	}
	if dataAPIUpgradeRunning(p, now) {
		t.Error("an upgrade whose process is gone holds the pass back")
	}
	if err := notice.WriteUpgrade(p, notice.Upgrade{Phase: "done", StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if dataAPIUpgradeRunning(p, now) {
		t.Error("a finished upgrade holds the pass back")
	}
}
