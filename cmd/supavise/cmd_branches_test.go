package main

import (
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/registry"
)

func TestBranchesCommandTree(t *testing.T) {
	var branches []string
	for _, c := range rootCmd.Commands() {
		if c.Name() == "branches" {
			for _, s := range c.Commands() {
				branches = append(branches, s.Name())
			}
		}
	}
	for _, want := range []string{"list", "create", "get", "delete", "merge", "reset", "push", "sweep", "diff", "restore", "seed", "runs", "logs", "notify-secret"} {
		found := false
		for _, b := range branches {
			found = found || b == want
		}
		if !found {
			t.Errorf("supavise branches %s is missing (have %v)", want, branches)
		}
	}
}

func TestPrintBranch(t *testing.T) {
	var sb strings.Builder
	b := &branching.Branch{ID: "i", Name: "feat", Ref: "r", ParentRef: "p", State: registry.BranchMigrationsPassed, ProjectStatus: registry.StatusActiveHealthy, CloneMethod: "clonefile", Detail: "ok"}
	printBranch(&sb, b)
	for _, want := range []string{"name:     feat", "status:   MIGRATIONS_PASSED (project ACTIVE_HEALTHY)", "data:     clonefile", "detail:   ok"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("missing %q in\n%s", want, sb.String())
		}
	}
}

func TestPrintBranchShowsWhyItFailed(t *testing.T) {
	var sb strings.Builder
	exp := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	b := &branching.Branch{ID: "i", Name: "feat", Ref: "r", ParentRef: "p", State: registry.BranchMigrationsFailed, ProjectStatus: registry.StatusActiveHealthy,
		Detail: "apply to the parent: relation f already exists", ExpiresAt: &exp}
	printBranch(&sb, b)
	for _, want := range []string{"status:   MIGRATIONS_FAILED", "failed:   apply to the parent: relation f already exists", "expires:  "} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("missing %q in\n%s", want, sb.String())
		}
	}
}

func TestViewRun(t *testing.T) {
	step := func(n, s string) registry.BranchRunStep { return registry.BranchRunStep{Name: n, Status: s} }
	ok := &registry.BranchRun{ID: "a", Op: "push", Steps: []registry.BranchRunStep{step("pull", "EXITED"), step("seed", "PAUSED")}}
	if v := viewRun(ok); v.Status != "SUCCESS" || v.Steps != "all done" {
		t.Errorf("success: %+v", v)
	}
	bad := &registry.BranchRun{ID: "b", Op: "merge", Failure: "migrate: diverged",
		Steps: []registry.BranchRunStep{step("pull", "EXITED"), step("migrate", "DEAD"), step("deploy", "PAUSED")}}
	if v := viewRun(bad); v.Status != "FAILED" || v.Steps != "migrate=DEAD" || v.Failure != "migrate: diverged" {
		t.Errorf("failure: %+v", v)
	}
	if v := viewRun(&registry.BranchRun{ID: "c"}); v.Status != "RUNNING" {
		t.Errorf("a run with no steps is running: %+v", v)
	}
}
