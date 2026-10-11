package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// pauseStatus reads the answer as Studio's paused page does: every key present, nulls as null.
func pauseStatus(t *testing.T, f *fixture, ref string) (map[string]any, []byte) {
	t.Helper()
	rec := f.do("GET", "/platform/projects/"+ref+"/pause/status", nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "GET /platform/projects/{ref}/pause/status", rec.Body.Bytes())
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out, rec.Body.Bytes()
}

// A paused project, and a paused branch, can be resumed from the dashboard for as long as the node keeps
// them: Studio's ProjectPausedState offers Resume when can_restore is true and treats a null
// remaining_days_till_restore_disabled as no expiry.
func TestPauseStatusOffersResume(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Not paused: nothing was paused on.
	out, _ := pauseStatus(t, f, testRef)
	if out["can_restore"] != true || out["last_paused_on"] != nil {
		t.Fatalf("running project: %v", out)
	}

	paused := time.Now().Add(-90 * time.Minute).UTC().Truncate(time.Second)
	for _, reason := range []string{"", registry.PauseIdle} {
		if err := f.reg.SetPause(ctx, testRef, reason, paused); err != nil {
			t.Fatal(err)
		}
		if err := f.reg.SetProjectStatus(ctx, testRef, registry.StatusInactive); err != nil {
			t.Fatal(err)
		}
		out, raw := pauseStatus(t, f, testRef)
		if out["can_restore"] != true || out["max_days_till_restore_disabled"] != float64(365) {
			t.Fatalf("paused (%q): %v", reason, out)
		}
		// The keys Studio reads are present, and the nulls are JSON null, not missing.
		for _, k := range []string{"remaining_days_till_restore_disabled", "latest_downloadable_backup_id", "last_paused_on"} {
			if v, ok := out[k]; !ok && k != "last_paused_on" {
				t.Fatalf("%s missing from %s", k, raw)
			} else if k != "last_paused_on" && v != nil {
				t.Fatalf("%s = %v, want null (%s)", k, v, raw)
			}
		}
		if out["last_paused_on"] != paused.Format(time.RFC3339) {
			t.Fatalf("last_paused_on = %v, want %s", out["last_paused_on"], paused.Format(time.RFC3339))
		}
	}

	// A project paused before the lifecycle store existed has no record: the date of its last change.
	if err := f.reg.ClearPause(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	out, _ = pauseStatus(t, f, testRef)
	if s, _ := out["last_paused_on"].(string); s == "" {
		t.Fatalf("no last_paused_on for a project with no record: %v", out)
	}

	if rec := f.do("GET", "/platform/projects/zzzzzzzzzzzzzzzzzzzz/pause/status", nil); rec.Code != 404 {
		t.Fatalf("unknown project: %d", rec.Code)
	}
}
