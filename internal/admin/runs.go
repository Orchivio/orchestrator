package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/config"
	"github.com/orchivio/orchestrator/internal/router"
)

const (
	defaultRunLimit = 50
	maxRunLimit     = 200
)

// RunFilter narrows a project's run list. Limit 0 means the default page size;
// Before is the opaque cursor of a previous RunPage.Next.
type RunFilter struct {
	Ticket, State string
	Limit         int
	Before        string
}

// RunPage is one page of runs, newest first; Next is "" on the last page.
type RunPage struct {
	Runs []router.Run
	Next string
}

// RunCursor is the keyset position after which the next page starts.
type RunCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// RunEvent is one stored run event.
type RunEvent struct {
	Type    string
	Payload json.RawMessage
	At      time.Time
}

func encodeCursor(r router.Run) string {
	raw, _ := json.Marshal(RunCursor{CreatedAt: r.CreatedAt, ID: r.ID}) // cannot fail: plain struct
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string) (*RunCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode cursor: %w", err)
	}
	var c RunCursor
	if err := json.Unmarshal(raw, &c); err != nil || c.ID == uuid.Nil {
		return nil, fmt.Errorf("cursor is not a run position")
	}
	return &c, nil
}

func runFieldError(field, message string) error {
	return &ValidationError{Fields: []config.FieldError{{Field: field, Message: message}}}
}

// ListRuns pages through the runs of a project of the tenant, newest first.
// A project that does not belong to the tenant is ErrNotFound.
func (s *Service) ListRuns(ctx context.Context, tenant string, projectID uuid.UUID, f RunFilter) (RunPage, error) {
	if f.Limit == 0 {
		f.Limit = defaultRunLimit
	}
	if f.Limit < 1 || f.Limit > maxRunLimit {
		return RunPage{}, runFieldError("limit", fmt.Sprintf("must be between 1 and %d", maxRunLimit))
	}
	var after *RunCursor
	if f.Before != "" {
		var err error
		if after, err = decodeCursor(f.Before); err != nil {
			return RunPage{}, runFieldError("before", "must be a cursor returned as next")
		}
	}
	if _, err := s.GetProject(ctx, tenant, projectID); err != nil {
		return RunPage{}, err
	}
	runs, err := s.store.ListProjectRuns(ctx, tenant, projectID, f, after, f.Limit+1)
	if err != nil {
		return RunPage{}, err
	}
	page := RunPage{Runs: runs}
	if len(runs) > f.Limit {
		page.Runs = runs[:f.Limit]
		page.Next = encodeCursor(page.Runs[f.Limit-1])
	}
	return page, nil
}

// GetRun returns a run with its events; ErrNotFound unless the project belongs
// to the tenant and the run to the project.
func (s *Service) GetRun(ctx context.Context, tenant string, projectID, runID uuid.UUID) (router.Run, []RunEvent, error) {
	if _, err := s.GetProject(ctx, tenant, projectID); err != nil {
		return router.Run{}, nil, err
	}
	return s.store.GetProjectRun(ctx, tenant, projectID, runID)
}
