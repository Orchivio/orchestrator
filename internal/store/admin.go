package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/config"
	"github.com/orchivio/orchestrator/internal/store/db"
)

var _ admin.Store = (*Store)(nil)

const pgUniqueViolation = "23505"

// projectSpec is the jsonb document of projects.spec.
type projectSpec struct {
	Repos        []config.Repo          `json:"repos"`
	Classifier   config.ClassifierRules `json:"classifier"`
	Roles        config.Roles           `json:"roles"`
	MaxDiffLines int                    `json:"maxDiffLines"`
	Milestones   []string               `json:"milestones"`
}

// mapWriteErr turns a unique violation into admin.ErrTaken.
func mapWriteErr(what string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return fmt.Errorf("%s: %w", what, admin.ErrTaken)
	}
	return err
}

// mapNoRows maps "no rows" to ErrNotFound, naming the entity and id.
func mapNoRows(what, id string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s %s: %w", what, id, admin.ErrNotFound)
	}
	return err
}

// afterMiss explains why a conditional write touched no row: the row is gone
// (get fails with ErrNotFound) or its version moved on.
func afterMiss(what, id string, get func() error) error {
	if err := get(); err != nil {
		return err
	}
	return fmt.Errorf("%s %s: %w", what, id, admin.ErrVersionMismatch)
}

func (s *Store) ListTenants(ctx context.Context) ([]admin.Tenant, error) {
	rows, err := s.q.ListTenants(ctx)
	return mapRows(rows, err, toTenant)
}

func (s *Store) GetTenant(ctx context.Context, slug string) (admin.Tenant, error) {
	row, err := s.q.GetTenant(ctx, slug)
	if err != nil {
		return admin.Tenant{}, mapNoRows("tenant", slug, err)
	}
	return toTenant(row), nil
}

func (s *Store) CreateTenant(ctx context.Context, t admin.Tenant) (admin.Tenant, error) {
	row, err := s.q.CreateTenant(ctx, db.CreateTenantParams{Slug: t.Slug, DisplayName: t.DisplayName,
		TrackerAdapter: t.TrackerAdapter, CodeHostAdapter: t.CodeHostAdapter, CreatedAt: s.now()})
	if err != nil {
		return admin.Tenant{}, mapWriteErr("tenant "+t.Slug, err)
	}
	return toTenant(row), nil
}

func (s *Store) UpdateTenant(ctx context.Context, t admin.Tenant, ifVersion int64) (admin.Tenant, error) {
	row, err := s.q.UpdateTenant(ctx, db.UpdateTenantParams{Slug: t.Slug, DisplayName: t.DisplayName,
		TrackerAdapter: t.TrackerAdapter, CodeHostAdapter: t.CodeHostAdapter, UpdatedAt: s.now(), IfVersion: ifVersion})
	if errors.Is(err, pgx.ErrNoRows) {
		return admin.Tenant{}, afterMiss("tenant", t.Slug, func() error { _, e := s.GetTenant(ctx, t.Slug); return e })
	}
	if err != nil {
		return admin.Tenant{}, err
	}
	return toTenant(row), nil
}

// DeleteTenant checks for unfinished runs, then soft-deletes the tenant and its
// connections and revokes its tokens in one transaction.
// ponytail: the run check is not locked; a run created in between slips through, add a lock if that matters.
func (s *Store) DeleteTenant(ctx context.Context, slug string, ifVersion int64) error {
	n, err := s.q.CountUnfinishedRunsOfTenant(ctx, slug)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("tenant %s has %d unfinished runs: %w", slug, n, admin.ErrInUse)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit
	q, now := s.q.WithTx(tx), s.now()
	rows, err := q.DeleteTenant(ctx, db.DeleteTenantParams{Slug: slug, DeletedAt: &now, IfVersion: ifVersion})
	if err != nil {
		return err
	}
	if rows == 0 {
		return afterMiss("tenant", slug, func() error { _, e := s.GetTenant(ctx, slug); return e })
	}
	if err := q.DeleteConnectionsOfTenant(ctx, db.DeleteConnectionsOfTenantParams{TenantSlug: slug, DeletedAt: &now}); err != nil {
		return err
	}
	if err := q.RevokeTokensOfTenant(ctx, db.RevokeTokensOfTenantParams{TenantSlug: pgtype.Text{String: slug, Valid: true}, RevokedAt: &now}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ListProfiles(ctx context.Context, tenant string) ([]admin.Profile, error) {
	rows, err := s.q.ListProfiles(ctx, tenant)
	return mapRows(rows, err, toProfile)
}

func (s *Store) GetProfile(ctx context.Context, tenant string, id uuid.UUID) (admin.Profile, error) {
	row, err := s.q.GetProfile(ctx, db.GetProfileParams{TenantSlug: tenant, ID: id})
	if err != nil {
		return admin.Profile{}, mapNoRows("profile", id.String(), err)
	}
	return toProfile(row), nil
}

func (s *Store) CreateProfile(ctx context.Context, p admin.Profile) (admin.Profile, error) {
	row, err := s.q.CreateProfile(ctx, db.CreateProfileParams{ID: p.ID, TenantSlug: p.TenantSlug, Name: p.Name,
		Runtime: p.Runtime, Model: p.Model, Instructions: p.Instructions,
		MaxDurationSeconds: int64(p.MaxDuration / time.Second), CreatedAt: s.now()})
	if err != nil {
		return admin.Profile{}, mapWriteErr("profile "+p.Name, err)
	}
	return toProfile(row), nil
}

func (s *Store) UpdateProfile(ctx context.Context, p admin.Profile, ifVersion int64) (admin.Profile, error) {
	row, err := s.q.UpdateProfile(ctx, db.UpdateProfileParams{TenantSlug: p.TenantSlug, ID: p.ID, Name: p.Name,
		Runtime: p.Runtime, Model: p.Model, Instructions: p.Instructions,
		MaxDurationSeconds: int64(p.MaxDuration / time.Second), UpdatedAt: s.now(), IfVersion: ifVersion})
	if errors.Is(err, pgx.ErrNoRows) {
		return admin.Profile{}, afterMiss("profile", p.ID.String(), func() error { _, e := s.GetProfile(ctx, p.TenantSlug, p.ID); return e })
	}
	if err != nil {
		return admin.Profile{}, mapWriteErr("profile "+p.Name, err)
	}
	return toProfile(row), nil
}

func (s *Store) DeleteProfile(ctx context.Context, tenant string, id uuid.UUID, ifVersion int64) error {
	n, err := s.q.CountLiveProjectsOfProfile(ctx, id)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("profile %s is used by %d projects: %w", id, n, admin.ErrInUse)
	}
	now := s.now()
	rows, err := s.q.DeleteProfile(ctx, db.DeleteProfileParams{TenantSlug: tenant, ID: id, DeletedAt: &now, IfVersion: ifVersion})
	if err == nil && rows == 0 {
		return afterMiss("profile", id.String(), func() error { _, e := s.GetProfile(ctx, tenant, id); return e })
	}
	return err
}

func (s *Store) ListProjects(ctx context.Context, tenant string) ([]admin.Project, error) {
	rows, err := s.q.ListProjects(ctx, tenant)
	if err != nil {
		return nil, err
	}
	out := make([]admin.Project, len(rows))
	for i, r := range rows {
		if out[i], err = toProject(r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) GetProject(ctx context.Context, tenant string, id uuid.UUID) (admin.Project, error) {
	row, err := s.q.GetProject(ctx, db.GetProjectParams{TenantSlug: tenant, ID: id})
	if err != nil {
		return admin.Project{}, mapNoRows("project", id.String(), err)
	}
	return toProject(row)
}

func (s *Store) CreateProject(ctx context.Context, p admin.Project, trackerAdapter string) (admin.Project, error) {
	spec, err := marshalSpec(p)
	if err != nil {
		return admin.Project{}, err
	}
	row, err := s.q.CreateProject(ctx, db.CreateProjectParams{ID: p.ID, TenantSlug: p.TenantSlug,
		TrackerAdapter: trackerAdapter, TrackerKey: p.TrackerKey, Name: p.Name, ProfileID: p.ProfileID,
		Spec: spec, CreatedAt: s.now()})
	if err != nil {
		return admin.Project{}, mapWriteErr("project "+p.TrackerKey, err)
	}
	return toProject(row)
}

func (s *Store) UpdateProject(ctx context.Context, p admin.Project, ifVersion int64) (admin.Project, error) {
	spec, err := marshalSpec(p)
	if err != nil {
		return admin.Project{}, err
	}
	row, err := s.q.UpdateProject(ctx, db.UpdateProjectParams{TenantSlug: p.TenantSlug, ID: p.ID, Name: p.Name,
		ProfileID: p.ProfileID, Spec: spec, UpdatedAt: s.now(), IfVersion: ifVersion})
	if errors.Is(err, pgx.ErrNoRows) {
		return admin.Project{}, afterMiss("project", p.ID.String(), func() error { _, e := s.GetProject(ctx, p.TenantSlug, p.ID); return e })
	}
	if err != nil {
		return admin.Project{}, err
	}
	return toProject(row)
}

// DeleteProject: see DeleteTenant for the check-then-delete caveat.
func (s *Store) DeleteProject(ctx context.Context, tenant string, id uuid.UUID, ifVersion int64) error {
	p, err := s.GetProject(ctx, tenant, id)
	if err != nil {
		return err
	}
	n, err := s.q.CountUnfinishedRunsOfProject(ctx, db.CountUnfinishedRunsOfProjectParams{TenantID: tenant, Project: p.TrackerKey})
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("project %s has %d unfinished runs: %w", id, n, admin.ErrInUse)
	}
	now := s.now()
	rows, err := s.q.DeleteProject(ctx, db.DeleteProjectParams{TenantSlug: tenant, ID: id, DeletedAt: &now, IfVersion: ifVersion})
	if err == nil && rows == 0 {
		return afterMiss("project", id.String(), func() error { _, e := s.GetProject(ctx, tenant, id); return e })
	}
	return err
}

func marshalSpec(p admin.Project) ([]byte, error) {
	return json.Marshal(projectSpec{Repos: p.Repos, Classifier: p.Classifier, Roles: p.Roles,
		MaxDiffLines: p.MaxDiffLines, Milestones: p.Milestones})
}

func toTenant(t db.Tenant) admin.Tenant {
	return admin.Tenant{Slug: t.Slug, DisplayName: t.DisplayName, TrackerAdapter: t.TrackerAdapter,
		CodeHostAdapter: t.CodeHostAdapter, Version: t.Version, CreatedAt: t.CreatedAt.UTC(), UpdatedAt: t.UpdatedAt.UTC()}
}

func toProfile(p db.Profile) admin.Profile {
	return admin.Profile{ID: p.ID, TenantSlug: p.TenantSlug, Name: p.Name, Runtime: p.Runtime, Model: p.Model,
		Instructions: p.Instructions, MaxDuration: time.Duration(p.MaxDurationSeconds) * time.Second,
		Version: p.Version, CreatedAt: p.CreatedAt.UTC(), UpdatedAt: p.UpdatedAt.UTC()}
}

func toProject(p db.Project) (admin.Project, error) {
	var spec projectSpec
	if err := json.Unmarshal(p.Spec, &spec); err != nil {
		return admin.Project{}, fmt.Errorf("project %s spec: %w", p.ID, err)
	}
	return admin.Project{ID: p.ID, TenantSlug: p.TenantSlug, Name: p.Name, TrackerKey: p.TrackerKey,
		ProfileID: p.ProfileID, Repos: spec.Repos, Classifier: spec.Classifier, Roles: spec.Roles,
		MaxDiffLines: spec.MaxDiffLines, Milestones: spec.Milestones,
		Version: p.Version, CreatedAt: p.CreatedAt.UTC(), UpdatedAt: p.UpdatedAt.UTC()}, nil
}

func (s *Store) CreateToken(ctx context.Context, t admin.Token, hash []byte) (admin.Token, error) {
	row, err := s.q.CreateToken(ctx, db.CreateTokenParams{ID: t.ID, Name: t.Name, Prefix: t.Prefix, Hash: hash,
		Scope: t.Scope, TenantSlug: pgtype.Text{String: t.TenantSlug, Valid: t.TenantSlug != ""}, Role: t.Role, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt})
	if err != nil {
		return admin.Token{}, mapWriteErr("token "+t.ID.String(), err)
	}
	return toToken(row), nil
}

func (s *Store) ListTokens(ctx context.Context) ([]admin.Token, error) {
	rows, err := s.q.ListTokens(ctx)
	return mapRows(rows, err, toToken)
}

func (s *Store) RevokeToken(ctx context.Context, id uuid.UUID) error {
	n, err := s.q.RevokeToken(ctx, db.RevokeTokenParams{ID: id, RevokedAt: ptr(s.now())})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("token %s: %w", id, admin.ErrNotFound)
	}
	return nil
}

func (s *Store) TokenByHash(ctx context.Context, hash []byte) (admin.Token, error) {
	row, err := s.q.TokenByHash(ctx, hash)
	if err != nil {
		return admin.Token{}, mapNoRows("token", "by hash", err)
	}
	return toToken(row), nil
}

func (s *Store) CountTokens(ctx context.Context) (int, error) {
	n, err := s.q.CountTokens(ctx)
	return int(n), err
}

func ptr[T any](v T) *T { return &v }

func toToken(t db.Token) admin.Token {
	tok := admin.Token{ID: t.ID, Name: t.Name, Prefix: t.Prefix, Scope: t.Scope, Role: t.Role,
		CreatedAt: t.CreatedAt.UTC(), ExpiresAt: t.ExpiresAt.UTC()}
	if t.TenantSlug.Valid {
		tok.TenantSlug = t.TenantSlug.String
	}
	if t.RevokedAt != nil {
		tok.RevokedAt = ptr(t.RevokedAt.UTC())
	}
	return tok
}
