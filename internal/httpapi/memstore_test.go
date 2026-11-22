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
}

func newMemStore() *memStore {
	return &memStore{tenants: map[string]admin.Tenant{}, deleted: map[string]bool{}, profiles: map[uuid.UUID]admin.Profile{},
		projects: map[uuid.UUID]admin.Project{}, keys: map[string]bool{}, adapter: map[uuid.UUID]string{}, hashes: map[string]uuid.UUID{}}
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
	m.deleted[slug] = true
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
	k := trackerAdapter + "/" + p.TrackerKey
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
