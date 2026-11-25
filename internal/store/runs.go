package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/router"
	"github.com/orchivio/orchestrator/internal/store/db"
)

// ListProjectRuns pages by (created_at, id) descending; runs without project_id never match.
func (s *Store) ListProjectRuns(ctx context.Context, tenant string, projectID uuid.UUID, f admin.RunFilter, after *admin.RunCursor, limit int) ([]router.Run, error) {
	p := db.ListProjectRunsParams{TenantID: tenant, ProjectID: &projectID, Ticket: f.Ticket, State: f.State, MaxRows: int32(limit)}
	if after != nil {
		p.HasCursor, p.CursorAt, p.CursorID = true, after.CreatedAt, after.ID
	}
	rows, err := s.q.ListProjectRuns(ctx, p)
	if err != nil {
		return nil, err
	}
	runs := make([]router.Run, len(rows))
	for i, row := range rows {
		if runs[i], err = toRun(row); err != nil {
			return nil, err
		}
	}
	return runs, nil
}

// GetProjectRun is ErrNotFound unless the run exists with this tenant and project_id.
func (s *Store) GetProjectRun(ctx context.Context, tenant string, projectID, runID uuid.UUID) (router.Run, []admin.RunEvent, error) {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return router.Run{}, nil, err
	}
	if run == nil || run.Tenant != tenant || run.ProjectID != projectID {
		return router.Run{}, nil, fmt.Errorf("run %s in project %s: %w", runID, projectID, admin.ErrNotFound)
	}
	events, err := s.ListEvents(ctx, runID)
	if err != nil {
		return router.Run{}, nil, err
	}
	return *run, events, nil
}
