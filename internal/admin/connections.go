package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/config"
)

const (
	stateTTL     = 10 * time.Minute
	stateBytes   = 32
	callbackPath = "/oauth/callback/"
)

var (
	ErrUnavailable        = errors.New("connections unavailable")        // 503: not configured or adapter unreachable
	ErrAdapter            = errors.New("adapter failed")                 // 502: adapter answered with an error
	ErrCredentialRejected = errors.New("credential rejected by adapter") // grpc Unauthenticated/PermissionDenied
	ErrNotConnected       = errors.New("not connected")                  // grpc FailedPrecondition
)

// NotConnectedError is ErrNotConnected with the adapter's explanation, which
// may be shown to the user (e.g. an org owner has to approve).
type NotConnectedError struct{ Reason string }

func (e *NotConnectedError) Error() string        { return "not connected: " + e.Reason }
func (e *NotConnectedError) Is(target error) bool { return target == ErrNotConnected }

// Completed is a credential handed out by an adapter; Data is opaque.
type Completed struct {
	Data              []byte
	RefreshAfter      *time.Time
	Label, ExternalID string
}

// Connector talks to the adapters named in ORCHIVIO_ADAPTERS; kind selects
// the tracker or code host service of the adapter.
type Connector interface {
	BeginConnect(ctx context.Context, adapter, kind, tenant, redirectURI, state string) (string, error)
	CompleteConnect(ctx context.Context, adapter, kind, redirectURI string, query map[string]string) (Completed, error)
	Refresh(ctx context.Context, adapter, kind string, data []byte, refreshAfter *time.Time) (Completed, error)
	Disconnect(ctx context.Context, adapter, kind string, data []byte) error
	CheckAccess(ctx context.Context, adapter, kind string, creds [][]byte, items []string) ([]string, error)
}

func (s *Service) connectionsReady() error {
	if s.box == nil || s.connector == nil {
		return ErrUnavailable
	}
	return nil
}

func (s *Service) redirectURI(adapter string) string {
	return strings.TrimRight(s.publicURL, "/") + callbackPath + url.PathEscape(adapter)
}

// kindOf is the kind the tenant assigned adapter to, or "" if it did not.
func kindOf(t Tenant, adapter string) string {
	switch adapter {
	case t.TrackerAdapter:
		return KindTracker
	case t.CodeHostAdapter:
		return KindCodeHost
	}
	return ""
}

func hashState(state string) []byte {
	sum := sha256.Sum256([]byte(state))
	return sum[:]
}

// Authorize starts connecting tenant to adapter and returns the provider URL
// the browser has to visit. The state is stored only as its hash.
func (s *Service) Authorize(ctx context.Context, tenant, adapter string) (string, error) {
	if err := s.connectionsReady(); err != nil {
		return "", err
	}
	t, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return "", err
	}
	kind := kindOf(t, adapter)
	if kind == "" {
		return "", invalid([]config.FieldError{{Field: "adapter", Message: "not assigned to tenant " + quote(tenant)}})
	}
	raw := make([]byte, stateBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("oauth state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	if err := s.store.CreateOAuthState(ctx, hashState(state), tenant, adapter, kind, s.now().Add(stateTTL)); err != nil {
		return "", err
	}
	u, err := s.connector.BeginConnect(ctx, adapter, kind, tenant, s.redirectURI(adapter), state)
	if err != nil {
		return "", fmt.Errorf("%w: begin connect at %s: %w", ErrAdapter, adapter, err)
	}
	return u, nil
}

// Callback finishes a connection from the provider's redirect. The single-use
// state is the only authorization; any problem with it is ErrUnauthorized.
func (s *Service) Callback(ctx context.Context, adapter string, query map[string]string) (Connection, error) {
	if err := s.connectionsReady(); err != nil {
		return Connection{}, err
	}
	tenant, kind, err := s.consumeState(ctx, adapter, query["state"])
	if err != nil {
		return Connection{}, err
	}
	done, err := s.connector.CompleteConnect(ctx, adapter, kind, s.redirectURI(adapter), query)
	if err != nil {
		return Connection{}, fmt.Errorf("complete connect at %s for tenant %s: %w", adapter, tenant, err)
	}
	if done.ExternalID == "" {
		return Connection{}, fmt.Errorf("%w: %s returned no external id", ErrAdapter, adapter)
	}
	sealed, err := s.box.Seal(done.Data)
	if err != nil {
		return Connection{}, err
	}
	c := Connection{ID: s.newID(), TenantSlug: tenant, Kind: kind, Adapter: adapter, ExternalID: done.ExternalID,
		Label: done.Label, Status: StatusActive, RefreshAfter: done.RefreshAfter}
	cred := SealedCredential{KeyID: s.box.KeyID(), Data: sealed}
	var saved Connection
	if kind == KindTracker {
		saved, err = s.switchTracker(ctx, c, cred)
	} else {
		saved, err = s.store.SaveConnection(ctx, c, cred)
	}
	if err != nil {
		return Connection{}, err
	}
	s.changed(ctx)
	return saved, nil
}

// consumeState spends the state and checks it was issued for adapter and the
// tenant still assigns adapter to the same kind.
func (s *Service) consumeState(ctx context.Context, adapter, state string) (tenant, kind string, err error) {
	if state == "" {
		return "", "", fmt.Errorf("oauth state missing: %w", ErrUnauthorized)
	}
	tenant, stateAdapter, kind, err := s.store.ConsumeOAuthState(ctx, hashState(state), s.now())
	if errors.Is(err, ErrNotFound) {
		return "", "", fmt.Errorf("oauth state unknown, used or expired: %w", ErrUnauthorized)
	}
	if err != nil {
		return "", "", err
	}
	if stateAdapter != adapter {
		return "", "", fmt.Errorf("oauth state of %s used for %s: %w", stateAdapter, adapter, ErrUnauthorized)
	}
	t, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return "", "", err
	}
	if kindOf(t, adapter) != kind {
		return "", "", fmt.Errorf("adapter %s no longer assigned to tenant %s: %w", adapter, tenant, ErrUnauthorized)
	}
	return tenant, kind, nil
}

// switchTracker saves a tracker connection, replacing one to another
// workspace atomically. A workspace other than the last one connected (even
// if that connection was deleted) is refused while the tenant has live
// projects: their tickets belong to the old workspace.
func (s *Service) switchTracker(ctx context.Context, c Connection, cred SealedCredential) (Connection, error) {
	last, err := s.store.LastTrackerExternalID(ctx, c.TenantSlug)
	if err != nil {
		return Connection{}, err
	}
	if last != "" && last != c.ExternalID {
		if err := s.requireNoProjects(ctx, c.TenantSlug); err != nil {
			return Connection{}, err
		}
	}
	// ponytail: the replaced credential is not revoked; revoking first would break the old
	// connection if the save fails, and the provider call cannot join the transaction.
	return s.store.ReplaceTrackerConnection(ctx, c.TenantSlug, c, cred)
}

// requireNoProjects is ErrProjectsExist while the tenant has live projects.
func (s *Service) requireNoProjects(ctx context.Context, tenant string) error {
	projects, err := s.store.ListProjects(ctx, tenant)
	if err != nil {
		return err
	}
	if len(projects) > 0 {
		return fmt.Errorf("tenant %s has %d projects on its tracker workspace: %w", tenant, len(projects), ErrProjectsExist)
	}
	return nil
}

func (s *Service) ListConnections(ctx context.Context, tenant string) ([]Connection, error) {
	if err := s.connectionsReady(); err != nil {
		return nil, err
	}
	if err := s.requireTenant(ctx, tenant); err != nil {
		return nil, err
	}
	return s.store.ListConnections(ctx, tenant)
}

func (s *Service) GetConnection(ctx context.Context, tenant string, id uuid.UUID) (Connection, error) {
	if err := s.connectionsReady(); err != nil {
		return Connection{}, err
	}
	if err := s.requireTenant(ctx, tenant); err != nil {
		return Connection{}, err
	}
	return s.store.GetConnection(ctx, tenant, id)
}

// DeleteConnection asks the adapter to revoke the credential (best effort,
// failures are logged) and soft-deletes the connection. A tracker connection
// stays while the tenant has projects.
func (s *Service) DeleteConnection(ctx context.Context, tenant string, id uuid.UUID) error {
	c, err := s.GetConnection(ctx, tenant, id)
	if err != nil {
		return err
	}
	if c.Kind == KindTracker {
		if err := s.requireNoProjects(ctx, tenant); err != nil {
			return err
		}
	}
	if err := s.disconnect(ctx, c); err != nil {
		s.log.Warn("disconnect at adapter failed; deleting anyway", "connection", c.ID, "adapter", c.Adapter, "error", err)
	}
	if err := s.store.DeleteConnection(ctx, tenant, id); err != nil {
		return err
	}
	s.changed(ctx)
	return nil
}

func (s *Service) disconnect(ctx context.Context, c Connection) error {
	sealed, err := s.store.ConnectionCredential(ctx, c.ID)
	if err != nil {
		return err
	}
	plain, err := s.box.Open(sealed.KeyID, sealed.Data)
	if err != nil {
		return err
	}
	return s.connector.Disconnect(ctx, c.Adapter, c.Kind, plain)
}
