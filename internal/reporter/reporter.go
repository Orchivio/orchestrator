// Package reporter writes run milestones and notices as ticket comments.
package reporter

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/soemii/orchivio-server/contract/redact"
	"github.com/soemii/orchivio-server/internal/config"
	"github.com/soemii/orchivio-server/internal/router"
)

// TrackerClient posts comments to the tenant's tracker.
type TrackerClient interface {
	PostComment(ctx context.Context, ref, body string) error
}

// Reporter implements router.Reporter.
type Reporter struct {
	trackers  func(tenant string) TrackerClient
	cfg       *config.Sync
	publicURL string
}

// New returns a reporter; trackers may return nil for unknown tenants.
func New(trackers func(tenant string) TrackerClient, cfg *config.Sync, publicURL string) *Reporter {
	return &Reporter{trackers: trackers, cfg: cfg, publicURL: strings.TrimRight(publicURL, "/")}
}

// Milestone comments on the run's ticket if the project enabled the milestone.
func (r *Reporter) Milestone(ctx context.Context, run router.Run, milestone, text string) error {
	_, project, ok := r.cfg.Current().ProjectFor(run.Tenant, run.Project)
	if !ok || !slices.Contains(project.Milestones, milestone) {
		return nil
	}
	body := redact.String(text) + fmt.Sprintf("\n\n[Timeline](%s/runs/%s)", r.publicURL, run.ID)
	return r.post(ctx, run.Tenant, run.TicketRef, body)
}

// Notice always comments; it belongs to no run and has no timeline link.
func (r *Reporter) Notice(ctx context.Context, tenant, ticketRef, text string) error {
	return r.post(ctx, tenant, ticketRef, redact.String(text))
}

func (r *Reporter) post(ctx context.Context, tenant, ref, body string) error {
	c := r.trackers(tenant)
	if c == nil {
		return fmt.Errorf("no tracker client for tenant %s", tenant)
	}
	return c.PostComment(ctx, ref, body)
}
