package app

import (
	"log/slog"

	"github.com/supavise/supavise/internal/config"
)

// liveStudioAI is fleet.Deps.StudioAI for the daemon: the [studio.ai] settings as the config files
// say now. `supavise studio ai set` writes them and renders Studio itself, while the daemon, which
// read the files once at boot, keeps running; a render from the daemon's old values (the next one
// is a failover, or a change of the dashboard's SSO) would take the provider away from Studio
// again. Without a config file, or when the files no longer load, the boot values stand.
func liveStudioAI(cfg *config.Config, path string, log *slog.Logger) func() config.StudioAI {
	return func() config.StudioAI {
		if path == "" {
			return cfg.Studio.AI
		}
		c, err := config.Load(path)
		if err != nil {
			log.Warn("the configuration does not load; Studio keeps the AI settings the daemon started with", "error", err)
			return cfg.Studio.AI
		}
		return c.Studio.AI
	}
}
