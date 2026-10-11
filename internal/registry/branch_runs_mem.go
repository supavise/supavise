package registry

import (
	"context"
	"slices"
	"time"
)

func (m *Memory) clock() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func cloneRun(r *BranchRun) BranchRun {
	c := *r
	c.Steps = slices.Clone(r.Steps)
	return c
}

func (m *Memory) findRun(id string) *BranchRun {
	for _, r := range m.branchRuns {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (m *Memory) CreateBranchRun(_ context.Context, r *BranchRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.findRun(r.ID) != nil {
		return ErrConflict
	}
	now := m.clock()
	c := cloneRun(r)
	c.CreatedAt, c.UpdatedAt = now, now
	for i := range c.Steps {
		c.Steps[i].CreatedAt, c.Steps[i].UpdatedAt = now, now
	}
	SortBranchRunSteps(c.Steps)
	m.branchRuns = append(m.branchRuns, &c)
	r.CreatedAt, r.UpdatedAt = now, now
	return nil
}

func (m *Memory) GetBranchRun(_ context.Context, id string) (*BranchRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.findRun(id)
	if r == nil {
		return nil, ErrNotFound
	}
	c := cloneRun(r)
	return &c, nil
}

func (m *Memory) ListBranchRuns(_ context.Context, ownerRef string, offset, limit int) ([]BranchRun, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var mine []BranchRun
	for i := len(m.branchRuns) - 1; i >= 0; i-- { // newest first
		if m.branchRuns[i].OwnerRef == ownerRef {
			mine = append(mine, cloneRun(m.branchRuns[i]))
		}
	}
	total := len(mine)
	if offset < 0 {
		offset = 0
	}
	if offset >= len(mine) {
		return []BranchRun{}, total, nil
	}
	mine = mine[offset:]
	if limit >= 0 && limit < len(mine) {
		mine = mine[:limit]
	}
	return mine, total, nil
}

func (m *Memory) SetBranchRunSteps(_ context.Context, id string, changes []BranchRunStepChange) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.findRun(id)
	if r == nil {
		return ErrNotFound
	}
	// Check every step first: the changes apply together or not at all.
	idx := make([]int, len(changes))
	for i, c := range changes {
		idx[i] = slices.IndexFunc(r.Steps, func(s BranchRunStep) bool { return s.Name == c.Name })
		if idx[i] < 0 {
			return ErrNotFound
		}
	}
	now := m.clock()
	for i, c := range changes {
		st := &r.Steps[idx[i]]
		if len(c.OnlyFrom) > 0 && !slices.Contains(c.OnlyFrom, st.Status) {
			continue
		}
		if c.Status != "" {
			st.Status = c.Status
		}
		st.Log += c.Log
		st.UpdatedAt = now
	}
	r.UpdatedAt = now
	return nil
}

func (m *Memory) FinishBranchRun(_ context.Context, id, status, failure string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.findRun(id)
	if r == nil {
		return ErrNotFound
	}
	r.Status, r.Failure, r.UpdatedAt = status, failure, m.clock()
	return nil
}

func (m *Memory) SetBranchRunCheck(_ context.Context, id string, checkRunID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.findRun(id)
	if r == nil {
		return ErrNotFound
	}
	r.CheckRunID, r.UpdatedAt = checkRunID, m.clock()
	return nil
}

func (m *Memory) RunningBranchRuns(_ context.Context) ([]BranchRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []BranchRun
	for _, r := range m.branchRuns {
		if r.Status == RunRunning {
			out = append(out, cloneRun(r))
		}
	}
	return out, nil
}

func (m *Memory) PruneBranchRuns(_ context.Context, before, orphans time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.branchRuns[:0]
	n := 0
	for _, r := range m.branchRuns {
		_, owned := m.projects[r.OwnerRef]
		if r.CreatedAt.Before(before) || (!owned && r.CreatedAt.Before(orphans)) {
			n++
			continue
		}
		kept = append(kept, r)
	}
	m.branchRuns = kept
	return n, nil
}

func (m *Memory) GetDefaultBranchGit(_ context.Context, ref string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.defaultGit[ref], nil
}

func (m *Memory) SetDefaultBranchGit(_ context.Context, ref, gitBranch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[ref]; !ok {
		return ErrNotFound
	}
	if m.defaultGit == nil {
		m.defaultGit = map[string]string{}
	}
	m.defaultGit[ref] = gitBranch
	return nil
}
