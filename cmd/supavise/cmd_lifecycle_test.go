package main

import (
	"testing"
	"time"

	"github.com/supavise/supavise/internal/branching"
)

func TestExpiryColumn(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	in := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	for name, tc := range map[string]struct {
		b    branching.Branch
		want string
	}{
		"no lifetime":          {branching.Branch{}, "-"},
		"a lifetime":           {branching.Branch{ExpiresAt: in(90 * time.Minute)}, "1h30m0s"},
		"lapsed":               {branching.Branch{ExpiresAt: in(-time.Minute)}, "now"},
		"persistent":           {branching.Branch{Persistent: true, ExpiresAt: in(time.Hour)}, "-"},
		"scheduled for delete": {branching.Branch{ExpiresAt: in(48 * time.Hour), DeletionScheduledAt: in(59 * time.Minute)}, "deleted in 59m0s (restore cancels)"},
	} {
		if got := expiryColumn(&tc.b, now); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
