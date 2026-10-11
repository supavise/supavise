package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// With the exec backend (development and tests) the command prints the end of each unit's log file
// and keeps the shared services' lines to the project's.
func TestLogsCommandWithTheExecBackend(t *testing.T) {
	const ref = "abcdefghijklmnopqrst"
	dir := t.TempDir()
	logs := filepath.Join(dir, "state", "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(unit, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(logs, unit+".log"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("supavise-postgres@"+ref+".service", "LOG: ready\nERROR: boom\n")
	write("supavise-realtime.service", "tenant="+ref+" joined\ntenant=zzzzzzzzzzzzzzzzzzzz joined\n")
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("state_dir = \""+filepath.Join(dir, "state")+"\"\nsupervisor = \"exec\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { configPath = "" })

	out, err := runRoot(t, "--config", cfg, "logs", ref, "--lines", "100", "--service", "", "--since", "")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"postgres  LOG: ready", "postgres  ERROR: boom", "realtime  tenant=" + ref + " joined"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "zzzzzzzz") {
		t.Errorf("another project's line is shown:\n%s", out)
	}

	out, err = runRoot(t, "--config", cfg, "logs", ref, "--service", "postgres", "--lines", "1", "--since", "")
	if err != nil || strings.Contains(out, "LOG: ready") || !strings.Contains(out, "ERROR: boom") {
		t.Errorf("--service postgres -n 1 = %q, %v, want only the last line", out, err)
	}
	if _, err := runRoot(t, "--config", cfg, "logs", "short", "--service", "", "--lines", "100"); err == nil || !strings.Contains(err.Error(), "not a project ref") {
		t.Errorf("a bad ref: %v", err)
	}
	if _, err := runRoot(t, "--config", cfg, "logs", ref, "--service", "kafka", "--lines", "100"); err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Errorf("a bad service: %v", err)
	}
	if _, err := runRoot(t, "--config", cfg, "logs"); err == nil {
		t.Error("logs without a ref was accepted")
	}
}
