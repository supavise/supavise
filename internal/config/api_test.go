package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAPIDisableOAuth(t *testing.T) {
	load := func(t *testing.T, toml string) *Config {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(p, []byte(toml), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if load(t, "domain = \"example.com\"\n").API.DisableOAuth {
		t.Error("OAuth is on by default")
	}
	if !load(t, "domain = \"example.com\"\n[api]\ndisable_oauth = true\n").API.DisableOAuth {
		t.Error("[api] disable_oauth is not read")
	}
	t.Setenv("SUPAVISE_API_DISABLE_OAUTH", "true")
	if !load(t, "domain = \"example.com\"\n").API.DisableOAuth {
		t.Error("SUPAVISE_API_DISABLE_OAUTH is not read")
	}
}

func TestAPIAuditRetention(t *testing.T) {
	load := func(t *testing.T, toml string) *Config {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(p, []byte(toml), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	day := 24 * time.Hour
	if got := load(t, "domain = \"example.com\"\n").API.AuditRetention(); got != 90*day {
		t.Errorf("default retention %v, want 90 days", got)
	}
	if got := load(t, "domain = \"example.com\"\n[api]\naudit_retention_days = 30\n").API.AuditRetention(); got != 30*day {
		t.Errorf("[api] audit_retention_days is not read: %v", got)
	}
	t.Setenv("SUPAVISE_API_AUDIT_RETENTION_DAYS", "14")
	if got := load(t, "domain = \"example.com\"\n").API.AuditRetention(); got != 14*day {
		t.Errorf("SUPAVISE_API_AUDIT_RETENTION_DAYS is not read: %v", got)
	}
	if got := (API{AuditRetentionDays: -5}).AuditRetention(); got != 90*day {
		t.Errorf("a negative value is %v, want the default", got)
	}
}
