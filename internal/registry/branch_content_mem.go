package registry

import (
	"context"
	"maps"
	"slices"
)

func (m *Memory) GetBranchSchemaBase(_ context.Context, branchRef string) (*BranchSchemaBase, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.schemaBases[branchRef]
	if !ok {
		return nil, ErrNotFound
	}
	b.Snapshot = slices.Clone(b.Snapshot)
	return &b, nil
}

func (m *Memory) PutBranchSchemaBase(_ context.Context, b BranchSchemaBase) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[b.BranchRef]; !ok {
		return ErrNotFound
	}
	if b.TakenAt.IsZero() {
		b.TakenAt = m.clock()
	}
	if m.schemaBases == nil {
		m.schemaBases = map[string]BranchSchemaBase{}
	}
	b.Snapshot = slices.Clone(b.Snapshot)
	m.schemaBases[b.BranchRef] = b
	return nil
}

func (m *Memory) GetBranchFunctionBase(_ context.Context, branchRef string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.functionBases[branchRef]), nil
}

func (m *Memory) PutBranchFunctionBase(_ context.Context, branchRef string, set map[string]string, drop []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[branchRef]; !ok {
		return ErrNotFound
	}
	if m.functionBases == nil {
		m.functionBases = map[string]map[string]string{}
	}
	cur := m.functionBases[branchRef]
	if cur == nil {
		cur = map[string]string{}
		m.functionBases[branchRef] = cur
	}
	maps.Copy(cur, set)
	for _, s := range drop {
		delete(cur, s)
	}
	return nil
}
