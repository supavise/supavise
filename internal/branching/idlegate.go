package branching

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// The wake gate. A branch paused for idleness has no Postgres, but the pooler (Supavisor) and every
// other client still dial its port, 127.0.0.1:<20000+3n>. While the branch sleeps this service listens
// there. The first client that sends a startup packet wakes the branch: the gate stops listening so
// that Postgres can bind the port, starts the branch, waits for Postgres, opens a connection to it,
// replays the packet it read and passes the bytes both ways. The client sees a slow connect, not a
// refused one.
//
// What it does not cover: a client that connects after the gate stopped listening and before Postgres
// listens (the first seconds of the start) is refused, and so is one whose own connect timeout is
// shorter than the start. Both reconnect. A connection that sends nothing (a port scan, a TCP health
// check) wakes nothing.

// gateReadTimeout is how long the gate waits for a client's first packet.
const gateReadTimeout = 15 * time.Second

// gate is the listener of one sleeping branch.
type gate struct {
	ref  string
	port int
	ln   net.Listener
	once sync.Once
}

func (g *gate) close() { g.once.Do(func() { _ = g.ln.Close() }) }

// kickGates asks runGates to look at the registry now.
func (s *Service) kickGates() {
	select {
	case s.idle.kick <- struct{}{}:
	default:
	}
}

// listen binds a loopback port; tests replace it.
func listenLoopback(port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// runGates keeps one gate per branch that sleeps on this node until ctx ends. The registry's change
// feed and a slow tick tell it when to look; each look binds the gates that are missing and closes the
// ones whose branch is not asleep any more.
func (s *Service) runGates(ctx context.Context) {
	changes, err := s.reg.Subscribe(ctx)
	if err != nil {
		changes = nil
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	defer s.closeGates()
	for {
		s.reconcileGates(ctx)
		var debounce <-chan time.Time
	wait:
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-changes:
				if !ok {
					changes = nil
					continue
				}
				if debounce == nil {
					debounce = time.After(100 * time.Millisecond)
				}
			case <-s.idle.kick:
				break wait
			case <-debounce:
				break wait
			case <-t.C:
				break wait
			}
		}
	}
}

// reconcileGates opens a gate for every branch of this node that sleeps for idleness and closes the
// others. Nothing listens when the cluster has more than one server (IdleSweep wakes the sleepers).
func (s *Service) reconcileGates(ctx context.Context) {
	want := map[string]int{}
	if !s.multiServer(ctx) {
		if all, err := s.reg.ListProjects(ctx); err == nil {
			idle := map[string]bool{}
			if st := registry.BranchLifecycleOf(s.reg); st != nil {
				if ps, err := st.ListPauses(ctx); err == nil {
					for _, p := range ps {
						if p.Reason == registry.PauseIdle {
							idle[p.Ref] = true
						}
					}
				}
			}
			for i := range all {
				p := &all[i]
				if p.Branch != nil && p.Status == registry.StatusInactive && idle[p.Ref] && s.homedHere(p) {
					want[p.Ref] = s.cfg.PortsFor(p.Ref, p.Seq).Postgres
				}
			}
		}
	}
	s.idle.mu.Lock()
	var stale []*gate
	for ref, g := range s.idle.gates {
		if port, ok := want[ref]; !ok || port != g.port {
			stale = append(stale, g)
			delete(s.idle.gates, ref)
		}
	}
	s.idle.mu.Unlock()
	for _, g := range stale {
		g.close()
	}
	for ref, port := range want {
		s.idle.mu.Lock()
		_, has := s.idle.gates[ref]
		s.idle.mu.Unlock()
		if has {
			continue
		}
		ln, err := s.listen(port)
		if err != nil {
			s.log.Debug("could not listen for a sleeping branch yet", "ref", ref, "port", port, "err", err)
			continue
		}
		g := &gate{ref: ref, port: port, ln: ln}
		s.idle.mu.Lock()
		s.idle.gates[ref] = g
		s.idle.mu.Unlock()
		go s.serveGate(ctx, g)
	}
}

func (s *Service) closeGates() {
	s.idle.mu.Lock()
	gs := s.idle.gates
	s.idle.gates = map[string]*gate{}
	s.idle.mu.Unlock()
	for _, g := range gs {
		g.close()
	}
}

// serveGate accepts connections on g until it is closed.
func (s *Service) serveGate(ctx context.Context, g *gate) {
	for {
		c, err := g.ln.Accept()
		if err != nil {
			return
		}
		go s.passThrough(ctx, g, c)
	}
}

// passThrough serves one client of a sleeping branch.
func (s *Service) passThrough(ctx context.Context, g *gate, c net.Conn) {
	defer c.Close()
	// The smallest thing a Postgres client sends first is 8 bytes: an SSLRequest, or the length and
	// protocol of a startup packet.
	first := make([]byte, 8)
	_ = c.SetReadDeadline(time.Now().Add(gateReadTimeout))
	if _, err := io.ReadFull(c, first); err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	// Postgres has to bind the port: stop listening before the start (a client that comes now is refused
	// and tries again), and open the gate again later if the branch does not wake.
	g.close()
	s.idle.mu.Lock()
	if s.idle.gates[g.ref] == g {
		delete(s.idle.gates, g.ref)
	}
	s.idle.mu.Unlock()

	wctx, cancel := context.WithTimeout(ctx, s.cfg.Branching.IdleWakeTimeout())
	defer cancel()
	err := s.wake(wctx, g.ref, "db")
	s.kickGates()
	if err != nil {
		s.log.Warn("could not wake a branch for a database connection", "ref", g.ref, "err", err)
		return
	}
	up, err := s.dialUp(wctx, g.port)
	if err != nil {
		s.log.Warn("a woken branch's database did not accept a connection", "ref", g.ref, "err", err)
		return
	}
	defer up.Close()
	if _, err := up.Write(first); err != nil {
		return
	}
	splice(c, up)
}

// dialUp connects to the branch's Postgres, which listens as soon as the start returns; a few tries
// cover the moment between the unit being up and the port being bound.
func (s *Service) dialUp(ctx context.Context, port int) (net.Conn, error) {
	var d net.Dialer
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var last error
	for {
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			return c, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, last
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// splice copies both ways until either side ends.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}
