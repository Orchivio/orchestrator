package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/store/db"
)

func (s *Store) ListConnections(ctx context.Context, tenant string) ([]admin.Connection, error) {
	rows, err := s.q.ListConnections(ctx, tenant)
	return mapRows(rows, err, toConnection)
}

func (s *Store) ListAllActiveConnections(ctx context.Context) ([]admin.Connection, []admin.SealedCredential, error) {
	rows, err := s.q.ListAllActiveConnections(ctx)
	return splitConnections(rows, err)
}

func (s *Store) GetConnection(ctx context.Context, tenant string, id uuid.UUID) (admin.Connection, error) {
	row, err := s.q.GetConnection(ctx, db.GetConnectionParams{TenantSlug: tenant, ID: id})
	if err != nil {
		return admin.Connection{}, mapNoRows("connection", id.String(), err)
	}
	return toConnection(row), nil
}

func (s *Store) ConnectionCredential(ctx context.Context, id uuid.UUID) (admin.SealedCredential, error) {
	row, err := s.q.GetConnectionCredential(ctx, id)
	if err != nil {
		return admin.SealedCredential{}, mapNoRows("connection", id.String(), err)
	}
	return admin.SealedCredential{KeyID: row.KeyID, Data: row.Credential}, nil
}

func (s *Store) SaveConnection(ctx context.Context, c admin.Connection, cred admin.SealedCredential) (admin.Connection, error) {
	return s.saveConnection(ctx, s.q, c, cred)
}

// ReplaceTrackerConnection soft-deletes the tenant's live tracker connection to
// another workspace and saves c, in one transaction: on any error both roll back.
func (s *Store) ReplaceTrackerConnection(ctx context.Context, tenant string, c admin.Connection, cred admin.SealedCredential) (admin.Connection, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return admin.Connection{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit
	q, now := s.q.WithTx(tx), s.now()
	if err := q.DeleteOtherTrackerConnections(ctx, db.DeleteOtherTrackerConnectionsParams{DeletedAt: &now,
		TenantSlug: tenant, Adapter: c.Adapter, ExternalID: c.ExternalID}); err != nil {
		return admin.Connection{}, err
	}
	saved, err := s.saveConnection(ctx, q, c, cred)
	if err != nil {
		return admin.Connection{}, err
	}
	return saved, tx.Commit(ctx)
}

func (s *Store) saveConnection(ctx context.Context, q *db.Queries, c admin.Connection, cred admin.SealedCredential) (admin.Connection, error) {
	row, err := q.SaveConnection(ctx, db.SaveConnectionParams{ID: c.ID, TenantSlug: c.TenantSlug, Kind: c.Kind,
		Adapter: c.Adapter, ExternalID: c.ExternalID, Label: c.Label, Status: c.Status, KeyID: cred.KeyID,
		Credential: cred.Data, CreatedAt: s.now(), RefreshAfter: c.RefreshAfter})
	if errors.Is(err, pgx.ErrNoRows) {
		return admin.Connection{}, fmt.Errorf("%s external id %s: %w", c.Adapter, c.ExternalID, admin.ErrConflict)
	}
	if err != nil {
		return admin.Connection{}, mapWriteErr("tracker connection of tenant "+c.TenantSlug, err)
	}
	return toConnection(row), nil
}

func (s *Store) UpdateConnectionCredential(ctx context.Context, id uuid.UUID, cred admin.SealedCredential, refreshAfter *time.Time, status string) error {
	n, err := s.q.UpdateConnectionCredential(ctx, db.UpdateConnectionCredentialParams{ID: id, KeyID: cred.KeyID,
		Credential: cred.Data, RefreshAfter: refreshAfter, Status: status, UpdatedAt: s.now()})
	if err == nil && n == 0 {
		return fmt.Errorf("connection %s: %w", id, admin.ErrNotFound)
	}
	return err
}

func (s *Store) UpdateConnectionCredentialIf(ctx context.Context, id uuid.UUID, expectedUpdatedAt time.Time, cred admin.SealedCredential,
	refreshAfter *time.Time, status string) (bool, error) {
	n, err := s.q.UpdateConnectionCredentialIf(ctx, db.UpdateConnectionCredentialIfParams{ID: id, UpdatedAt: expectedUpdatedAt,
		KeyID: cred.KeyID, Credential: cred.Data, RefreshAfter: refreshAfter, Status: status, UpdatedAt_2: s.now()})
	return n == 1, err
}

func (s *Store) DeleteConnection(ctx context.Context, tenant string, id uuid.UUID) error {
	now := s.now()
	n, err := s.q.DeleteConnection(ctx, db.DeleteConnectionParams{TenantSlug: tenant, ID: id, DeletedAt: &now})
	if err == nil && n == 0 {
		return fmt.Errorf("connection %s: %w", id, admin.ErrNotFound)
	}
	return err
}

func (s *Store) ClaimDueConnections(ctx context.Context, now, leaseUntil time.Time) ([]admin.Connection, []admin.SealedCredential, error) {
	rows, err := s.q.ClaimDueConnections(ctx, db.ClaimDueConnectionsParams{Now: now, LeaseUntil: leaseUntil})
	return splitConnections(rows, err)
}

func (s *Store) CreateOAuthState(ctx context.Context, hash []byte, tenant, adapter, kind string, expiresAt time.Time) error {
	return mapWriteErr("oauth state", s.q.CreateOAuthState(ctx, db.CreateOAuthStateParams{Hash: hash,
		TenantSlug: tenant, Adapter: adapter, Kind: kind, CreatedAt: s.now(), ExpiresAt: expiresAt}))
}

func (s *Store) ConsumeOAuthState(ctx context.Context, hash []byte, now time.Time) (string, string, string, error) {
	row, err := s.q.ConsumeOAuthState(ctx, db.ConsumeOAuthStateParams{Hash: hash, Now: now})
	if err != nil {
		return "", "", "", mapNoRows("oauth state", "(hidden)", err)
	}
	return row.TenantSlug, row.Adapter, row.Kind, nil
}

func splitConnections(rows []db.Connection, err error) ([]admin.Connection, []admin.SealedCredential, error) {
	if err != nil {
		return nil, nil, err
	}
	conns, creds := make([]admin.Connection, len(rows)), make([]admin.SealedCredential, len(rows))
	for i, r := range rows {
		conns[i], creds[i] = toConnection(r), admin.SealedCredential{KeyID: r.KeyID, Data: r.Credential}
	}
	return conns, creds, nil
}

func toConnection(c db.Connection) admin.Connection {
	return admin.Connection{ID: c.ID, TenantSlug: c.TenantSlug, Kind: c.Kind, Adapter: c.Adapter, ExternalID: c.ExternalID,
		Label: c.Label, Status: c.Status, RefreshAfter: c.RefreshAfter, CreatedAt: c.CreatedAt.UTC(), UpdatedAt: c.UpdatedAt.UTC()}
}
