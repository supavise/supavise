package registry

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"time"
)

// githubMem is the in-memory GitHubStore of Memory.
type githubMem struct {
	app      *GitHubApp
	states   map[string]memState // by hex of the hash
	auths    map[string]*GitHubAuthorization
	conns    []*GitHubConnection
	links    map[string]*GitHubBranchLink
	seen     map[string]time.Time
	nextAuth int64
	nextConn int64
}

type memState struct {
	params  string
	expires time.Time
}

// gh returns the GitHub state with m.mu held.
func (m *Memory) gh() *githubMem {
	if m.github == nil {
		m.github = &githubMem{states: map[string]memState{}, auths: map[string]*GitHubAuthorization{},
			links: map[string]*GitHubBranchLink{}, seen: map[string]time.Time{}}
	}
	return m.github
}

// dropInstallations forgets the connections and the branch links, which name installations of the
// App that goes.
func (g *githubMem) dropInstallations() {
	g.conns = nil
	g.links = map[string]*GitHubBranchLink{}
}

func cloneApp(a *GitHubApp) *GitHubApp {
	c := *a
	c.SealedClientSecret = bytes.Clone(a.SealedClientSecret)
	c.SealedWebhookSecret = bytes.Clone(a.SealedWebhookSecret)
	c.SealedPrivateKey = bytes.Clone(a.SealedPrivateKey)
	return &c
}

func cloneAuth(a *GitHubAuthorization) *GitHubAuthorization {
	c := *a
	c.SealedAccess = bytes.Clone(a.SealedAccess)
	c.SealedRefresh = bytes.Clone(a.SealedRefresh)
	return &c
}

func cloneLink(l *GitHubBranchLink) *GitHubBranchLink {
	c := *l
	c.Functions = maps.Clone(nonNilMap(l.Functions))
	if l.SeededAt != nil {
		t := *l.SeededAt
		c.SeededAt = &t
	}
	return &c
}

func (m *Memory) GetGitHubApp(_ context.Context) (*GitHubApp, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if g := m.gh(); g.app != nil {
		return cloneApp(g.app), nil
	}
	return nil, ErrNotFound
}

func (m *Memory) PutGitHubApp(_ context.Context, a *GitHubApp) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.gh()
	now := m.clock()
	c := cloneApp(a)
	c.CreatedAt, c.UpdatedAt = now, now
	g.app = c
	g.auths = map[string]*GitHubAuthorization{}
	g.states = map[string]memState{}
	g.dropInstallations()
	a.CreatedAt, a.UpdatedAt = now, now
	return nil
}

func (m *Memory) DeleteGitHubApp(_ context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.gh()
	had := g.app != nil
	g.app = nil
	g.auths = map[string]*GitHubAuthorization{}
	g.states = map[string]memState{}
	g.dropInstallations()
	return had, nil
}

func (m *Memory) PutGitHubState(_ context.Context, stateHash []byte, params, _ string, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.gh()
	k := string(stateHash)
	if _, dup := g.states[k]; dup {
		return ErrConflict
	}
	g.states[k] = memState{params: params, expires: expires}
	return nil
}

func (m *Memory) PeekGitHubState(_ context.Context, stateHash []byte, now time.Time) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.gh().states[string(stateHash)]
	if !ok || !st.expires.After(now) {
		return "", false, nil
	}
	return st.params, true, nil
}

func (m *Memory) TakeGitHubState(_ context.Context, stateHash []byte, now time.Time) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.gh()
	k := string(stateHash)
	st, ok := g.states[k]
	if !ok {
		return "", false, nil
	}
	delete(g.states, k)
	if !st.expires.After(now) {
		return "", false, nil
	}
	return st.params, true, nil
}

func (m *Memory) GetGitHubAuthorization(_ context.Context, userID string) (*GitHubAuthorization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.gh().auths[userID]; ok {
		return cloneAuth(a), nil
	}
	return nil, ErrNotFound
}

func (m *Memory) PutGitHubAuthorization(_ context.Context, a *GitHubAuthorization) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.gh()
	now := m.clock()
	c := cloneAuth(a)
	if old, ok := g.auths[a.UserID]; ok {
		c.ID, c.CreatedAt = old.ID, old.CreatedAt
	} else {
		g.nextAuth++
		c.ID, c.CreatedAt = g.nextAuth, now
	}
	c.UpdatedAt = now
	g.auths[a.UserID] = c
	a.ID, a.CreatedAt, a.UpdatedAt = c.ID, c.CreatedAt, c.UpdatedAt
	return nil
}

func (m *Memory) DeleteGitHubAuthorization(_ context.Context, userID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.gh()
	_, ok := g.auths[userID]
	delete(g.auths, userID)
	return ok, nil
}

func (m *Memory) findGitHubConn(id int64) int {
	return slices.IndexFunc(m.gh().conns, func(c *GitHubConnection) bool { return c.ID == id })
}

func (m *Memory) GetGitHubConnection(_ context.Context, id int64) (*GitHubConnection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i := m.findGitHubConn(id); i >= 0 {
		c := *m.gh().conns[i]
		c.ProdFunctions = maps.Clone(c.ProdFunctions)
		return &c, nil
	}
	return nil, ErrNotFound
}

func (m *Memory) GetGitHubConnectionByProject(_ context.Context, projectRef string) (*GitHubConnection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.gh().conns {
		if c.ProjectRef == projectRef {
			cc := *c
			cc.ProdFunctions = maps.Clone(cc.ProdFunctions)
			return &cc, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) listGitHubConns(keep func(*GitHubConnection) bool) []GitHubConnection {
	var out []GitHubConnection
	for _, c := range m.gh().conns {
		if keep(c) {
			cc := *c
			cc.ProdFunctions = maps.Clone(c.ProdFunctions)
			out = append(out, cc)
		}
	}
	return out
}

func (m *Memory) ListGitHubConnections(_ context.Context, orgID int64) ([]GitHubConnection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listGitHubConns(func(c *GitHubConnection) bool { return c.OrgID == orgID }), nil
}

func (m *Memory) ListGitHubConnectionsByRepo(_ context.Context, repositoryID int64) ([]GitHubConnection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listGitHubConns(func(c *GitHubConnection) bool { return c.RepositoryID == repositoryID }), nil
}

func (m *Memory) CreateGitHubConnection(_ context.Context, c *GitHubConnection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.gh()
	for _, o := range g.conns {
		if o.ProjectRef == c.ProjectRef {
			return ErrConflict
		}
	}
	g.nextConn++
	now := m.clock()
	cc := *c
	cc.ID, cc.InsertedAt, cc.UpdatedAt = g.nextConn, now, now
	g.conns = append(g.conns, &cc)
	c.ID, c.InsertedAt, c.UpdatedAt = cc.ID, now, now
	return nil
}

func (m *Memory) UpdateGitHubConnection(_ context.Context, c *GitHubConnection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.findGitHubConn(c.ID)
	if i < 0 {
		return ErrNotFound
	}
	cur := m.gh().conns[i]
	cur.RepositoryName, cur.Workdir, cur.SupabaseChangesOnly = c.RepositoryName, c.Workdir, c.SupabaseChangesOnly
	cur.BranchLimit, cur.NewBranchPerPR, cur.UpdatedAt = c.BranchLimit, c.NewBranchPerPR, m.clock()
	return nil
}

func (m *Memory) DeleteGitHubConnection(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.findGitHubConn(id)
	if i < 0 {
		return ErrNotFound
	}
	g := m.gh()
	g.conns = slices.Delete(g.conns, i, i+1)
	return nil
}

func (m *Memory) SetGitHubProdFunctions(_ context.Context, id int64, hashes map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.findGitHubConn(id)
	if i < 0 {
		return ErrNotFound
	}
	m.gh().conns[i].ProdFunctions = maps.Clone(nonNilMap(hashes))
	return nil
}

func (m *Memory) GetGitHubBranchLink(_ context.Context, branchRef string) (*GitHubBranchLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.gh().links[branchRef]; ok {
		return cloneLink(l), nil
	}
	return nil, ErrNotFound
}

func (m *Memory) FindGitHubBranchLink(_ context.Context, projectRef string, repositoryID int64, headRef string, pr int) (*GitHubBranchLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *GitHubBranchLink
	for _, l := range m.gh().links {
		if l.ProjectRef != projectRef || l.RepositoryID != repositoryID {
			continue
		}
		if pr > 0 && l.PRNumber != pr || pr <= 0 && l.HeadRef != headRef {
			continue
		}
		// By Git branch, a link with no pull request comes first; then the newest.
		if best == nil || (pr <= 0 && (l.PRNumber == 0) != (best.PRNumber == 0) && l.PRNumber == 0) ||
			((pr > 0 || (l.PRNumber == 0) == (best.PRNumber == 0)) && l.CreatedAt.After(best.CreatedAt)) {
			best = l
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return cloneLink(best), nil
}

func (m *Memory) ListGitHubBranchLinks(_ context.Context, projectRef string) ([]GitHubBranchLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []GitHubBranchLink
	for _, l := range m.gh().links {
		if l.ProjectRef == projectRef {
			out = append(out, *cloneLink(l))
		}
	}
	slices.SortFunc(out, func(a, b GitHubBranchLink) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

func (m *Memory) PutGitHubBranchLink(_ context.Context, l *GitHubBranchLink) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.gh()
	now := m.clock()
	c := cloneLink(l)
	if old, ok := g.links[l.BranchRef]; ok {
		c.CreatedAt = old.CreatedAt
		if old.SeededAt != nil { // a stale link never un-seeds a branch
			c.SeededAt = old.SeededAt
		}
	} else {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	g.links[l.BranchRef] = c
	l.CreatedAt, l.UpdatedAt, l.SeededAt = c.CreatedAt, now, c.SeededAt
	return nil
}

func (m *Memory) ClearGitHubBranchLinkSeed(_ context.Context, branchRef string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.gh().links[branchRef]
	if !ok {
		return ErrNotFound
	}
	l.SeededAt, l.UpdatedAt = nil, m.clock()
	return nil
}

func (m *Memory) DeleteGitHubBranchLink(_ context.Context, branchRef string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.gh().links, branchRef)
	return nil
}

func (m *Memory) GitHubPRNumbers(_ context.Context, branchRefs []string) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, ref := range branchRefs {
		if l, ok := m.gh().links[ref]; ok && l.PRNumber > 0 {
			out[ref] = l.PRNumber
		}
	}
	return out, nil
}

func (m *Memory) RecordGitHubDelivery(_ context.Context, id, _ string, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.gh()
	if _, ok := g.seen[id]; ok {
		return false, nil
	}
	g.seen[id] = now
	return true, nil
}

func (m *Memory) ForgetGitHubDelivery(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.gh().seen, id)
	return nil
}

func (m *Memory) PruneGitHubDeliveries(_ context.Context, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.gh()
	for id, at := range g.seen {
		if at.Before(before) {
			delete(g.seen, id)
		}
	}
	for k, st := range g.states {
		if st.expires.Before(before) {
			delete(g.states, k)
		}
	}
	return nil
}
