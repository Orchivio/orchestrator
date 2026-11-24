package admin

import (
	"context"
	"fmt"

	"github.com/orchivio/orchestrator/internal/config"
)

var _ config.Source = (*Service)(nil)

// Load builds the runtime config from all live tenants, profiles and projects.
func (s *Service) Load(ctx context.Context) (*config.Config, error) {
	tenants, err := s.store.ListTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	cfg := &config.Config{Tenants: make(map[string]config.Tenant, len(tenants))}
	for _, t := range tenants {
		ct, err := s.loadTenant(ctx, t)
		if err != nil {
			return nil, err
		}
		cfg.Tenants[t.Slug] = ct
	}
	if err := s.loadCredentials(ctx, tenants, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadCredentials decrypts the active connections into their tenants. A
// connection of an adapter the tenant no longer uses is skipped; one that
// cannot be decrypted is logged (by id) and skipped so the others still load.
func (s *Service) loadCredentials(ctx context.Context, tenants []Tenant, cfg *config.Config) error {
	if s.box == nil {
		return nil
	}
	conns, sealed, err := s.store.ListAllActiveConnections(ctx)
	if err != nil {
		return fmt.Errorf("list active connections: %w", err)
	}
	bySlug := make(map[string]Tenant, len(tenants))
	for _, t := range tenants {
		bySlug[t.Slug] = t
	}
	for i, c := range conns {
		t, ok := bySlug[c.TenantSlug]
		if !ok || kindOf(t, c.Adapter) != c.Kind {
			continue
		}
		plain, err := s.box.Open(sealed[i].KeyID, sealed[i].Data)
		if err != nil {
			s.log.Error("connection credential unusable", "connection", c.ID, "tenant", c.TenantSlug, "error", err)
			continue
		}
		ct := cfg.Tenants[c.TenantSlug]
		if c.Kind == KindTracker {
			ct.TrackerCredential = plain
		} else {
			ct.CodeHostCredentials = append(ct.CodeHostCredentials, plain)
		}
		cfg.Tenants[c.TenantSlug] = ct
	}
	return nil
}

func (s *Service) loadTenant(ctx context.Context, t Tenant) (config.Tenant, error) {
	profiles, err := s.store.ListProfiles(ctx, t.Slug)
	if err != nil {
		return config.Tenant{}, fmt.Errorf("list profiles of tenant %s: %w", t.Slug, err)
	}
	projects, err := s.store.ListProjects(ctx, t.Slug)
	if err != nil {
		return config.Tenant{}, fmt.Errorf("list projects of tenant %s: %w", t.Slug, err)
	}
	ct := config.Tenant{
		Name:     t.Slug,
		Tracker:  config.AdapterRef{Name: t.TrackerAdapter, Address: s.adapters[t.TrackerAdapter]},
		CodeHost: config.AdapterRef{Name: t.CodeHostAdapter, Address: s.adapters[t.CodeHostAdapter]},
		Profiles: make(map[string]config.Profile, len(profiles)),
	}
	names := make(map[[16]byte]string, len(profiles))
	for _, p := range profiles {
		names[p.ID] = p.Name
		ct.Profiles[p.Name] = config.Profile{Runtime: p.Runtime, Model: p.Model, Instructions: p.Instructions, MaxDuration: p.MaxDuration}
	}
	for _, p := range projects {
		cp := toConfigProject(p, names[p.ProfileID])
		if len(cp.Milestones) == 0 {
			cp.Milestones = config.DefaultMilestones()
		}
		ct.Projects = append(ct.Projects, cp)
	}
	return ct, nil
}
