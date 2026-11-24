package admin_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/config"
)

var ctx = context.Background()

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

func newTestService(t *testing.T) *admin.Service {
	t.Helper()
	adapters := map[string]string{"linear": "adapter-linear:7101", "github": "adapter-github:7201"}
	return admin.NewService(newMemStore(), adapters, time.Now, uuid.New, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func hasField(ve *admin.ValidationError, field string) bool {
	return slices.ContainsFunc(ve.Fields, func(f config.FieldError) bool { return f.Field == field })
}

func fieldMessage(ve *admin.ValidationError, field string) string {
	for _, f := range ve.Fields {
		if f.Field == field {
			return f.Message
		}
	}
	return ""
}

// seed creates tenant acme with profile default.
func seed(t *testing.T, svc *admin.Service) admin.Profile {
	t.Helper()
	if _, err := svc.CreateTenant(ctx, admin.Tenant{Slug: "acme", DisplayName: "Acme", TrackerAdapter: "linear", CodeHostAdapter: "github"}); err != nil {
		t.Fatal(err)
	}
	p, err := svc.CreateProfile(ctx, admin.Profile{TenantSlug: "acme", Name: "default", Runtime: "podman", MaxDuration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCreateTenantRejectsUnknownAdapterAndBadSlug(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.CreateTenant(ctx, admin.Tenant{Slug: "Acme!", TrackerAdapter: "jira", CodeHostAdapter: "github"})
	var ve *admin.ValidationError
	if !errors.As(err, &ve) || !hasField(ve, "slug") || !hasField(ve, "trackerAdapter") {
		t.Fatalf("err = %v", err)
	}
}

func TestProjectValidationUsesTenantProfiles(t *testing.T) {
	svc := newTestService(t)
	prof := seed(t, svc)
	_, err := svc.CreateProject(ctx, admin.Project{TenantSlug: "acme", Name: "Res", TrackerKey: "RES", ProfileID: uuid.New()})
	var ve *admin.ValidationError
	if !errors.As(err, &ve) || !hasField(ve, "profileId") || hasField(ve, "profile") {
		t.Fatalf("unknown profile: err = %v", err)
	}
	_, err = svc.CreateProject(ctx, admin.Project{TenantSlug: "acme", Name: "Res", TrackerKey: "RES", ProfileID: prof.ID,
		Classifier: config.ClassifierRules{DefaultRepo: "nope"}})
	if !errors.As(err, &ve) || !hasField(ve, "classifier.defaultRepo") {
		t.Fatalf("default repo: err = %v", err)
	}
}

func TestTrackerKeyIsImmutable(t *testing.T) {
	svc := newTestService(t)
	prof := seed(t, svc)
	p, err := svc.CreateProject(ctx, admin.Project{TenantSlug: "acme", Name: "Res", TrackerKey: "RES", ProfileID: prof.ID})
	if err != nil {
		t.Fatal(err)
	}
	p.TrackerKey = "OTHER"
	_, err = svc.UpdateProject(ctx, p, 0)
	var ve *admin.ValidationError
	if !errors.As(err, &ve) || fieldMessage(ve, "trackerKey") != "immutable" {
		t.Fatalf("err = %v", err)
	}
}

func TestTrackerAdapterIsImmutable(t *testing.T) {
	svc := newTestService(t)
	seed(t, svc)
	_, err := svc.UpdateTenant(ctx, admin.Tenant{Slug: "acme", TrackerAdapter: "github", CodeHostAdapter: "github"}, 0)
	var ve *admin.ValidationError
	if !errors.As(err, &ve) || fieldMessage(ve, "trackerAdapter") != "immutable" {
		t.Fatalf("err = %v", err)
	}
}

func TestTakenTrackerKeyIsValidationError(t *testing.T) {
	svc := newTestService(t)
	prof := seed(t, svc)
	if _, err := svc.CreateProject(ctx, admin.Project{TenantSlug: "acme", Name: "Res", TrackerKey: "RES", ProfileID: prof.ID}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreateProject(ctx, admin.Project{TenantSlug: "acme", Name: "Res2", TrackerKey: "RES", ProfileID: prof.ID})
	var ve *admin.ValidationError
	if !errors.As(err, &ve) || fieldMessage(ve, "trackerKey") != "already taken" {
		t.Fatalf("err = %v", err)
	}
}

func TestProfileValidation(t *testing.T) {
	svc := newTestService(t)
	seed(t, svc)
	_, err := svc.CreateProfile(ctx, admin.Profile{TenantSlug: "acme"})
	var ve *admin.ValidationError
	if !errors.As(err, &ve) || !hasField(ve, "name") || !hasField(ve, "runtime") || !hasField(ve, "maxDuration") {
		t.Fatalf("err = %v", err)
	}
	_, err = svc.CreateProfile(ctx, admin.Profile{TenantSlug: "acme", Name: "default", Runtime: "podman", MaxDuration: time.Hour})
	if !errors.As(err, &ve) || fieldMessage(ve, "name") != "already taken" {
		t.Fatalf("dup: err = %v", err)
	}
}

func TestLoadBuildsRuntimeConfig(t *testing.T) {
	svc := newTestService(t)
	prof := seed(t, svc)
	if _, err := svc.CreateProject(ctx, admin.Project{TenantSlug: "acme", Name: "Res", TrackerKey: "RES", ProfileID: prof.ID}); err != nil {
		t.Fatal(err)
	}
	cfg, err := svc.Load(ctx)
	_, p, ok := cfg.ProjectFor("acme", "RES")
	if err != nil || !ok || p.Profile != "default" || p.ID == uuid.Nil ||
		cfg.Tenants["acme"].Tracker.Address != "adapter-linear:7101" || !slices.Equal(p.Milestones, config.DefaultMilestones()) {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestWriteTriggersOnChange(t *testing.T) {
	svc := newTestService(t)
	calls := 0
	svc.OnChange(func(context.Context) error { calls++; return errors.New("ignored") })
	if _, err := svc.CreateTenant(ctx, admin.Tenant{Slug: "acme", TrackerAdapter: "linear", CodeHostAdapter: "github"}); err != nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	if _, err := svc.CreateTenant(ctx, admin.Tenant{Slug: "!", TrackerAdapter: "linear", CodeHostAdapter: "github"}); err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
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

func TestChildrenOfMissingOrDeletedTenantAreNotFound(t *testing.T) {
	svc := newTestService(t)
	prof := seed(t, svc)
	proj, err := svc.CreateProject(ctx, admin.Project{TenantSlug: "acme", Name: "Res", TrackerKey: "RES", ProfileID: prof.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteTenant(ctx, "acme", 0); err != nil {
		t.Fatal(err)
	}
	calls := map[string]func(slug string) error{
		"ListProfiles":  func(s string) error { _, err := svc.ListProfiles(ctx, s); return err },
		"GetProfile":    func(s string) error { _, err := svc.GetProfile(ctx, s, prof.ID); return err },
		"UpdateProfile": func(s string) error { _, err := svc.UpdateProfile(ctx, prof, 0); return err },
		"DeleteProfile": func(s string) error { return svc.DeleteProfile(ctx, s, prof.ID, 0) },
		"ListProjects":  func(s string) error { _, err := svc.ListProjects(ctx, s); return err },
		"GetProject":    func(s string) error { _, err := svc.GetProject(ctx, s, proj.ID); return err },
		"UpdateProject": func(s string) error { _, err := svc.UpdateProject(ctx, proj, 0); return err },
		"DeleteProject": func(s string) error { return svc.DeleteProject(ctx, s, proj.ID, 0) },
	}
	for name, call := range calls {
		for _, slug := range []string{"acme", "never-existed"} {
			if err := call(slug); !errors.Is(err, admin.ErrNotFound) {
				t.Errorf("%s(%s) = %v, want ErrNotFound", name, slug, err)
			}
		}
	}
}

func TestProfileMaxDurationBelowOneSecondIsInvalid(t *testing.T) {
	svc := newTestService(t)
	seed(t, svc)
	_, err := svc.CreateProfile(ctx, admin.Profile{TenantSlug: "acme", Name: "fast", Runtime: "podman", MaxDuration: 500 * time.Millisecond})
	var ve *admin.ValidationError
	if !errors.As(err, &ve) || fieldMessage(ve, "maxDuration") != "must be at least 1s" {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateTenantReportsAdapterFieldsInFixedOrder(t *testing.T) {
	svc := newTestService(t)
	for range 20 {
		_, err := svc.CreateTenant(ctx, admin.Tenant{Slug: "acme", TrackerAdapter: "x", CodeHostAdapter: "y"})
		var ve *admin.ValidationError
		if !errors.As(err, &ve) || len(ve.Fields) != 2 || ve.Fields[0].Field != "trackerAdapter" || ve.Fields[1].Field != "codeHostAdapter" {
			t.Fatalf("err = %v", err)
		}
	}
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

func (m *memStore) SaveConnection(_ context.Context, c admin.Connection, cred admin.SealedCredential) (admin.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mc := range m.live() {
		if mc.c.Adapter == c.Adapter && mc.c.ExternalID == c.ExternalID {
			if mc.c.TenantSlug != c.TenantSlug {
				return admin.Connection{}, admin.ErrConflict
			}
			mc.c.Label, mc.c.Status, mc.c.RefreshAfter, mc.cred = c.Label, c.Status, nil, cred
			return mc.c, nil
		}
		if c.Kind == admin.KindTracker && mc.c.Kind == admin.KindTracker && mc.c.TenantSlug == c.TenantSlug {
			return admin.Connection{}, admin.ErrTaken
		}
	}
	c.CreatedAt = time.Now()
	m.conns[c.ID] = &memConn{c: c, cred: cred}
	return c, nil
}

func (m *memStore) UpdateConnectionCredential(_ context.Context, id uuid.UUID, cred admin.SealedCredential, refreshAfter *time.Time, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mc, ok := m.conns[id]
	if !ok || mc.dead {
		return admin.ErrNotFound
	}
	mc.cred, mc.c.RefreshAfter, mc.c.Status = cred, refreshAfter, status
	return nil
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
