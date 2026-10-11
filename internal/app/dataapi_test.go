package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
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
