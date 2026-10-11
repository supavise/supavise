package svclog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const ref = "abcdefghijklmnopqrst"

func TestResolve(t *testing.T) {
	all, err := Resolve(ref, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range all {
		got = append(got, fmt.Sprintf("%s=%s shared=%v", s.Name, s.Unit, s.Shared))
	}
	want := []string{
		"postgres=supavise-postgres@" + ref + ".service shared=false",
		"auth=supavise-gotrue@" + ref + ".service shared=false",
		"rest=supavise-postgrest@" + ref + ".service shared=false",
		"realtime=supavise-realtime.service shared=true",
		"storage=supavise-storage.service shared=true",
		"pooler=supavise-supavisor.service shared=true",
		"functions=supavise-edge-runtime.service shared=true",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("sources:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for alias, unit := range map[string]string{"db": "supavise-postgres@" + ref + ".service", "GoTrue": "supavise-gotrue@" + ref + ".service",
		"postgrest": "supavise-postgrest@" + ref + ".service", "supavisor": "supavise-supavisor.service", "edge-functions": "supavise-edge-runtime.service"} {
		one, err := Resolve(ref, alias)
		if err != nil || len(one) != 1 || one[0].Unit != unit {
			t.Errorf("--service %s = %v, %v, want %s", alias, one, err, unit)
		}
	}
	sys, err := Resolve("system", "")
	if err != nil || len(sys) != 2 || sys[0].Unit != "supavise-postgres@system.service" || sys[1].Unit != "supavise-gotrue@system.service" {
		t.Errorf("system = %v, %v, want postgres and auth", sys, err)
	}
	for _, c := range []struct{ ref, svc, want string }{
		{"short", "", "not a project ref"},
		{ref + "x", "", "not a project ref"},
		{"ABCDEFGHIJKLMNOPQRST", "", "not a project ref"},
		{ref, "kafka", "unknown service"},
		{"system", "rest", "system project has no rest log"},
	} {
		if _, err := Resolve(c.ref, c.svc); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Resolve(%q, %q) error = %v, want %q", c.ref, c.svc, err, c.want)
		}
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]string{
		"":                     "",
		"90s":                  "2026-10-10 11:58:30 UTC",
		"30m":                  "2026-10-10 11:30:00 UTC",
		"2h":                   "2026-10-10 10:00:00 UTC",
		"1d":                   "2026-10-09 12:00:00 UTC",
		"1w":                   "2026-10-03 12:00:00 UTC",
		"2026-10-10T08:15:00Z": "2026-10-10 08:15:00 UTC",
		"2026-10-10 08:15:00":  "2026-10-10 08:15:00 UTC",
		"2026-10-09":           "2026-10-09 00:00:00 UTC",
	} {
		got, err := ParseSince(in, now)
		if err != nil || got != want {
			t.Errorf("ParseSince(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"yesterday", "1y", "-5m", "5", "1 hour ago"} {
		if _, err := ParseSince(bad, now); err == nil {
			t.Errorf("ParseSince(%q) accepted", bad)
		}
	}
}

// fakeJournal answers each journalctl call from lines keyed by the unit in its arguments, and
// records the arguments.
type fakeJournal struct {
	mu    sync.Mutex
	calls [][]string
	// history[unit] is what a non-follow call prints; live[unit] is a pipe a follow call reads.
	history map[string][]map[string]any
	live    map[string]*io.PipeWriter
	liveR   map[string]*io.PipeReader
	failOn  string // a unit whose call fails on Close
}

func newFakeJournal() *fakeJournal {
	return &fakeJournal{history: map[string][]map[string]any{}, live: map[string]*io.PipeWriter{}, liveR: map[string]*io.PipeReader{}}
}

func unitOf(args []string) string {
	for i, a := range args {
		if a == "-u" {
			return args[i+1]
		}
	}
	return ""
}

type fakeProc struct {
	io.Reader
	err error
	cl  func()
}

func (p fakeProc) Close() error {
	if p.cl != nil {
		p.cl()
	}
	return p.err
}

func (f *fakeJournal) Start(ctx context.Context, args []string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	unit := unitOf(args)
	var err error
	if unit == f.failOn {
		err = fmt.Errorf("journalctl: boom")
	}
	for _, a := range args {
		if a == "-f" {
			pr, pw := io.Pipe()
			f.live[unit], f.liveR[unit] = pw, pr
			go func() { <-ctx.Done(); pw.Close() }()
			return fakeProc{Reader: pr, err: err}, nil
		}
	}
	var b bytes.Buffer
	for _, e := range f.history[unit] {
		_ = json.NewEncoder(&b).Encode(e)
	}
	return fakeProc{Reader: &b, err: err}, nil
}

func entry1(ts time.Time, cursor string, msg any) map[string]any {
	return map[string]any{"__REALTIME_TIMESTAMP": fmt.Sprint(ts.UnixMicro()), "__CURSOR": cursor, "MESSAGE": msg}
}

var base = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func TestRunMergesByTimeAndKeepsTheProjectsLines(t *testing.T) {
	j := newFakeJournal()
	pg := "supavise-postgres@" + ref + ".service"
	j.history[pg] = []map[string]any{
		entry1(base.Add(1*time.Second), "p1", "LOG:  checkpoint starting"),
		entry1(base.Add(5*time.Second), "p2", "ERROR:  boom\x1b[2J"),
	}
	j.history["supavise-gotrue@"+ref+".service"] = []map[string]any{entry1(base.Add(3*time.Second), "a1", "login ok")}
	j.history["supavise-realtime.service"] = []map[string]any{
		entry1(base.Add(2*time.Second), "r1", "tenant="+ref+" joined"),
		entry1(base.Add(4*time.Second), "r2", "tenant=zzzzzzzzzzzzzzzzzzzz joined"), // another project's, though --grep should have kept it out
	}
	j.history["supavise-storage.service"] = []map[string]any{entry1(base.Add(6*time.Second), "s1", []int{0x68, 0x69, 0xff, 0x20}), entry1(base.Add(7*time.Second), "s2", "x "+ref)}
	var out, errb bytes.Buffer
	err := Run(context.Background(), Options{Ref: ref, Lines: 5, Out: &out, Err: &errb, Journal: j, Now: func() time.Time { return base }, Root: true})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"2026-10-10T12:00:03.000Z auth      login ok",
		"2026-10-10T12:00:05.000Z postgres  ERROR:  boom\\x1b[2J",
		"2026-10-10T12:00:07.000Z storage   x " + ref,
	}, "\n")
	// Five lines are the project's: two of postgres, auth, the realtime line with the ref and the storage
	// line with the ref. The byte-array message (invalid UTF-8) names no ref, so it is dropped.
	got := strings.TrimSpace(out.String())
	for _, line := range strings.Split(want, "\n") {
		if !strings.Contains(got, line) {
			t.Errorf("output lacks %q:\n%s", line, got)
		}
	}
	if strings.Contains(got, "zzzzzzzz") {
		t.Errorf("another project's line is shown:\n%s", got)
	}
	lines := strings.Split(got, "\n")
	if len(lines) != 5 {
		t.Errorf("%d lines, want 5:\n%s", len(lines), got)
	}
	for i := 1; i < len(lines); i++ {
		if lines[i-1][:24] > lines[i][:24] {
			t.Errorf("lines are not in time order:\n%s", got)
		}
	}
	// Arguments: every call has -u, json output and -n 5; --grep with the ref only for the shared units.
	for _, c := range j.calls {
		a := strings.Join(c, " ")
		shared := !strings.Contains(c[1], "@")
		if !strings.Contains(a, "-o json") || !strings.Contains(a, "-n 5") || !strings.Contains(a, "--no-pager") {
			t.Errorf("call %q lacks -o json, -n 5 or --no-pager", a)
		}
		if strings.Contains(a, "--grep "+ref) != shared {
			t.Errorf("call %q: --grep present = %v, want %v", a, strings.Contains(a, "--grep"), shared)
		}
		if strings.Contains(a, " -f") || strings.Contains(a, "--since") {
			t.Errorf("call %q follows or limits by time", a)
		}
	}
	if len(j.calls) != 7 {
		t.Errorf("journalctl calls = %d, want 7", len(j.calls))
	}
	if errb.Len() != 0 {
		t.Errorf("stderr: %s", errb.String())
	}
}

func TestRunLimitsTheLinesAndPassesSince(t *testing.T) {
	j := newFakeJournal()
	pg := "supavise-postgres@" + ref + ".service"
	for i := 0; i < 10; i++ {
		j.history[pg] = append(j.history[pg], entry1(base.Add(time.Duration(i)*time.Second), fmt.Sprint("c", i), fmt.Sprint("line ", i)))
	}
	var out bytes.Buffer
	err := Run(context.Background(), Options{Ref: ref, Service: "postgres", Lines: 3, Since: "2h", Out: &out, Err: io.Discard, Journal: j, Now: func() time.Time { return base }})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), "\n"); got != 3 || !strings.Contains(out.String(), "line 9") || strings.Contains(out.String(), "line 6") {
		t.Errorf("output:\n%s", out.String())
	}
	if len(j.calls) != 1 || !strings.Contains(strings.Join(j.calls[0], " "), "--since 2026-10-10 10:00:00 UTC") {
		t.Errorf("calls = %v, want one with --since 2026-10-10 10:00:00 UTC", j.calls)
	}
}

func TestRunSaysWhenThereAreNoLines(t *testing.T) {
	var out, errb bytes.Buffer
	if err := Run(context.Background(), Options{Ref: ref, Service: "auth", Lines: 10, Since: "5m", Out: &out, Err: &errb, Journal: newFakeJournal(), Now: func() time.Time { return base }}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"no log lines for " + ref + " since 5m", "supavise projects list", "run the command with sudo"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr %q lacks %q", errb.String(), want)
		}
	}
	if out.Len() != 0 {
		t.Errorf("stdout: %s", out.String())
	}
}

func TestRunReportsAJournalctlFailure(t *testing.T) {
	j := newFakeJournal()
	j.failOn = "supavise-gotrue@" + ref + ".service"
	err := Run(context.Background(), Options{Ref: ref, Lines: 10, Out: io.Discard, Err: io.Discard, Journal: j})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want journalctl's", err)
	}
	if err := Run(context.Background(), Options{Ref: ref, Lines: -1, Out: io.Discard, Journal: j}); err == nil {
		t.Error("a negative -n was accepted")
	}
	if err := Run(context.Background(), Options{Ref: ref, Since: "nonsense", Lines: 1, Out: io.Discard, Journal: j}); err == nil {
		t.Error("a bad --since was accepted")
	}
}

// With --follow the history is printed and then the new lines; a line that is in both is printed once.
func TestRunFollowsAndPrintsALineOnce(t *testing.T) {
	j := newFakeJournal()
	pg := "supavise-postgres@" + ref + ".service"
	j.history[pg] = []map[string]any{entry1(base, "c1", "old line")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Ref: ref, Service: "postgres", Lines: 5, Follow: true, Out: pw, Err: io.Discard, Journal: j})
	}()
	read := func() string {
		buf := make([]byte, 4096)
		n, _ := pr.Read(buf)
		return string(buf[:n])
	}
	if got := read(); !strings.Contains(got, "old line") {
		t.Fatalf("history not printed first: %q", got)
	}
	// The follower is started before the history is read, so the pipe exists by now.
	deadline := time.Now().Add(2 * time.Second)
	var w *io.PipeWriter
	for time.Now().Before(deadline) {
		j.mu.Lock()
		w = j.live[pg]
		j.mu.Unlock()
		if w != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if w == nil {
		t.Fatal("no follow call")
	}
	// c1 was in the history: skipped. c2 is new.
	go func() {
		_ = json.NewEncoder(w).Encode(entry1(base, "c1", "old line"))
		_ = json.NewEncoder(w).Encode(entry1(base.Add(time.Second), "c2", "new line"))
	}()
	got := read()
	if strings.Contains(got, "old line") || !strings.Contains(got, "new line") {
		t.Errorf("followed output = %q, want only the new line", got)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after cancel = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop when its context ended")
	}
	var follow []string
	for _, c := range j.calls {
		if strings.Contains(strings.Join(c, " "), " -f") {
			follow = c
		}
	}
	if !strings.Contains(strings.Join(follow, " "), "-n 0") {
		t.Errorf("follow call = %v, want -n 0 so that it starts at the end", follow)
	}
}

func TestRunTail(t *testing.T) {
	logs := map[string]string{
		"supavise-postgres@" + ref + ".service": "one\ntwo\n",
		"supavise-realtime.service":             "tenant=" + ref + " hi\nother tenant\n",
		"supavise-gotrue@" + ref + ".service":   "",
	}
	var out, errb bytes.Buffer
	err := Run(context.Background(), Options{Ref: ref, Lines: 10, Follow: true, Out: &out, Err: &errb,
		Tail: func(unit string, n int) string { return logs[unit] }})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"postgres  one", "postgres  two", "realtime  tenant=" + ref + " hi"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "other tenant") {
		t.Error("another tenant's line is shown")
	}
	if !strings.Contains(errb.String(), "--follow needs the systemd backend") {
		t.Errorf("stderr = %q", errb.String())
	}
	if err := Run(context.Background(), Options{Ref: ref, Lines: 10, Since: "1h", Out: &out, Err: &errb, Tail: func(string, int) string { return "" }}); err == nil {
		t.Error("--since was accepted without a journal")
	}
}

// ExecJournal against a stand-in journalctl: a failure carries its message, a program that is still
// running when the reader is done is stopped without an error.
func TestExecJournal(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ctx := context.Background()

	rc, err := ExecJournal{Program: script("fail", `echo '{"MESSAGE":"hi"}'; echo "Failed to open journal" >&2; exit 1`)}.Start(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	if !strings.Contains(string(b), "hi") {
		t.Errorf("output = %q", b)
	}
	if err := rc.Close(); err == nil || !strings.Contains(err.Error(), "Failed to open journal") {
		t.Errorf("Close = %v, want the program's message", err)
	}

	rc, err = ExecJournal{Program: script("ok", `echo '{"MESSAGE":"hi"}'`)}.Start(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(rc)
	if err := rc.Close(); err != nil {
		t.Errorf("Close after a clean exit = %v", err)
	}

	rc, err = ExecJournal{Program: script("forever", `echo '{"MESSAGE":"hi"}'; exec sleep 30`)}.Start(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if _, err := rc.Read(buf); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := rc.Close(); err != nil {
		t.Errorf("Close of a stopped program = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("Close waited for the program")
	}

	if _, err := (ExecJournal{Program: filepath.Join(dir, "missing")}).Start(ctx, nil); err == nil || !strings.Contains(err.Error(), "systemd") {
		t.Errorf("missing program error = %v, want a hint about systemd", err)
	}
}
