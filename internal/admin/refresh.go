package admin

import (
	"context"
	"errors"
	"time"
)

// refreshLease keeps a claimed connection from being claimed again while it
// is refreshed; after a failed attempt it is the delay until the next one.
const refreshLease = 5 * time.Minute

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
	done, err := s.connector.Refresh(ctx, c.Adapter, c.Kind, plain, c.RefreshAfter)
	if errors.Is(err, ErrCredentialRejected) {
		s.log.Warn("connection needs re-authorization", "connection", c.ID, "tenant", c.TenantSlug, "adapter", c.Adapter, "error", err)
		return s.stored(c, s.store.UpdateConnectionCredential(ctx, c.ID, sealed, c.RefreshAfter, StatusNeedsReauth))
	}
	if err != nil {
		s.log.Error("connection refresh failed, retrying later", "connection", c.ID, "tenant", c.TenantSlug, "adapter", c.Adapter, "error", err)
		return false
	}
	if len(done.Data) == 0 { // adapter sent none back: keep the old one rather than store nothing
		done.Data = plain
	}
	renewed, err := s.box.Seal(done.Data)
	if err == nil {
		err = s.store.UpdateConnectionCredential(ctx, c.ID, SealedCredential{KeyID: s.box.KeyID(), Data: renewed}, done.RefreshAfter, StatusActive)
	}
	return s.stored(c, err)
}

// stored logs a failed write of c and reports whether it succeeded.
func (s *Service) stored(c Connection, err error) bool {
	if err != nil {
		s.log.Error("storing refreshed connection failed", "connection", c.ID, "tenant", c.TenantSlug, "error", err)
	}
	return err == nil
}
