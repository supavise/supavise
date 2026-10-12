package lifecycle

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// PauseOptions tune Engine.PauseWith.
type PauseOptions struct {
	// Reason is why the project is paused: "" for the owner's pause (the dashboard, the API, the
	// CLI), registry.PauseIdle for one the node makes because a branch is not used. Only an idle
	// pause wakes by itself, on the first request or database connection.
	Reason string
}

// recordPause writes the reason of a pause before the status changes (registry.LifecycleStore). A
// pause the node makes for idleness is refused when the reason cannot be recorded: without it the
// project would sit paused and never wake. An owner's pause goes ahead, because the record only
// feeds the dashboard's last-paused date.
func (e *Engine) recordPause(ctx context.Context, ref string, o PauseOptions) error {
	st := registry.BranchLifecycleOf(e.reg)
	if st == nil {
		if o.Reason != "" {
			return fmt.Errorf("lifecycle: pause %s: the registry cannot record why a project is paused", ref)
		}
		return nil
	}
	if err := st.SetPause(ctx, ref, o.Reason, e.opts.Now().UTC()); err != nil {
		if o.Reason != "" {
			return fmt.Errorf("lifecycle: pause %s: %w", ref, err)
		}
		e.log.Warn("pause: could not record the pause", "ref", ref, "error", err)
	}
	return nil
}

// forgetPause removes the record of a pause; a failure is logged and goes no further, because a
// stale record only matters while the project is INACTIVE and a pause writes a new one.
func (e *Engine) forgetPause(ctx context.Context, ref string) {
	st := registry.BranchLifecycleOf(e.reg)
	if st == nil {
		return
	}
	if err := st.ClearPause(ctx, ref); err != nil {
		e.log.Warn("could not clear the record of a pause", "ref", ref, "error", err)
	}
}

// homeOfParent is the node a branch of parent is homed on: the parent's. "" lets the registry choose
// (the cluster leader), which is also the answer when the parent cannot be read.
func (e *Engine) homeOfParent(ctx context.Context, parent string) string {
	p, err := e.reg.GetProject(ctx, parent)
	if err != nil {
		return ""
	}
	return p.NodeID
}

// ObjectRemover removes the Storage objects a project keeps in the bucket of the S3 backend (the
// <ref>/ prefix). The daemon sets one when [fleet] storage_backend is "s3" (SetObjectRemover); the
// Engine has none otherwise.
type ObjectRemover interface {
	RemoveObjects(ctx context.Context, ref string) error
}

// SetObjectRemover sets the remover of the bucket's objects. internal/app calls it before the Engine
// serves a request; it is not safe to call while operations run.
func (e *Engine) SetObjectRemover(r ObjectRemover) { e.objects = r }

// purgeStorageObjects removes ref's Storage objects from the file backend and from the bucket.
func (e *Engine) purgeStorageObjects(ctx context.Context, ref string) {
	if err := e.removeStorageObjects(ref); err != nil {
		e.log.Warn("could not remove the Storage objects of the file backend", "ref", ref, "error", err)
	}
	e.purgeBucketObjects(ctx, ref)
}

// purgeBucketObjects removes ref's objects from the S3 bucket. A failure is recorded and does not
// stop the delete: orphaned objects cost storage, while a delete that cannot finish keeps a project
// that nobody wants.
func (e *Engine) purgeBucketObjects(ctx context.Context, ref string) {
	if e.objects == nil || e.cfg.Fleet.StorageBackend != "s3" {
		return
	}
	if err := e.objects.RemoveObjects(ctx, ref); err != nil {
		e.log.Warn("could not remove the Storage objects of the project from the bucket; they stay under its prefix", "ref", ref, "error", err)
		e.event(ctx, ref, "project.storage_cleanup_failed", map[string]string{"error": err.Error()})
	}
}

// portWait is how long awaitDatabasePort waits for a listener on a stopped project's database port to go.
const portWait = 3 * time.Second

// awaitDatabasePort waits until nothing listens on the database port of p, which is about to be
// started. A branch paused for idleness has a listener there, the wake gate of the branching service
// (internal/branching, "The wake gate"), and the gate closes the moment the status leaves INACTIVE; the
// Engine has already set it, so this only waits out the moments between the change and the close,
// which may be another process's. A port nothing listens on (every other project) costs one refused
// connection. After portWait it goes on and lets the start report what holds the port.
func (e *Engine) awaitDatabasePort(ctx context.Context, p *registry.Project) {
	if !e.homedHere(p) {
		// The gate and any leftover listener are on the project's home. Here the port is the mesh forwarder
		// to it, which listens whatever the status, so waiting for it to go would only run out the clock.
		return
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(e.cfg.PortsFor(p.Ref, p.Seq).Postgres))
	deadline := time.Now().Add(portWait)
	for {
		c, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err != nil {
			return // nothing listens
		}
		_ = c.Close()
		if ctx.Err() != nil || time.Now().After(deadline) {
			e.log.Warn("something still listens on the database port of a project that is about to start", "ref", p.Ref, "addr", addr)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}
