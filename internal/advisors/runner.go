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
// each ask for the lints costs the database one run. A project has at most one run under way, and
// the node at most MaxConcurrent, because the lints scan statistics and can take seconds on a large
// database. It is safe for concurrent use.
type Runner struct {
	// TTL is how long a result is served again. Zero means DefaultTTL.
	TTL time.Duration
	// Timeout bounds one run, the database's statement_timeout included. Zero means DefaultTimeout.
	Timeout time.Duration
	// MaxEntries bounds the number of projects kept. Zero means DefaultMaxEntries.
	MaxEntries int
	// MaxConcurrent bounds the runs under way across all projects; the others wait for a slot
	// within their Timeout. Zero means DefaultMaxConcurrent.
	MaxConcurrent int
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu     sync.Mutex
	cache  map[string]cached
	flight map[string]*call
	sem    chan struct{}
}

// Defaults of Runner.
const (
	DefaultTTL        = 10 * time.Second
	DefaultTimeout    = StatementTimeout + 10*time.Second
	DefaultMaxEntries = 256
	// DefaultMaxConcurrent is how many projects run their lints at once.
	DefaultMaxConcurrent = 4
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
	// stale is set, with Runner.mu held, by an Invalidate that came while the run was under way:
	// the run describes the schema before the change, so it answers the callers that joined it
	// and is not kept, and a caller that arrives later waits for it instead of starting a second
	// run beside it.
	stale bool
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
	for {
		r.mu.Lock()
		if c, ok := r.cache[key]; ok && c.schemas == schemas && r.now().Sub(c.at) < r.ttl() {
			r.mu.Unlock()
			return c.lints, nil
		}
		f := r.flight[key]
		if f != nil && !f.stale && f.schemas == schemas {
			r.mu.Unlock()
			return wait(ctx, f)
		}
		if f != nil {
			// The run under way cannot answer this call: it started before an Invalidate, or for
			// other schemas. A project has one run at a time, so wait for it and look again;
			// the callers that wait together then share the one run that follows.
			r.mu.Unlock()
			select {
			case <-f.done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		f = &call{done: make(chan struct{}), schemas: schemas}
		if r.flight == nil {
			r.flight = map[string]*call{}
		}
		r.flight[key] = f
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
			if f.err == nil && !f.stale {
				r.store(key, cached{lints: f.lints, schemas: schemas, at: r.now()})
			}
			r.mu.Unlock()
			close(f.done)
		}()
		return wait(ctx, f)
	}
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
	release, err := r.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := exec(ctx, Query(schemas, r.now()))
	if err != nil {
		return nil, err
	}
	return ParseRows(rows)
}

// acquire takes one of the node's run slots, waiting for one until ctx ends.
func (r *Runner) acquire(ctx context.Context) (release func(), err error) {
	r.mu.Lock()
	if r.sem == nil {
		n := r.MaxConcurrent
		if n <= 0 {
			n = DefaultMaxConcurrent
		}
		r.sem = make(chan struct{}, n)
	}
	sem := r.sem
	r.mu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// store keeps c under key; r.mu must be held. Entries past their TTL are never served again, so
// they go first; when the cache is still full the oldest entry goes.
func (r *Runner) store(key string, c cached) {
	if r.cache == nil {
		r.cache = map[string]cached{}
	}
	for k, v := range r.cache {
		if c.at.Sub(v.at) >= r.ttl() {
			delete(r.cache, k)
		}
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
// something changed the project's schema, so the advisors do not show the old state, and when the
// project is deleted, so nothing stays in memory. A run under way is not cancelled: it finishes
// for the callers that joined it, its result is not kept, and the next call waits for it before
// it starts the run that follows.
func (r *Runner) Invalidate(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, key)
	if f := r.flight[key]; f != nil {
		f.stale = true
	}
}

// IsTimeout reports whether err means the lint query ran out of time: the database cancelled it
// for its statement timeout, or the run's context ended.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "statement timeout")
}
