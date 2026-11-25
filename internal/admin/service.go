package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/config"
	"github.com/orchivio/orchestrator/internal/secretbox"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

const (
	msgRequired  = "required"
	msgImmutable = "immutable"
)

// Service validates writes, enforces immutability and notifies OnChange.
type Service struct {
	store     Store
	adapters  map[string]string // adapter name -> address
	box       *secretbox.Box    // nil: connections unavailable
	connector Connector         // nil: connections unavailable
	publicURL string
	now       func() time.Time
	newID     func() uuid.UUID
	log       *slog.Logger
	onChange  func(context.Context) error

	refreshTimeout, checkTimeout time.Duration // per adapter call
}

// NewService builds the service. box and conn may be nil; connection
// operations then fail with ErrUnavailable.
func NewService(st Store, adapters map[string]string, box *secretbox.Box, conn Connector, publicURL string,
	now func() time.Time, newID func() uuid.UUID, log *slog.Logger) *Service {
	return &Service{store: st, adapters: adapters, box: box, connector: conn, publicURL: publicURL, now: now, newID: newID, log: log,
		refreshTimeout: refreshTimeout, checkTimeout: checkTimeout}
}

// OnChange registers a hook run after every successful write. Its error is
// logged, never returned. Call before serving requests; not concurrency-safe.
func (s *Service) OnChange(f func(context.Context) error) { s.onChange = f }

func (s *Service) changed(ctx context.Context) {
	if s.onChange == nil {
		return
	}
	if err := s.onChange(ctx); err != nil {
		s.log.Error("on-change hook failed", "error", err)
	}
}

// finish maps a store result of a write: ErrTaken becomes a ValidationError on
// takenField; success triggers OnChange.
func finish[T any](s *Service, ctx context.Context, v T, err error, takenField string) (T, error) {
	if errors.Is(err, ErrTaken) {
		return v, &ValidationError{[]config.FieldError{{Field: takenField, Message: ErrTaken.Error()}}}
	}
	if err == nil {
		s.changed(ctx)
	}
	return v, err
}

func invalid(fields []config.FieldError) error {
	if len(fields) == 0 {
		return nil
	}
	return &ValidationError{fields}
}

// --- tenants ---

func (s *Service) ListTenants(ctx context.Context) ([]Tenant, error) { return s.store.ListTenants(ctx) }

// ListTenantsFor lists the tenants p may see: all for an instance admin, its own for a tenant admin.
func (s *Service) ListTenantsFor(ctx context.Context, p Principal) ([]Tenant, error) {
	if err := p.authorized(); err != nil {
		return nil, err
	}
	if p.IsInstanceAdmin() {
		return s.ListTenants(ctx)
	}
	t, err := s.GetTenant(ctx, p.TenantSlug)
	if errors.Is(err, ErrNotFound) {
		return []Tenant{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []Tenant{t}, nil
}

// CreateTenantFor and DeleteTenantFor are reserved to instance admins.
func (s *Service) CreateTenantFor(ctx context.Context, p Principal, t Tenant) (Tenant, error) {
	if err := p.requireInstanceAdmin(); err != nil {
		return Tenant{}, err
	}
	return s.CreateTenant(ctx, t)
}

func (s *Service) DeleteTenantFor(ctx context.Context, p Principal, slug string, ifVersion int64) error {
	if err := p.SeesTenant(slug); err != nil {
		return err
	}
	if err := p.requireInstanceAdmin(); err != nil {
		return err
	}
	return s.DeleteTenant(ctx, slug, ifVersion)
}

// UpdateTenantFor lets a tenant admin change only the display name of its own
// tenant: the adapters are taken from the stored tenant; naming another one is ErrForbidden.
func (s *Service) UpdateTenantFor(ctx context.Context, p Principal, t Tenant, ifVersion int64) (Tenant, error) {
	if err := p.SeesTenant(t.Slug); err != nil {
		return Tenant{}, err
	}
	if !p.IsInstanceAdmin() {
		cur, err := s.store.GetTenant(ctx, t.Slug)
		if err != nil {
			return Tenant{}, err
		}
		if t.TrackerAdapter != "" && t.TrackerAdapter != cur.TrackerAdapter ||
			t.CodeHostAdapter != "" && t.CodeHostAdapter != cur.CodeHostAdapter {
			return Tenant{}, ErrForbidden
		}
		t.TrackerAdapter, t.CodeHostAdapter = cur.TrackerAdapter, cur.CodeHostAdapter
	}
	return s.UpdateTenant(ctx, t, ifVersion)
}
func (s *Service) GetTenant(ctx context.Context, slug string) (Tenant, error) {
	return s.store.GetTenant(ctx, slug)
}

func (s *Service) validateTenant(t Tenant) []config.FieldError {
	var errs []config.FieldError
	if !slugPattern.MatchString(t.Slug) {
		errs = append(errs, config.FieldError{Field: "slug", Message: "must match " + slugPattern.String()})
	}
	for _, f := range []struct{ field, name string }{{"trackerAdapter", t.TrackerAdapter}, {"codeHostAdapter", t.CodeHostAdapter}} {
		if _, ok := s.adapters[f.name]; !ok {
			errs = append(errs, config.FieldError{Field: f.field, Message: "unknown adapter " + quote(f.name)})
		}
	}
	return errs
}

func quote(s string) string { return `"` + s + `"` }

func (s *Service) CreateTenant(ctx context.Context, t Tenant) (Tenant, error) {
	if err := invalid(s.validateTenant(t)); err != nil {
		return Tenant{}, err
	}
	t.CreatedAt, t.UpdatedAt = s.now(), s.now()
	created, err := s.store.CreateTenant(ctx, t)
	return finish(s, ctx, created, err, "slug")
}

func (s *Service) UpdateTenant(ctx context.Context, t Tenant, ifVersion int64) (Tenant, error) {
	cur, err := s.store.GetTenant(ctx, t.Slug)
	if err != nil {
		return Tenant{}, err
	}
	errs := s.validateTenant(t)
	if t.TrackerAdapter != cur.TrackerAdapter {
		errs = append(errs, config.FieldError{Field: "trackerAdapter", Message: msgImmutable})
	}
	if err := invalid(errs); err != nil {
		return Tenant{}, err
	}
	if t.CodeHostAdapter != cur.CodeHostAdapter {
		if err := s.requireNoCodeHostConnection(ctx, t.Slug); err != nil {
			return Tenant{}, err
		}
	}
	t.CreatedAt, t.UpdatedAt = cur.CreatedAt, s.now()
	updated, err := s.store.UpdateTenant(ctx, t, ifVersion)
	return finish(s, ctx, updated, err, "slug")
}

// requireNoCodeHostConnection is ErrInUse while the tenant has an active code
// host connection: every one must use the tenant's code host adapter.
func (s *Service) requireNoCodeHostConnection(ctx context.Context, tenant string) error {
	conns, err := s.store.ListConnections(ctx, tenant)
	if err != nil {
		return err
	}
	for _, c := range conns {
		if c.Kind == KindCodeHost && c.Status == StatusActive {
			return fmt.Errorf("tenant %s has active code host connection %s: %w", tenant, c.ID, ErrInUse)
		}
	}
	return nil
}

// DeleteTenant soft-deletes the tenant with its connections and revokes its
// tokens (one store transaction), then asks the adapters to revoke the
// connection credentials (best effort, failures logged). The credentials are
// read before and revoked after the delete, so a refused delete keeps them working.
func (s *Service) DeleteTenant(ctx context.Context, slug string, ifVersion int64) error {
	open, err := s.openConnections(ctx, slug)
	if err != nil {
		return err
	}
	if err := s.store.DeleteTenant(ctx, slug, ifVersion); err != nil {
		return err
	}
	for c, plain := range open {
		if err := s.connector.Disconnect(ctx, c.Adapter, c.Kind, plain); err != nil {
			s.log.Warn("disconnect of deleted tenant failed", "tenant", slug, "connection", c.ID, "adapter", c.Adapter, "error", err)
		}
	}
	s.changed(ctx)
	return nil
}

// openConnections decrypts the tenant's live connections; none without
// connections support. An unusable credential is logged and skipped.
func (s *Service) openConnections(ctx context.Context, tenant string) (map[Connection][]byte, error) {
	if s.connectionsReady() != nil {
		return nil, nil
	}
	conns, err := s.store.ListConnections(ctx, tenant)
	if err != nil {
		return nil, err
	}
	out := map[Connection][]byte{}
	for _, c := range conns {
		plain, err := s.openCredential(ctx, c)
		if err != nil {
			s.log.Warn("connection credential unusable", "tenant", tenant, "connection", c.ID, "error", err)
			continue
		}
		out[c] = plain
	}
	return out, nil
}

// requireTenant returns ErrNotFound unless the tenant exists and is not deleted.
func (s *Service) requireTenant(ctx context.Context, slug string) error {
	_, err := s.store.GetTenant(ctx, slug)
	return err
}

// --- profiles ---

func (s *Service) ListProfiles(ctx context.Context, tenant string) ([]Profile, error) {
	if err := s.requireTenant(ctx, tenant); err != nil {
		return nil, err
	}
	return s.store.ListProfiles(ctx, tenant)
}
func (s *Service) GetProfile(ctx context.Context, tenant string, id uuid.UUID) (Profile, error) {
	if err := s.requireTenant(ctx, tenant); err != nil {
		return Profile{}, err
	}
	return s.store.GetProfile(ctx, tenant, id)
}

func validateProfile(p Profile) []config.FieldError {
	var errs []config.FieldError
	if p.Name == "" {
		errs = append(errs, config.FieldError{Field: "name", Message: msgRequired})
	}
	if p.Runtime == "" {
		errs = append(errs, config.FieldError{Field: "runtime", Message: msgRequired})
	}
	if p.MaxDuration < time.Second {
		errs = append(errs, config.FieldError{Field: "maxDuration", Message: "must be at least 1s"})
	}
	return errs
}

func (s *Service) CreateProfile(ctx context.Context, p Profile) (Profile, error) {
	if _, err := s.store.GetTenant(ctx, p.TenantSlug); err != nil {
		return Profile{}, err
	}
	if err := invalid(validateProfile(p)); err != nil {
		return Profile{}, err
	}
	p.ID = s.newID()
	p.CreatedAt, p.UpdatedAt = s.now(), s.now()
	created, err := s.store.CreateProfile(ctx, p)
	return finish(s, ctx, created, err, "name")
}

func (s *Service) UpdateProfile(ctx context.Context, p Profile, ifVersion int64) (Profile, error) {
	cur, err := s.GetProfile(ctx, p.TenantSlug, p.ID)
	if err != nil {
		return Profile{}, err
	}
	if err := invalid(validateProfile(p)); err != nil {
		return Profile{}, err
	}
	p.CreatedAt, p.UpdatedAt = cur.CreatedAt, s.now()
	updated, err := s.store.UpdateProfile(ctx, p, ifVersion)
	return finish(s, ctx, updated, err, "name")
}

func (s *Service) DeleteProfile(ctx context.Context, tenant string, id uuid.UUID, ifVersion int64) error {
	if err := s.requireTenant(ctx, tenant); err != nil {
		return err
	}
	_, err := finish(s, ctx, struct{}{}, s.store.DeleteProfile(ctx, tenant, id, ifVersion), "name")
	return err
}

// --- projects ---

func (s *Service) ListProjects(ctx context.Context, tenant string) ([]Project, error) {
	if err := s.requireTenant(ctx, tenant); err != nil {
		return nil, err
	}
	return s.store.ListProjects(ctx, tenant)
}
func (s *Service) GetProject(ctx context.Context, tenant string, id uuid.UUID) (Project, error) {
	if err := s.requireTenant(ctx, tenant); err != nil {
		return Project{}, err
	}
	return s.store.GetProject(ctx, tenant, id)
}

// validateProject checks the project against the tenant's live profiles.
func (s *Service) validateProject(ctx context.Context, p Project) ([]config.FieldError, error) {
	profiles, err := s.store.ListProfiles(ctx, p.TenantSlug)
	if err != nil {
		return nil, err
	}
	t := config.Tenant{Name: p.TenantSlug, Profiles: map[string]config.Profile{}}
	profileName, found := "", false
	for _, pr := range profiles {
		t.Profiles[pr.Name] = config.Profile{}
		if pr.ID == p.ProfileID {
			profileName, found = pr.Name, true
		}
	}
	var errs []config.FieldError
	for _, f := range config.ValidateProject(t, toConfigProject(p, profileName)) {
		if f.Field != "profile" { // reported below as profileId
			errs = append(errs, f)
		}
	}
	if !found {
		errs = append(errs, config.FieldError{Field: "profileId", Message: "unknown profile " + quote(p.ProfileID.String())})
	}
	return errs, nil
}

func toConfigProject(p Project, profileName string) config.Project {
	return config.Project{ID: p.ID, Name: p.Name, TrackerKey: p.TrackerKey, Profile: profileName, Repos: p.Repos,
		Classifier: p.Classifier, Roles: p.Roles, MaxDiffLines: p.MaxDiffLines, Milestones: p.Milestones}
}

func (s *Service) CreateProject(ctx context.Context, p Project) (Project, error) {
	tenant, err := s.store.GetTenant(ctx, p.TenantSlug)
	if err != nil {
		return Project{}, err
	}
	errs, err := s.validateProject(ctx, p)
	if err != nil {
		return Project{}, err
	}
	if err := s.validateAccess(ctx, tenant, p, errs); err != nil {
		return Project{}, err
	}
	p.ID = s.newID()
	p.CreatedAt, p.UpdatedAt = s.now(), s.now()
	created, err := s.store.CreateProject(ctx, p, tenant.TrackerAdapter)
	return finish(s, ctx, created, err, "trackerKey")
}

func (s *Service) UpdateProject(ctx context.Context, p Project, ifVersion int64) (Project, error) {
	cur, err := s.GetProject(ctx, p.TenantSlug, p.ID)
	if err != nil {
		return Project{}, err
	}
	errs, err := s.validateProject(ctx, p)
	if err != nil {
		return Project{}, err
	}
	if p.TrackerKey != cur.TrackerKey {
		errs = append(errs, config.FieldError{Field: "trackerKey", Message: msgImmutable})
	}
	tenant, err := s.store.GetTenant(ctx, p.TenantSlug)
	if err != nil {
		return Project{}, err
	}
	if err := s.validateAccess(ctx, tenant, p, errs); err != nil {
		return Project{}, err
	}
	p.CreatedAt, p.UpdatedAt = cur.CreatedAt, s.now()
	updated, err := s.store.UpdateProject(ctx, p, ifVersion)
	return finish(s, ctx, updated, err, "trackerKey")
}

func (s *Service) DeleteProject(ctx context.Context, tenant string, id uuid.UUID, ifVersion int64) error {
	if err := s.requireTenant(ctx, tenant); err != nil {
		return err
	}
	_, err := finish(s, ctx, struct{}{}, s.store.DeleteProject(ctx, tenant, id, ifVersion), "trackerKey")
	return err
}
