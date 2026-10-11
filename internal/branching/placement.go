package branching

import (
	"context"
	"time"
)

// Placement. A branch is homed on its parent's server (lifecycle.Engine.Create), so that the two databases
// are on one machine. What reads or writes both through SQL (merge, push, diff, the migration replay of a
// schema-only create) does not care where they are: the canonical ports of a project that runs on another
// server are forwarders to it. What copies the parent's data directory (a create or a reset with data, by
// copy-on-write clone or from the parent's base backup) works on this server's disk, and refuses when the
// parent lives elsewhere, naming the server, before anything is created or removed.

// checkParentHere refuses a clone of a parent whose data directory is on another server.
func (s *Service) checkParentHere(parentRef string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := s.reg.GetProject(ctx, parentRef)
	if err != nil || s.homedHere(p) {
		return nil // an unreadable parent is reported by the checks that need it
	}
	home := p.NodeID
	if n, err := s.reg.GetNode(ctx, p.NodeID); err == nil && n.Name != "" {
		home = n.Name
	}
	return conflict("project %s is homed on server %s and its data directory is on that server's disk, so a branch with data cannot be cloned from it here. "+
		"Create the branch without data (it replays the parent's migrations and works across servers), or move the project to this server first "+
		"(add a replica here with `supavise replicas add`, then `supavise failover %s`)", parentRef, home, parentRef)
}

// nodeOfEngine is what a service learns of its node from an Engine that knows it (*lifecycle.Engine).
type nodeOfEngine interface{ NodeID() string }
