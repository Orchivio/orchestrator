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
	Version                      int64
	CreatedAt, UpdatedAt         time.Time
}

var (
	ErrNotFound        = errors.New("not found")
	ErrInUse           = errors.New("in use")           // 409: active runs, or profile referenced
	ErrVersionMismatch = errors.New("version mismatch") // 412
	ErrTaken           = errors.New("already taken")    // store: slug or tracker key ever used
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

var ErrUnauthorized = errors.New("unauthorized")

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
	CreateToken(ctx context.Context, t Token, hash []byte) (Token, error)
	ListTokens(ctx context.Context) ([]Token, error)             // includes revoked, newest first
	RevokeToken(ctx context.Context, id uuid.UUID) error         // ErrNotFound
	TokenByHash(ctx context.Context, hash []byte) (Token, error) // ErrNotFound
	CountTokens(ctx context.Context) (int, error)                // all rows, revoked/expired included
}
