package advisors

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// Exec runs one SQL text against a project's database as the role pg-meta and Studio use for the
// lints and returns the rows of the last statement as a JSON array.
type Exec func(ctx context.Context, sql string) ([]byte, error)

// Runner runs the lint SQL of projects on demand. Results are kept for TTL, and callers that ask
// for the same project while a run is in flight share it, so a dashboard whose pages and widgets
// each ask for the lints costs the database one run. It is safe for concurrent use.
type Runner struct {
	// TTL is how long a result is served again. Zero means DefaultTTL.
	TTL time.Duration
	// Timeout bounds one run, the database's statement_timeout included. Zero means DefaultTimeout.
	Timeout time.Duration
	// MaxEntries bounds the number of projects kept. Zero means DefaultMaxEntries.
	MaxEntries int
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu     sync.Mutex
	cache  map[string]cached
	flight map[string]*call
	// gen counts Invalidate calls per project, so a run that started before one does not store a
	// result that predates the change.
	gen map[string]uint64
}

// Defaults of Runner.
const (
	DefaultTTL        = 10 * time.Second
	DefaultTimeout    = StatementTimeout + 10*time.Second
	DefaultMaxEntries = 256
)

type cached struct {
	lints   []Lint
	schemas string
	at      time.Time
}

type call struct {
	done    chan struct{}
	schemas string
	lints   []Lint
	err     error
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) ttl() time.Duration {
	if r.TTL > 0 {
		return r.TTL
	}
	return DefaultTTL
}

// SQL returns the lints of the vendored query for project key (a project ref) with the Data API
// exposing schemas (comma-separated; empty means DefaultExposedSchemas). The result must not be
// modified. A waiting caller gives up when its own context ends; the run itself continues for
// the others.
func (r *Runner) SQL(ctx context.Context, key, schemas string, exec Exec) ([]Lint, error) {
	if schemas == "" {
		schemas = DefaultExposedSchemas
	}
	r.mu.Lock()
	if c, ok := r.cache[key]; ok && c.schemas == schemas && r.now().Sub(c.at) < r.ttl() {
		r.mu.Unlock()
		return c.lints, nil
	}
	if f, ok := r.flight[key]; ok && f.schemas == schemas {
		r.mu.Unlock()
		return wait(ctx, f)
	}
	f := &call{done: make(chan struct{}), schemas: schemas}
	if r.flight == nil {
		r.flight = map[string]*call{}
	}
	r.flight[key] = f
	gen := r.gen[key]
	r.mu.Unlock()

	// The run belongs to every caller that joined it, so the first caller leaving does not cancel it.
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	go func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		f.lints, f.err = r.run(rctx, schemas, exec)
		r.mu.Lock()
		if r.flight[key] == f {
			delete(r.flight, key)
		}
		if f.err == nil && r.gen[key] == gen {
			r.store(key, cached{lints: f.lints, schemas: schemas, at: r.now()})
		}
		r.mu.Unlock()
		close(f.done)
	}()
	return wait(ctx, f)
}

func wait(ctx context.Context, f *call) ([]Lint, error) {
	select {
	case <-f.done:
		return f.lints, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *Runner) run(ctx context.Context, schemas string, exec Exec) ([]Lint, error) {
	rows, err := exec(ctx, Query(schemas, r.now()))
	if err != nil {
		return nil, err
	}
	return ParseRows(rows)
}

// store keeps c under key; r.mu must be held. When the cache is full the oldest entry goes.
func (r *Runner) store(key string, c cached) {
	if r.cache == nil {
		r.cache = map[string]cached{}
	}
	max := r.MaxEntries
	if max <= 0 {
		max = DefaultMaxEntries
	}
	if _, ok := r.cache[key]; !ok && len(r.cache) >= max {
		oldest, first := "", true
		var at time.Time
		for k, v := range r.cache {
			if first || v.at.Before(at) {
				oldest, at, first = k, v.at, false
			}
		}
		delete(r.cache, oldest)
	}
	r.cache[key] = c
}

// Invalidate forgets what is kept for key: the next call runs the lints again. Call it after
// something changed the project's schema, so the advisors do not show the old state.
func (r *Runner) Invalidate(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, key)
	if r.gen == nil {
		r.gen = map[string]uint64{}
	}
	r.gen[key]++
	delete(r.flight, key)
}

// IsTimeout reports whether err means the lint query ran out of time: the database cancelled it
// for its statement timeout, or the run's context ended.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "statement timeout")
}
