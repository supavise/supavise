package branching

import (
	"context"
	"time"

	"github.com/supavise/supavise/internal/registry"
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
	return conflict("project %s is homed on server %s and its data directory is on that server's disk, so a branch with data cannot be cloned from it here. "+
		"Create the branch without data (it replays the parent's migrations and works across servers), or move the project to this server first "+
		"(add a replica here with `supavise replicas add`, then `supavise projects failover %s`)", parentRef, s.serverName(ctx, p.NodeID), parentRef)
}

// checkBranchHere refuses a reset with data of a branch whose own cluster is on another server than the
// parent's data. The reset removes the old cluster and clones the parent's data directory onto this
// server's disk, which cannot be handed to the other one: a branch is homed where its parent was when it
// was created, and a failover of the parent or of the leader leaves it behind. Checked before the old
// cluster is removed, so that the branch keeps its data.
func (s *Service) checkBranchHere(ctx context.Context, branch *registry.Project, parentRef string) error {
	if s.homedHere(branch) {
		return nil
	}
	return conflict("branch %s is homed on server %s, but the data of its parent %s is on this server's disk and cannot be copied to another server, so it cannot be reset with data. "+
		"Delete the branch and create it again (a new branch is homed with its parent), or reset it without data", branch.Ref, s.serverName(ctx, branch.NodeID), parentRef)
}

// serverName is the name of server id as a message shows it: the id when the registry has no name for it.
func (s *Service) serverName(ctx context.Context, id string) string {
	if n, err := s.reg.GetNode(ctx, id); err == nil && n.Name != "" {
		return n.Name
	}
	return id
}

// nodeOfEngine is what a service learns of its node from an Engine that knows it (*lifecycle.Engine).
type nodeOfEngine interface{ NodeID() string }
