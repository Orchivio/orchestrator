// Package admin holds the runtime-managed domain (tenants, profiles, projects),
// its persistence port and the service enforcing the business rules.
package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/config"
	"github.com/orchivio/orchestrator/internal/router"
)

type Tenant struct {
	Slug, DisplayName, TrackerAdapter, CodeHostAdapter string
	Version                                            int64
	CreatedAt, UpdatedAt                               time.Time
}

type Profile struct {
	ID                               uuid.UUID
	TenantSlug, Name, Runtime, Model string
	Instructions                     string
	MaxDuration                      time.Duration
	Version                          int64
	CreatedAt, UpdatedAt             time.Time
}

type Project struct {
	ID                           uuid.UUID
	TenantSlug, Name, TrackerKey string
	ProfileID                    uuid.UUID
	Repos                        []config.Repo
	Classifier                   config.ClassifierRules
	Roles                        config.Roles
	MaxDiffLines                 int
	Milestones                   []string
	StatusMap                    map[string]string
	ReviewTrigger                string
	Version                      int64
	CreatedAt, UpdatedAt         time.Time
}

var (
	ErrNotFound        = errors.New("not found")
	ErrInUse           = errors.New("in use")           // 409: active runs, or profile referenced
	ErrVersionMismatch = errors.New("version mismatch") // 412
	ErrTaken           = errors.New("already taken")    // store: slug or tracker key ever used
	ErrConflict        = errors.New("conflict")         // 409: external id connected to another tenant
	// ErrProjectsExist is the ErrInUse of a tracker connection: the tenant's projects need its workspace.
	ErrProjectsExist = fmt.Errorf("tenant has projects: %w", ErrInUse)
)

// ValidationError is the 422 case; Fields names every invalid field.
type ValidationError struct{ Fields []config.FieldError }

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = fmt.Sprintf("%s: %s", f.Field, f.Message)
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

const (
	TokenPrefix     = "orv_"
	ScopeInstance   = "instance"
	ScopeTenant     = "tenant"
	RoleAdmin       = "admin"
	DefaultTokenTTL = 90 * 24 * time.Hour
)

// Token is an API token as stored; the plaintext exists only at issue time.
type Token struct {
	ID                                    uuid.UUID
	Name, Prefix, Scope, TenantSlug, Role string
	CreatedAt, ExpiresAt                  time.Time
	RevokedAt                             *time.Time
}

const (
	KindTracker       = "tracker"
	KindCodeHost      = "codehost"
	StatusActive      = "active"
	StatusNeedsReauth = "needs_reauth"
)

// Connection is a tenant's link to an external workspace or installation.
// The credential is stored apart and only ever as ciphertext.
type Connection struct {
	ID                                                   uuid.UUID
	TenantSlug, Kind, Adapter, ExternalID, Label, Status string
	RefreshAfter                                         *time.Time
	CreatedAt, UpdatedAt                                 time.Time
}

// SealedCredential is an encrypted credential and the id of the key that sealed it.
type SealedCredential struct {
	KeyID string
	Data  []byte
}

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden") // 403: allowed to see, not to do
)

// Principal is who an authenticated request acts as.
type Principal struct{ Scope, TenantSlug, Role string }

// PrincipalOf returns the principal a token grants, false if the token grants
// no admin access (unknown scope or role, or a tenant token without tenant).
func PrincipalOf(t Token) (Principal, bool) {
	p := Principal{Scope: t.Scope, TenantSlug: t.TenantSlug, Role: t.Role}
	return p, p.authorized() == nil
}

// authorized fails closed: only an instance admin or a tenant admin of a named
// tenant has any rights; every other principal (the zero value included) is ErrForbidden.
func (p Principal) authorized() error {
	if p.IsInstanceAdmin() || p.Scope == ScopeTenant && p.Role == RoleAdmin && p.TenantSlug != "" {
		return nil
	}
	return ErrForbidden
}

func (p Principal) IsInstanceAdmin() bool { return p.Scope == ScopeInstance && p.Role == RoleAdmin }

// SeesTenant is nil when p may access tenant slug at all. A foreign tenant is
// ErrNotFound so its existence is not revealed.
func (p Principal) SeesTenant(slug string) error {
	if err := p.authorized(); err != nil {
		return err
	}
	if p.IsInstanceAdmin() || p.TenantSlug == slug {
		return nil
	}
	return fmt.Errorf("tenant %s: %w", slug, ErrNotFound)
}

func (p Principal) requireInstanceAdmin() error {
	if p.IsInstanceAdmin() {
		return nil
	}
	return ErrForbidden
}

// Store persists the admin domain. ifVersion 0 = no precondition. Deletes are
// soft; lists exclude deleted rows.
type Store interface {
	ListTenants(ctx context.Context) ([]Tenant, error)
	GetTenant(ctx context.Context, slug string) (Tenant, error)
	CreateTenant(ctx context.Context, t Tenant) (Tenant, error) // ErrTaken
	UpdateTenant(ctx context.Context, t Tenant, ifVersion int64) (Tenant, error)
	DeleteTenant(ctx context.Context, slug string, ifVersion int64) error // ErrInUse if unfinished runs
	ListProfiles(ctx context.Context, tenant string) ([]Profile, error)
	GetProfile(ctx context.Context, tenant string, id uuid.UUID) (Profile, error)
	CreateProfile(ctx context.Context, p Profile) (Profile, error) // ErrTaken: name used by a live profile
	UpdateProfile(ctx context.Context, p Profile, ifVersion int64) (Profile, error)
	DeleteProfile(ctx context.Context, tenant string, id uuid.UUID, ifVersion int64) error // ErrInUse if a live project uses it
	ListProjects(ctx context.Context, tenant string) ([]Project, error)
	GetProject(ctx context.Context, tenant string, id uuid.UUID) (Project, error)
	CreateProject(ctx context.Context, p Project, trackerAdapter string) (Project, error) // ErrTaken: (adapter, key) ever used
	UpdateProject(ctx context.Context, p Project, ifVersion int64) (Project, error)
	DeleteProject(ctx context.Context, tenant string, id uuid.UUID, ifVersion int64) error // ErrInUse if unfinished runs
	// ListProjectRuns returns up to limit runs with project_id = projectID, newest first, strictly after the cursor.
	ListProjectRuns(ctx context.Context, tenant string, projectID uuid.UUID, f RunFilter, after *RunCursor, limit int) ([]router.Run, error)
	// GetProjectRun returns the run and its events; ErrNotFound unless the run belongs to the project.
	GetProjectRun(ctx context.Context, tenant string, projectID, runID uuid.UUID) (router.Run, []RunEvent, error)
	CreateToken(ctx context.Context, t Token, hash []byte) (Token, error)
	ListTokens(ctx context.Context) ([]Token, error)             // includes revoked, newest first
	RevokeToken(ctx context.Context, id uuid.UUID) error         // ErrNotFound
	TokenByHash(ctx context.Context, hash []byte) (Token, error) // ErrNotFound
	CountTokens(ctx context.Context) (int, error)                // all rows, revoked/expired included

	ListConnections(ctx context.Context, tenant string) ([]Connection, error)               // live only
	ListAllActiveConnections(ctx context.Context) ([]Connection, []SealedCredential, error) // live + active
	GetConnection(ctx context.Context, tenant string, id uuid.UUID) (Connection, error)
	// LastTrackerExternalID is the external id of the tenant's most recent tracker
	// connection, deleted ones included; "" if it never had one.
	LastTrackerExternalID(ctx context.Context, tenant string) (string, error)
	ConnectionCredential(ctx context.Context, id uuid.UUID) (SealedCredential, error)
	// SaveConnection inserts, or reconnects the tenant's live row with the same
	// (adapter, external id). ErrConflict if another tenant holds that external id;
	// ErrTaken if the tenant already has a live tracker connection.
	SaveConnection(ctx context.Context, c Connection, cred SealedCredential) (Connection, error)
	// ReplaceTrackerConnection atomically soft-deletes the tenant's live tracker
	// connection to another workspace and saves c like SaveConnection; on error nothing changes.
	ReplaceTrackerConnection(ctx context.Context, tenant string, c Connection, cred SealedCredential) (Connection, error)
	// UpdateConnectionCredentialIf writes only while the row's UpdatedAt equals
	// expectedUpdatedAt (a claim keeps it, a reconnect moves it); false = changed underneath.
	UpdateConnectionCredentialIf(ctx context.Context, id uuid.UUID, expectedUpdatedAt time.Time, cred SealedCredential,
		refreshAfter *time.Time, status string) (bool, error)
	DeleteConnection(ctx context.Context, tenant string, id uuid.UUID) error // soft
	// ClaimDueConnections returns live, active rows with refresh_after <= now and
	// moves their refresh_after to leaseUntil so a second claim skips them.
	ClaimDueConnections(ctx context.Context, now, leaseUntil time.Time) ([]Connection, []SealedCredential, error)
	CreateOAuthState(ctx context.Context, hash []byte, tenant, adapter, kind string, expiresAt time.Time) error
	// ConsumeOAuthState is single use; unknown, used or expired is ErrNotFound.
	ConsumeOAuthState(ctx context.Context, hash []byte, now time.Time) (tenant, adapter, kind string, err error)
}
