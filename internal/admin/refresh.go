package admin

import (
	"context"
	"errors"
	"time"
)

// refreshLease keeps a claimed connection from being claimed again while it
// is refreshed; after a failed attempt it is the delay until the next one.
const refreshLease = 5 * time.Minute

// Deadlines of single adapter calls; refreshTimeout stays well below refreshLease.
const (
	refreshTimeout = time.Minute
	checkTimeout   = 30 * time.Second
)

// RefreshDue renews every connection whose refresh time has come. A rejected
// credential marks the connection needs_reauth; any other failure is logged
// and retried once the lease expires. OnChange runs if anything was stored.
func (s *Service) RefreshDue(ctx context.Context) error {
	if err := s.connectionsReady(); err != nil {
		return err
	}
	now := s.now()
	conns, sealed, err := s.store.ClaimDueConnections(ctx, now, now.Add(refreshLease))
	if err != nil {
		return err
	}
	changed := false
	for i, c := range conns {
		if s.refreshOne(ctx, c, sealed[i]) {
			changed = true
		}
	}
	if changed {
		s.changed(ctx)
	}
	return nil
}

// refreshOne reports whether it stored a change.
func (s *Service) refreshOne(ctx context.Context, c Connection, sealed SealedCredential) bool {
	plain, err := s.box.Open(sealed.KeyID, sealed.Data)
	if err != nil {
		s.log.Error("connection credential unusable", "connection", c.ID, "tenant", c.TenantSlug, "error", err)
		return false
	}
	callCtx, cancel := context.WithTimeout(ctx, s.refreshTimeout)
	done, err := s.connector.Refresh(callCtx, c.Adapter, c.Kind, plain, c.RefreshAfter)
	cancel()
	if errors.Is(err, ErrCredentialRejected) {
		s.log.Warn("connection needs re-authorization", "connection", c.ID, "tenant", c.TenantSlug, "adapter", c.Adapter, "error", err)
		return s.stored(ctx, c, sealed, c.RefreshAfter, StatusNeedsReauth)
	}
	if err != nil {
		s.log.Error("connection refresh failed, retrying later", "connection", c.ID, "tenant", c.TenantSlug, "adapter", c.Adapter, "error", err)
		return false
	}
	if len(done.Data) == 0 { // adapter sent none back: keep the old one rather than store nothing
		done.Data = plain
	}
	renewed, err := s.box.Seal(done.Data)
	if err != nil {
		s.log.Error("sealing refreshed connection failed", "connection", c.ID, "tenant", c.TenantSlug, "error", err)
		return false
	}
	return s.stored(ctx, c, SealedCredential{KeyID: s.box.KeyID(), Data: renewed}, done.RefreshAfter, StatusActive)
}

// stored writes the refresh result unless c changed since its claim (e.g. a
// reconnect stored a newer credential); it reports whether it wrote.
func (s *Service) stored(ctx context.Context, c Connection, cred SealedCredential, refreshAfter *time.Time, status string) bool {
	ok, err := s.store.UpdateConnectionCredentialIf(ctx, c.ID, c.UpdatedAt, cred, refreshAfter, status)
	if err != nil {
		s.log.Error("storing refreshed connection failed", "connection", c.ID, "tenant", c.TenantSlug, "error", err)
		return false
	}
	if !ok {
		s.log.Info("connection changed during refresh, result dropped", "connection", c.ID)
	}
	return ok
}
