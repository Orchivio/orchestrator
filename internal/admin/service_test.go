package admin_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/config"
	"github.com/orchivio/orchestrator/internal/secretbox"
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

// newTestService serves a store in which tenant acme is already connected to
// linear (ws-acme) and github (inst-acme); the connector allows everything.
func newTestService(t *testing.T) *admin.Service {
	t.Helper()
	box, err := secretbox.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	st := newMemStore()
	connectDirectly(t, st, box, "acme", admin.KindTracker, "linear", "ws-acme")
	connectDirectly(t, st, box, "acme", admin.KindCodeHost, "github", "inst-acme")
	adapters := map[string]string{"linear": "adapter-linear:7101", "github": "adapter-github:7201"}
	return admin.NewService(st, adapters, box, &fakeConnector{}, "", time.Now, uuid.New, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// connectDirectly stores an active connection whose credential is "plain-"+externalID.
func connectDirectly(t *testing.T, st admin.Store, box *secretbox.Box, tenant, kind, adapter, externalID string) admin.Connection {
	t.Helper()
	sealed, err := box.Seal([]byte("plain-" + externalID))
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.SaveConnection(ctx, admin.Connection{ID: uuid.New(), TenantSlug: tenant, Kind: kind, Adapter: adapter,
		ExternalID: externalID, Label: externalID, Status: admin.StatusActive}, admin.SealedCredential{KeyID: box.KeyID(), Data: sealed})
	if err != nil {
		t.Fatal(err)
	}
	return c
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
		t.Fatalf("project=%+v err=%v", p, err)
	}
	acme := cfg.Tenants["acme"]
	if string(acme.TrackerCredential) != "plain-ws-acme" || len(acme.CodeHostCredentials) != 1 || string(acme.CodeHostCredentials[0]) != "plain-inst-acme" {
		t.Fatalf("credentials not decrypted into config")
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

func validProject(e *connEnv) admin.Project {
	return admin.Project{TenantSlug: "acme", Name: "Res", TrackerKey: "RES", ProfileID: e.store.profileID(),
		Repos: []config.Repo{{Slug: "o/x", BaseBranch: "main"}, {Slug: "o/y", BaseBranch: "main"}}}
}

func TestProjectValidationUsesCheckAccess(t *testing.T) {
	e := newConnEnv(t)
	_, err := e.svc.CreateProject(ctx, validProject(e))
	var ve *admin.ValidationError
	if !errors.As(err, &ve) || fieldMessage(ve, "trackerKey") != "not connected" || fieldMessage(ve, "repos") != "not connected" {
		t.Fatalf("without connections: err = %v", err)
	}
	if _, err := e.connect(t, "linear", "ws1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.connect(t, "github", "inst1"); err != nil {
		t.Fatal(err)
	}

	e.conn.denied = map[string]bool{"o/y": true, "RES": true}
	_, err = e.svc.CreateProject(ctx, validProject(e))
	if !errors.As(err, &ve) || fieldMessage(ve, "repos[1].slug") != "not accessible" || fieldMessage(ve, "trackerKey") != "not accessible" ||
		hasField(ve, "repos[0].slug") {
		t.Fatalf("denied: err = %v", err)
	}
	want := []checkCall{
		{adapter: "linear", kind: admin.KindTracker, creds: []string{"plain-ws1"}, items: []string{"RES"}},
		{adapter: "github", kind: admin.KindCodeHost, creds: []string{"plain-inst1"}, items: []string{"o/x", "o/y"}},
	}
	if fmt.Sprint(e.conn.checks) != fmt.Sprint(want) {
		t.Fatalf("checks = %+v", e.conn.checks)
	}

	e.conn.denied = nil
	p, err := e.svc.CreateProject(ctx, validProject(e))
	if err != nil {
		t.Fatal(err)
	}
	e.conn.denied = map[string]bool{"o/x": true}
	if _, err := e.svc.UpdateProject(ctx, p, 0); !errors.As(err, &ve) || fieldMessage(ve, "repos[0].slug") != "not accessible" {
		t.Fatalf("update: err = %v", err)
	}
}

func TestProjectNotSavedWhenAdapterUnreachable(t *testing.T) {
	e := newConnEnv(t)
	for _, a := range []string{"linear", "github"} {
		if _, err := e.connect(t, a, a+"-1"); err != nil {
			t.Fatal(err)
		}
	}
	e.conn.checkErr = fmt.Errorf("check access at linear: %w", admin.ErrUnavailable)
	if _, err := e.svc.CreateProject(ctx, validProject(e)); !errors.Is(err, admin.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if ps, _ := e.svc.ListProjects(ctx, "acme"); len(ps) != 0 {
		t.Fatalf("projects = %+v", ps)
	}
}

func TestProjectNeedsActiveConnection(t *testing.T) {
	e := newConnEnv(t)
	tr, err := e.connect(t, "linear", "ws1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.connect(t, "github", "inst1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.store.UpdateConnectionCredentialIf(ctx, tr.ID, e.store.conns[tr.ID].c.UpdatedAt, e.store.conns[tr.ID].cred, nil,
		admin.StatusNeedsReauth); err != nil || !ok {
		t.Fatal(ok, err)
	}
	_, err = e.svc.CreateProject(ctx, validProject(e))
	var ve *admin.ValidationError
	if !errors.As(err, &ve) || fieldMessage(ve, "trackerKey") != "not connected" || hasField(ve, "repos") {
		t.Fatalf("err = %v", err)
	}
}

func TestCodeHostAdapterChangeBlockedByConnections(t *testing.T) {
	e := newConnEnv(t)
	c, err := e.connect(t, "github", "inst1")
	if err != nil {
		t.Fatal(err)
	}
	change := admin.Tenant{Slug: "acme", DisplayName: "Acme", TrackerAdapter: "linear", CodeHostAdapter: "gitlab"}
	if _, err := e.svc.UpdateTenant(ctx, change, 0); !errors.Is(err, admin.ErrInUse) {
		t.Fatalf("with connection: err = %v", err)
	}
	if _, err := e.svc.UpdateTenant(ctx, admin.Tenant{Slug: "acme", DisplayName: "Renamed", TrackerAdapter: "linear", CodeHostAdapter: "github"}, 0); err != nil {
		t.Fatalf("unchanged code host: %v", err)
	}
	if err := e.svc.DeleteConnection(ctx, "acme", c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateTenant(ctx, change, 0); err != nil {
		t.Fatalf("without connection: %v", err)
	}
}

func TestProjectValidationGivesUpOnHangingAdapter(t *testing.T) {
	e := newConnEnv(t)
	for _, a := range []string{"linear", "github"} {
		if _, err := e.connect(t, a, a+"-1"); err != nil {
			t.Fatal(err)
		}
	}
	admin.SetTimeouts(e.svc, time.Minute, 10*time.Millisecond)
	e.conn.block = true

	if _, err := e.svc.CreateProject(ctx, validProject(e)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if ps, _ := e.svc.ListProjects(ctx, "acme"); len(ps) != 0 {
		t.Fatalf("projects = %+v", ps)
	}
}
