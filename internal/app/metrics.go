package app

import (
	"context"
	"log/slog"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/diskquota"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/metrics"
	"github.com/supavise/supavise/internal/registry"
)

// newMetrics builds the node's utilization collector (internal/metrics): once a minute it reads the
// control groups of the projects homed on this node, one SQL session of each and each one's disk quota,
// and keeps the samples on disk. The Management API's infra-monitoring and resource-warning routes
// answer from it, and the alert checker takes its resource conditions from it (metricsPressure).
// Control groups exist only under systemd, so a node with the exec supervisor (development and tests)
// gets none, and the routes answer with series that have no points.
//
// self returns the id of this node; a project is sampled here when its home is this node.
func newMetrics(cfg *config.Config, node *lifecycle.Node, log *slog.Logger, self func() string) *metrics.Service {
	if cfg.Supervisor != config.SupervisorSystemd {
		return nil
	}
	return metrics.New(metrics.Options{
		Cfg: cfg, Projects: node.Registry, Log: log,
		Local: func(p *registry.Project) bool { return p.NodeID == self() },
		Disk:  &metrics.NodeDisk{Cfg: cfg, Limits: diskquota.New(cfg, node.Supervisor)},
	})
}

// selfOf is the id of the node the daemon runs on: the membership the mesh hook provided, the
// founder of a server that never joined a cluster before one exists.
func selfOf(w *Wire) string {
	if w != nil {
		if m, ok := Get[cluster.Membership](w); ok {
			return m.Self().ID
		}
	}
	return registry.FounderNodeID
}

// metricsPressure adapts the collector's conditions to the alert checker's.
func metricsPressure(svc *metrics.Service) func(context.Context) []alerts.Pressure {
	return func(context.Context) []alerts.Pressure {
		cs := svc.Pressure()
		out := make([]alerts.Pressure, len(cs))
		for i, c := range cs {
			out[i] = alerts.Pressure{Key: c.Key, Kind: c.Kind, Severity: c.Severity, Ref: c.Ref, Title: c.Title, Detail: c.Detail}
		}
		return out
	}
}
