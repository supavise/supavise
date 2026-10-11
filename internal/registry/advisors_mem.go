package registry

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/supavise/supavise/internal/secrets"
)

var (
	_ AdvisorStore = (*Postgres)(nil)
	_ AdvisorStore = (*Memory)(nil)
)

func cloneException(e AdvisorException) AdvisorException {
	e.LintMetadata = maps.Clone(e.LintMetadata)
	return e
}

func (m *Memory) ListAdvisorExceptions(_ context.Context, ref string) ([]AdvisorException, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AdvisorException, 0, len(m.advisorRules[ref]))
	for _, e := range m.advisorRules[ref] {
		out = append(out, cloneException(e))
	}
	return out, nil
}

func (m *Memory) CreateAdvisorExceptions(_ context.Context, ref string, in []AdvisorException) ([]AdvisorException, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[ref]; !ok {
		return nil, ErrNotFound
	}
	if len(m.advisorRules[ref])+len(in) > MaxAdvisorExceptions {
		return nil, fmt.Errorf("%w: a project holds at most %d advisor rules", ErrConflict, MaxAdvisorExceptions)
	}
	if m.advisorRules == nil {
		m.advisorRules = map[string][]AdvisorException{}
	}
	now := time.Now().UTC()
	if m.Now != nil {
		now = m.Now().UTC()
	}
	out := make([]AdvisorException, 0, len(in))
	for _, e := range in {
		e = cloneException(e)
		e.ID, e.Ref, e.InsertedAt = secrets.NewUUID(), ref, now
		m.advisorRules[ref] = append(m.advisorRules[ref], e)
		out = append(out, cloneException(e))
	}
	return out, nil
}

func (m *Memory) UpdateAdvisorException(_ context.Context, ref, id string, p AdvisorExceptionPatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := slices.IndexFunc(m.advisorRules[ref], func(e AdvisorException) bool { return e.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	e := &m.advisorRules[ref][i]
	if p.LintCategory != nil {
		e.LintCategory = *p.LintCategory
	}
	if p.LintName != nil {
		e.LintName = *p.LintName
	}
	if p.LintMetadata != nil {
		e.LintMetadata = maps.Clone(p.LintMetadata)
	}
	if p.AssignedTo != nil {
		e.AssignedTo = *p.AssignedTo
	}
	if p.IsDisabled != nil {
		e.IsDisabled = *p.IsDisabled
	}
	if p.Note != nil {
		e.Note = *p.Note
	}
	if p.LastUpdatedBy != "" {
		e.LastUpdatedBy = p.LastUpdatedBy
	}
	return nil
}

func (m *Memory) DeleteAdvisorExceptions(_ context.Context, ref string, ids []string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	before := len(m.advisorRules[ref])
	m.advisorRules[ref] = slices.DeleteFunc(m.advisorRules[ref], func(e AdvisorException) bool { return slices.Contains(ids, e.ID) })
	return before - len(m.advisorRules[ref]), nil
}
