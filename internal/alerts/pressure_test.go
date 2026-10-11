package alerts

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/notice"
)

// pressureRig is a rig whose checker takes its resource conditions from a list the test sets.
func pressureRig(t *testing.T) (*rig, *[]Pressure) {
	t.Helper()
	r := newRig(t)
	var conds []Pressure
	r.checker.Pressure = func(context.Context) []Pressure { return conds }
	return r, &conds
}

var memoryWarning = Pressure{Key: KindMemoryLow + "/", Kind: KindMemoryLow, Severity: SeverityWarning, Title: "Node memory is low", Detail: "6% of the node's memory is available."}

// A resource condition is held for the debounce like any other, sent once, and told as over when
// the source stops reporting it.
func TestPressureIsDebouncedSentOnceAndResolved(t *testing.T) {
	r, conds := pressureRig(t)
	*conds = []Pressure{memoryWarning}
	for i := 0; i < 3; i++ {
		r.cycle()
		if r.sink.count() != 0 {
			t.Fatalf("raised after %d cycle(s), before the debounce", i+1)
		}
	}
	r.cycle()
	got := r.sent()
	if len(got) != 1 || got[0].Kind != KindMemoryLow || got[0].Severity != SeverityWarning || got[0].Ref != "" || !strings.Contains(got[0].Detail, "6%") {
		t.Fatalf("sent %+v", got)
	}
	for i := 0; i < 20; i++ {
		r.cycle()
	}
	if r.sink.count() != 1 {
		t.Errorf("%d alerts for one standing condition", r.sink.count())
	}
	*conds = nil
	r.cycle()
	got = r.sent()
	if len(got) != 2 || !got[1].Resolved || got[1].Kind != KindMemoryLow || got[1].Title != memoryWarning.Title {
		t.Fatalf("recovery: %+v", got)
	}
}

// A blip shorter than the debounce sends nothing.
func TestPressureThatClearsBeforeTheDebounceSendsNothing(t *testing.T) {
	r, conds := pressureRig(t)
	*conds = []Pressure{memoryWarning}
	r.cycle()
	r.cycle()
	*conds = nil
	for i := 0; i < 6; i++ {
		r.cycle()
	}
	if n := r.sink.count(); n != 0 {
		t.Fatalf("%d alerts", n)
	}
}

// The critical state of a matter has a key of its own: it is told when it comes, although the
// warning of the same matter was sent minutes ago, and each ends by itself.
func TestPressureEscalationIsToldAndEachLevelEndsOnItsOwn(t *testing.T) {
	r, conds := pressureRig(t)
	disk := func(sev, suffix string) Pressure {
		return Pressure{Key: KindProjectDiskHigh + "/" + refA + suffix, Kind: KindProjectDiskHigh, Severity: sev, Ref: refA,
			Title: "Disk of project alpha (" + refA + ") is over 80% full"}
	}
	*conds = []Pressure{disk(SeverityWarning, "")}
	for i := 0; i < 4; i++ {
		r.cycle()
	}
	if got := r.sent(); len(got) != 1 || got[0].Severity != SeverityWarning || got[0].Ref != refA {
		t.Fatalf("the warning: %+v", got)
	}
	*conds = []Pressure{disk(SeverityWarning, ""), disk(SeverityCritical, "/critical")}
	for i := 0; i < 4; i++ {
		r.cycle()
	}
	got := r.sent()
	if len(got) != 2 || got[1].Severity != SeverityCritical {
		t.Fatalf("the critical one was not told: %+v", got)
	}
	*conds = []Pressure{disk(SeverityWarning, "")} // the critical level ends first
	r.cycle()
	got = r.sent()
	if len(got) != 3 || !got[2].Resolved || got[2].Severity != SeverityCritical {
		t.Fatalf("the critical recovery: %+v", got)
	}
	*conds = nil
	r.cycle()
	if got = r.sent(); len(got) != 4 || !got[3].Resolved || got[3].Severity != SeverityWarning {
		t.Fatalf("the warning recovery: %+v", got)
	}
}

// A source that has not measured a key yet (the daemon just started, the project's Postgres is not
// up) does not end the alert that is open for it, and does not send the recovery. When it has
// measured and the condition is gone, the alert ends as usual.
func TestPressureAlertSurvivesWhatIsNotMeasuredYet(t *testing.T) {
	r, conds := pressureRig(t)
	unjudged := map[string]bool{}
	r.checker.Unjudged = func(key string) bool { return unjudged[key] }
	d := Pressure{Key: KindProjectDiskHigh + "/" + refA, Kind: KindProjectDiskHigh, Severity: SeverityWarning, Ref: refA,
		Title: "Disk of project alpha (" + refA + ") is over 80% full"}
	*conds = []Pressure{d}
	for i := 0; i < 4; i++ {
		r.cycle()
	}
	if got := r.sent(); len(got) != 1 {
		t.Fatalf("the alert: %+v", got)
	}
	*conds = nil
	unjudged[d.Key] = true
	for i := 0; i < 5; i++ {
		r.cycle()
	}
	if got := r.sent(); len(got) != 1 {
		t.Fatalf("an unmeasured condition ended the alert: %+v", got)
	}
	// Measured at last, and still wrong: no new alert, since the old one is still open.
	*conds = []Pressure{d}
	delete(unjudged, d.Key)
	for i := 0; i < 5; i++ {
		r.cycle()
	}
	if got := r.sent(); len(got) != 1 {
		t.Fatalf("a second alert for one standing condition: %+v", got)
	}
	*conds = nil
	r.cycle()
	if got := r.sent(); len(got) != 2 || !got[1].Resolved {
		t.Fatalf("the recovery: %+v", got)
	}
}

// An upgrade or a maintenance window restarts projects, not the node's memory or a project's
// quota: these are still raised.
func TestPressureIsRaisedDuringAnUpgrade(t *testing.T) {
	r, conds := pressureRig(t)
	*conds = []Pressure{memoryWarning}
	b, _ := json.Marshal(notice.Upgrade{Phase: "backup", From: "v1", To: "v2", StartedAt: r.clk.now()})
	if err := os.MkdirAll(filepath.Join(r.cfg.StateDir, "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.cfg.StateDir, "system", "upgrade.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		r.cycle()
	}
	if got := r.sent(); len(got) != 1 || got[0].Kind != KindMemoryLow {
		t.Fatalf("sent %+v", got)
	}
}

// A checker with no source of resource conditions behaves as it did.
func TestNoPressureSourceRaisesNothing(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 6; i++ {
		r.cycle()
	}
	if r.sink.count() != 0 {
		t.Fatalf("%d alerts", r.sink.count())
	}
}
