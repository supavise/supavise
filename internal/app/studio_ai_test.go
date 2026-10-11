package app

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/supavise/supavise/internal/config"
)

func TestLiveStudioAIFollowsTheFiles(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "config.toml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	boot := config.Default()
	boot.Studio.AI = config.StudioAI{BaseURL: "https://boot.example.com/v1"}

	write("[studio.ai]\nbase_url = \"https://first.example.com/v1\"\n")
	live := liveStudioAI(boot, path, log)
	if got := live().BaseURL; got != "https://first.example.com/v1" {
		t.Fatalf("base_url = %q, want the file's, not the boot value", got)
	}
	write("[studio.ai]\nbase_url = \"https://second.example.com/v1\"\nmodel = \"m\"\n")
	if got := live(); got.BaseURL != "https://second.example.com/v1" || got.Model != "m" {
		t.Fatalf("a change made while the daemon runs is not seen: %+v", got)
	}
	write("[studio.ai]\nbase_url = \"ftp://nope\"\n")
	if got := live().BaseURL; got != "https://boot.example.com/v1" {
		t.Fatalf("a file that does not load must leave the boot values, got %q", got)
	}
	if got := liveStudioAI(boot, "", log)().BaseURL; got != "https://boot.example.com/v1" {
		t.Fatalf("no config file: the boot values, got %q", got)
	}
}
