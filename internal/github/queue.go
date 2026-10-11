package github

import (
	"context"
	"sync"
)

// queue runs one piece of work at a time per key, and lets a newer piece of work replace an older
// one. A pull request that gets three commits in a row is built once, from the last: the work on
// the first is cancelled, the second never starts. Work with different keys runs side by side.
type queue struct {
	mu   sync.Mutex
	jobs map[string]*job
}

type job struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// enqueue starts fn on its own goroutine under key. fn gets a context that ends when newer work
// with the same key arrives or the service stops; it should then stop soon and leave the branch
// consistent (a migration that was cut in the middle rolled back).
func (s *Service) enqueue(key string, fn func(ctx context.Context)) {
	ctx, cancel := context.WithCancel(s.base)
	j := &job{cancel: cancel, done: make(chan struct{})}
	s.q.mu.Lock()
	if s.q.jobs == nil {
		s.q.jobs = map[string]*job{}
	}
	prev := s.q.jobs[key]
	s.q.jobs[key] = j
	s.q.mu.Unlock()
	if prev != nil {
		prev.cancel()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			close(j.done)
			cancel()
			s.q.mu.Lock()
			if s.q.jobs[key] == j {
				delete(s.q.jobs, key)
			}
			s.q.mu.Unlock()
		}()
		if prev != nil {
			<-prev.done
		}
		s.q.mu.Lock()
		superseded := s.q.jobs[key] != j
		s.q.mu.Unlock()
		if superseded || ctx.Err() != nil {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("GitHub work panicked", "key", key, "panic", r)
			}
		}()
		fn(ctx)
	}()
}

// idle blocks until no work is queued under key (tests wait for a webhook's work this way).
func (s *Service) idle(key string) {
	for {
		s.q.mu.Lock()
		j := s.q.jobs[key]
		s.q.mu.Unlock()
		if j == nil {
			return
		}
		<-j.done
	}
}
