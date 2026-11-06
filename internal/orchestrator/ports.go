package orchestrator

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/soemii/orchivio-server/internal/router"

	pluginv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/plugin/v1"
)

// Store is the persistence port of the orchestrator.
type Store interface {
	// LeaseNext returns the oldest queued run, already set to running with
	// owner and lease end, or nil, nil when nothing is queued.
	LeaseNext(ctx context.Context, workerID string, until time.Time) (*router.Run, error)
	GetRun(ctx context.Context, id uuid.UUID) (*router.Run, error) // nil, nil if unknown
	UpdateRun(ctx context.Context, r router.Run) error
	AppendEvent(ctx context.Context, tenant string, runID uuid.UUID, typ string, payload any) error
	AddInput(ctx context.Context, in router.Input) error // agent questions (FromAgent) for the history
}

// CodeHost is the tenant's code host adapter.
type CodeHost interface {
	IssueRepoCredentials(ctx context.Context, tenant string, repos []string, write bool) (*pluginv1.RepoCredentials, error)
	OpenPullRequest(ctx context.Context, tenant string, req *pluginv1.OpenPullRequestRequest) (*pluginv1.PullRequest, error)
}
