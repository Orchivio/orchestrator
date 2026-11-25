package httpapi_test

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/admin"
)

// memStore is an in-memory admin.Store; slugs and keys stay taken after deletion.
type memStore struct {
	mu       sync.Mutex
	tenants  map[string]admin.Tenant
	deleted  map[string]bool
	profiles map[uuid.UUID]admin.Profile
	projects map[uuid.UUID]admin.Project
	keys     map[string]bool // adapter/key ever used
	adapter  map[uuid.UUID]string
	tokens   []admin.Token // insertion order
	hashes   map[string]uuid.UUID
	conns    map[uuid.UUID]*memConn
	states   map[string]*memState
}

func newMemStore() *memStore {
	return &memStore{tenants: map[string]admin.Tenant{}, deleted: map[string]bool{}, profiles: map[uuid.UUID]admin.Profile{},
		projects: map[uuid.UUID]admin.Project{}, keys: map[string]bool{}, adapter: map[uuid.UUID]string{}, hashes: map[string]uuid.UUID{},
		conns: map[uuid.UUID]*memConn{}, states: map[string]*memState{}}
}

func (m *memStore) ListTenants(context.Context) ([]admin.Tenant, error) {
	var out []admin.Tenant
	for s, t := range m.tenants {
		if !m.deleted[s] {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b admin.Tenant) int { return compareStrings(a.Slug, b.Slug) })
	return out, nil
}
func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
func (m *memStore) GetTenant(_ context.Context, slug string) (admin.Tenant, error) {
	t, ok := m.tenants[slug]
	if !ok || m.deleted[slug] {
		return admin.Tenant{}, admin.ErrNotFound
	}
	return t, nil
}
func (m *memStore) CreateTenant(_ context.Context, t admin.Tenant) (admin.Tenant, error) {
	if _, ok := m.tenants[t.Slug]; ok {
		return admin.Tenant{}, admin.ErrTaken
	}
	t.Version = 1
	m.tenants[t.Slug] = t
	return t, nil
}
func (m *memStore) UpdateTenant(ctx context.Context, t admin.Tenant, ifVersion int64) (admin.Tenant, error) {
	cur, err := m.GetTenant(ctx, t.Slug)
	if err != nil {
		return admin.Tenant{}, err
	}
	if ifVersion != 0 && ifVersion != cur.Version {
		return admin.Tenant{}, admin.ErrVersionMismatch
	}
	t.Version = cur.Version + 1
	m.tenants[t.Slug] = t
	return t, nil
}
func (m *memStore) DeleteTenant(ctx context.Context, slug string, ifVersion int64) error {
	cur, err := m.GetTenant(ctx, slug)
	if err != nil {
		return err
	}
	if ifVersion != 0 && ifVersion != cur.Version {
		return admin.ErrVersionMismatch
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted[slug] = true
	now := time.Now()
	for _, mc := range m.conns {
		mc.dead = mc.dead || mc.c.TenantSlug == slug
	}
	for i := range m.tokens {
		if m.tokens[i].TenantSlug == slug && m.tokens[i].RevokedAt == nil {
			m.tokens[i].RevokedAt = &now
		}
	}
	return nil
}
func (m *memStore) ListProfiles(_ context.Context, tenant string) ([]admin.Profile, error) {
	var out []admin.Profile
	for _, p := range m.profiles {
		if p.TenantSlug == tenant {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b admin.Profile) int { return compareStrings(a.Name, b.Name) })
	return out, nil
}
func (m *memStore) GetProfile(_ context.Context, tenant string, id uuid.UUID) (admin.Profile, error) {
	p, ok := m.profiles[id]
	if !ok || p.TenantSlug != tenant {
		return admin.Profile{}, admin.ErrNotFound
	}
	return p, nil
}
func (m *memStore) CreateProfile(_ context.Context, p admin.Profile) (admin.Profile, error) {
	for _, o := range m.profiles {
		if o.TenantSlug == p.TenantSlug && o.Name == p.Name {
			return admin.Profile{}, admin.ErrTaken
		}
	}
	p.Version = 1
	m.profiles[p.ID] = p
	return p, nil
}
func (m *memStore) UpdateProfile(ctx context.Context, p admin.Profile, ifVersion int64) (admin.Profile, error) {
	cur, err := m.GetProfile(ctx, p.TenantSlug, p.ID)
	if err != nil {
		return admin.Profile{}, err
	}
	if ifVersion != 0 && ifVersion != cur.Version {
		return admin.Profile{}, admin.ErrVersionMismatch
	}
	p.Version = cur.Version + 1
	m.profiles[p.ID] = p
	return p, nil
}
func (m *memStore) DeleteProfile(ctx context.Context, tenant string, id uuid.UUID, ifVersion int64) error {
	if _, err := m.GetProfile(ctx, tenant, id); err != nil {
		return err
	}
	for _, p := range m.projects {
		if p.ProfileID == id {
			return admin.ErrInUse
		}
	}
	delete(m.profiles, id)
	return nil
}
func (m *memStore) ListProjects(_ context.Context, tenant string) ([]admin.Project, error) {
	var out []admin.Project
	for _, p := range m.projects {
		if p.TenantSlug == tenant {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b admin.Project) int { return compareStrings(a.TrackerKey, b.TrackerKey) })
	return out, nil
}
func (m *memStore) GetProject(_ context.Context, tenant string, id uuid.UUID) (admin.Project, error) {
	p, ok := m.projects[id]
	if !ok || p.TenantSlug != tenant {
		return admin.Project{}, admin.ErrNotFound
	}
	return p, nil
}
func (m *memStore) CreateProject(_ context.Context, p admin.Project, trackerAdapter string) (admin.Project, error) {
	k := p.TenantSlug + "/" + p.TrackerKey
	if m.keys[k] {
		return admin.Project{}, admin.ErrTaken
	}
	m.keys[k] = true
	p.Version = 1
	m.projects[p.ID] = p
	return p, nil
}
func (m *memStore) UpdateProject(ctx context.Context, p admin.Project, ifVersion int64) (admin.Project, error) {
	cur, err := m.GetProject(ctx, p.TenantSlug, p.ID)
	if err != nil {
		return admin.Project{}, err
	}
	if ifVersion != 0 && ifVersion != cur.Version {
		return admin.Project{}, admin.ErrVersionMismatch
	}
	p.Version = cur.Version + 1
	m.projects[p.ID] = p
	return p, nil
}
func (m *memStore) DeleteProject(ctx context.Context, tenant string, id uuid.UUID, ifVersion int64) error {
	if _, err := m.GetProject(ctx, tenant, id); err != nil {
		return err
	}
	delete(m.projects, id)
	return nil
}

func (m *memStore) CreateToken(_ context.Context, t admin.Token, hash []byte) (admin.Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.hashes[string(hash)]; dup {
		return admin.Token{}, admin.ErrTaken
	}
	m.hashes[string(hash)] = t.ID
	m.tokens = append(m.tokens, t)
	return t, nil
}

func (m *memStore) ListTokens(context.Context) ([]admin.Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := slices.Clone(m.tokens)
	slices.Reverse(out)
	return out, nil
}

func (m *memStore) RevokeToken(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.tokens {
		if m.tokens[i].ID == id {
			now := time.Now()
			m.tokens[i].RevokedAt = &now
			return nil
		}
	}
	return admin.ErrNotFound
}

func (m *memStore) TokenByHash(_ context.Context, hash []byte) (admin.Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.hashes[string(hash)]
	if !ok {
		return admin.Token{}, admin.ErrNotFound
	}
	for _, t := range m.tokens {
		if t.ID == id {
			return t, nil
		}
	}
	return admin.Token{}, admin.ErrNotFound
}

func (m *memStore) CountTokens(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tokens), nil
}

// Connections: live rows only; (adapter, external id) exclusive, one live tracker per tenant.
type memConn struct {
	c    admin.Connection
	cred admin.SealedCredential
	dead bool
}

func (m *memStore) live() []*memConn {
	var out []*memConn
	for _, mc := range m.conns {
		if !mc.dead {
			out = append(out, mc)
		}
	}
	slices.SortFunc(out, func(a, b *memConn) int { return a.c.CreatedAt.Compare(b.c.CreatedAt) })
	return out
}

func (m *memStore) ListConnections(_ context.Context, tenant string) ([]admin.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []admin.Connection
	for _, mc := range m.live() {
		if mc.c.TenantSlug == tenant {
			out = append(out, mc.c)
		}
	}
	return out, nil
}

func (m *memStore) ListAllActiveConnections(context.Context) ([]admin.Connection, []admin.SealedCredential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var cs []admin.Connection
	var creds []admin.SealedCredential
	for _, mc := range m.live() {
		if mc.c.Status == admin.StatusActive {
			cs, creds = append(cs, mc.c), append(creds, mc.cred)
		}
	}
	return cs, creds, nil
}

func (m *memStore) GetConnection(_ context.Context, tenant string, id uuid.UUID) (admin.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mc, ok := m.conns[id]; ok && !mc.dead && mc.c.TenantSlug == tenant {
		return mc.c, nil
	}
	return admin.Connection{}, admin.ErrNotFound
}

func (m *memStore) ConnectionCredential(_ context.Context, id uuid.UUID) (admin.SealedCredential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mc, ok := m.conns[id]; ok && !mc.dead {
		return mc.cred, nil
	}
	return admin.SealedCredential{}, admin.ErrNotFound
}

func (m *memStore) LastTrackerExternalID(_ context.Context, tenant string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var last *memConn
	for _, mc := range m.conns {
		if mc.c.TenantSlug == tenant && mc.c.Kind == admin.KindTracker && (last == nil || !mc.dead && last.dead ||
			mc.dead == last.dead && mc.c.CreatedAt.After(last.c.CreatedAt)) {
			last = mc
		}
	}
	if last == nil {
		return "", nil
	}
	return last.c.ExternalID, nil
}

func (m *memStore) SaveConnection(_ context.Context, c admin.Connection, cred admin.SealedCredential) (admin.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveConnection(c, cred)
}

// ReplaceTrackerConnection is all or nothing: replaced rows revive on error.
func (m *memStore) ReplaceTrackerConnection(_ context.Context, tenant string, c admin.Connection, cred admin.SealedCredential) (admin.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var replaced []*memConn
	for _, mc := range m.live() {
		if mc.c.TenantSlug == tenant && mc.c.Kind == admin.KindTracker && (mc.c.Adapter != c.Adapter || mc.c.ExternalID != c.ExternalID) {
			mc.dead, replaced = true, append(replaced, mc)
		}
	}
	saved, err := m.saveConnection(c, cred)
	if err != nil {
		for _, mc := range replaced {
			mc.dead = false
		}
	}
	return saved, err
}

func (m *memStore) saveConnection(c admin.Connection, cred admin.SealedCredential) (admin.Connection, error) {
	for _, mc := range m.live() {
		if mc.c.Adapter == c.Adapter && mc.c.ExternalID == c.ExternalID {
			if mc.c.TenantSlug != c.TenantSlug {
				return admin.Connection{}, admin.ErrConflict
			}
			mc.c.Label, mc.c.Status, mc.c.RefreshAfter, mc.cred, mc.c.UpdatedAt = c.Label, c.Status, c.RefreshAfter, cred, time.Now()
			return mc.c, nil
		}
		if c.Kind == admin.KindTracker && mc.c.Kind == admin.KindTracker && mc.c.TenantSlug == c.TenantSlug {
			return admin.Connection{}, admin.ErrTaken
		}
	}
	c.CreatedAt = time.Now()
	c.UpdatedAt = c.CreatedAt
	m.conns[c.ID] = &memConn{c: c, cred: cred}
	return c, nil
}

// UpdateConnectionCredentialIf writes only if the row's UpdatedAt is still expected.
func (m *memStore) UpdateConnectionCredentialIf(_ context.Context, id uuid.UUID, expected time.Time, cred admin.SealedCredential, refreshAfter *time.Time, status string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mc, ok := m.conns[id]
	if !ok || mc.dead || !mc.c.UpdatedAt.Equal(expected) {
		return false, nil
	}
	mc.cred, mc.c.RefreshAfter, mc.c.Status, mc.c.UpdatedAt = cred, refreshAfter, status, time.Now()
	return true, nil
}

func (m *memStore) DeleteConnection(_ context.Context, tenant string, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mc, ok := m.conns[id]
	if !ok || mc.dead || mc.c.TenantSlug != tenant {
		return admin.ErrNotFound
	}
	mc.dead = true
	return nil
}

func (m *memStore) ClaimDueConnections(_ context.Context, now, leaseUntil time.Time) ([]admin.Connection, []admin.SealedCredential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var cs []admin.Connection
	var creds []admin.SealedCredential
	for _, mc := range m.live() {
		if mc.c.Status == admin.StatusActive && mc.c.RefreshAfter != nil && !mc.c.RefreshAfter.After(now) {
			lease := leaseUntil
			mc.c.RefreshAfter = &lease
			cs, creds = append(cs, mc.c), append(creds, mc.cred)
		}
	}
	return cs, creds, nil
}

type memState struct {
	tenant, adapter, kind string
	expires               time.Time
	used                  bool
}

func (m *memStore) CreateOAuthState(_ context.Context, hash []byte, tenant, adapter, kind string, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[string(hash)] = &memState{tenant: tenant, adapter: adapter, kind: kind, expires: expiresAt}
	return nil
}

func (m *memStore) ConsumeOAuthState(_ context.Context, hash []byte, now time.Time) (string, string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[string(hash)]
	if !ok || st.used || !st.expires.After(now) {
		return "", "", "", admin.ErrNotFound
	}
	st.used = true
	return st.tenant, st.adapter, st.kind, nil
}
