package admin

import (
	"context"
	"fmt"
	"slices"

	"github.com/orchivio/orchestrator/internal/config"
)

const (
	msgNotConnected  = "not connected"
	msgNotAccessible = "not accessible"
)

// validateAccess turns errs into a ValidationError; only a project valid so
// far is checked at the adapters: its tracker key must be reachable through
// the tenant's tracker connection and its repos through its code host
// connections. An unreachable adapter fails with its error, nothing is saved.
func (s *Service) validateAccess(ctx context.Context, t Tenant, p Project, errs []config.FieldError) error {
	if err := invalid(errs); err != nil {
		return err
	}
	if err := s.connectionsReady(); err != nil {
		return err
	}
	creds, err := s.activeCredentials(ctx, t)
	if err != nil {
		return err
	}
	tracker, err := s.checkAccess(ctx, t.TrackerAdapter, KindTracker, creds[KindTracker], []string{p.TrackerKey},
		"trackerKey", func(int) string { return "trackerKey" })
	if err != nil {
		return err
	}
	if len(p.Repos) == 0 {
		return invalid(tracker)
	}
	slugs := make([]string, len(p.Repos))
	for i, r := range p.Repos {
		slugs[i] = r.Slug
	}
	repos, err := s.checkAccess(ctx, t.CodeHostAdapter, KindCodeHost, creds[KindCodeHost], slugs,
		"repos", func(i int) string { return fmt.Sprintf("repos[%d].slug", i) })
	if err != nil {
		return err
	}
	return invalid(append(tracker, repos...))
}

// checkAccess reports items the adapter denies on itemField(i), or a missing
// connection on missingField.
func (s *Service) checkAccess(ctx context.Context, adapter, kind string, creds [][]byte, items []string,
	missingField string, itemField func(int) string) ([]config.FieldError, error) {
	if len(creds) == 0 {
		return []config.FieldError{{Field: missingField, Message: msgNotConnected}}, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, s.checkTimeout)
	defer cancel()
	denied, err := s.connector.CheckAccess(callCtx, adapter, kind, creds, items)
	if err != nil {
		return nil, fmt.Errorf("check access at %s: %w", adapter, err)
	}
	var errs []config.FieldError
	for i, it := range items {
		if slices.Contains(denied, it) {
			errs = append(errs, config.FieldError{Field: itemField(i), Message: msgNotAccessible})
		}
	}
	return errs, nil
}

// activeCredentials decrypts the tenant's active connections of its assigned adapters by kind.
func (s *Service) activeCredentials(ctx context.Context, t Tenant) (map[string][][]byte, error) {
	conns, err := s.store.ListConnections(ctx, t.Slug)
	if err != nil {
		return nil, err
	}
	out := map[string][][]byte{}
	for _, c := range conns {
		if c.Status != StatusActive || kindOf(t, c.Adapter) != c.Kind {
			continue
		}
		plain, err := s.openCredential(ctx, c)
		if err != nil {
			return nil, err
		}
		out[c.Kind] = append(out[c.Kind], plain)
	}
	return out, nil
}
