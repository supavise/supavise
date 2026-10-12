package registry

import (
	"context"
	"sort"
	"time"
)

func (m *Memory) SetPause(_ context.Context, ref, reason string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[ref]; !ok {
		return ErrNotFound
	}
	if m.pauses == nil {
		m.pauses = map[string]Pause{}
	}
	m.pauses[ref] = Pause{Ref: ref, Reason: reason, PausedAt: at}
	return nil
}

func (m *Memory) ClearPause(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pauses, ref)
	return nil
}

func (m *Memory) GetPause(_ context.Context, ref string) (*Pause, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pauses[ref]
	if !ok {
		return nil, ErrNotFound
	}
	return &p, nil
}

func (m *Memory) ListPauses(_ context.Context) ([]Pause, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Pause, 0, len(m.pauses))
	for _, p := range m.pauses {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

func (m *Memory) GetExpiryWarning(_ context.Context, ref string) (*ExpiryWarning, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.expiryWarned[ref]
	if !ok {
		return nil, ErrNotFound
	}
	return &w, nil
}

func (m *Memory) SetExpiryWarning(_ context.Context, ref string, forExpiry, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[ref]; !ok {
		return ErrNotFound
	}
	if m.expiryWarned == nil {
		m.expiryWarned = map[string]ExpiryWarning{}
	}
	m.expiryWarned[ref] = ExpiryWarning{For: forExpiry, At: at}
	return nil
}
