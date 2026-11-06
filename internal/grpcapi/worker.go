// Package grpcapi exposes the orchestrator to workers over gRPC.
package grpcapi

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/google/uuid"
	"github.com/soemii/orchivio-server/internal/orchestrator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/plugin/v1"
	workerv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/worker/v1"
)

const contractVersion = "worker.v1"

// Worker implements workerv1.WorkerServiceServer.
type Worker struct {
	workerv1.UnimplementedWorkerServiceServer
	orch *orchestrator.Orchestrator
}

// NewWorker returns the worker API backed by orch.
func NewWorker(orch *orchestrator.Orchestrator) *Worker { return &Worker{orch: orch} }

func (w *Worker) Register(_ context.Context, req *workerv1.RegisterRequest) (*workerv1.RegisterResponse, error) {
	if err := requireWorker(req.GetWorkerId()); err != nil {
		return nil, err
	}
	if req.GetContractVersion() != contractVersion {
		return nil, status.Errorf(codes.FailedPrecondition, "unsupported contract version %q, want %q", req.GetContractVersion(), contractVersion)
	}
	slog.Info("worker registered", "worker", req.GetWorkerId(), "capacity", req.GetCapacity())
	return &workerv1.RegisterResponse{}, nil
}

func (w *Worker) LeaseRun(ctx context.Context, req *workerv1.LeaseRunRequest) (*workerv1.LeaseRunResponse, error) {
	if err := requireWorker(req.GetWorkerId()); err != nil {
		return nil, err
	}
	a, err := w.orch.Lease(ctx, req.GetWorkerId())
	if err != nil {
		return nil, toStatus(err)
	}
	return &workerv1.LeaseRunResponse{Run: a}, nil
}

func (w *Worker) ReportEvents(stream workerv1.WorkerService_ReportEventsServer) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&workerv1.ReportEventsResponse{})
		}
		if err != nil {
			return err
		}
		if err := requireWorker(req.GetWorkerId()); err != nil {
			return err
		}
		runID, err := uuid.Parse(req.GetRunId())
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "run id: %v", err)
		}
		err = w.orch.RecordEvent(stream.Context(), req.GetWorkerId(), runID, req.GetEvent())
		if errors.Is(err, orchestrator.ErrInvalidEvent) {
			// one bad event must not drop the rest of the run's timeline
			slog.Warn("skipping invalid event", "worker", req.GetWorkerId(), "run", runID, "error", err)
			continue
		}
		if err != nil {
			return toStatus(err)
		}
	}
}

func (w *Worker) GetRepoCredentials(ctx context.Context, req *workerv1.GetRepoCredentialsRequest) (*pluginv1.RepoCredentials, error) {
	if err := requireWorker(req.GetWorkerId()); err != nil {
		return nil, err
	}
	runID, err := uuid.Parse(req.GetRunId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "run id: %v", err)
	}
	creds, err := w.orch.Credentials(ctx, req.GetWorkerId(), runID, req.GetWrite())
	if err != nil {
		return nil, toStatus(err)
	}
	return creds, nil
}

func (w *Worker) CompleteRun(ctx context.Context, req *workerv1.CompleteRunRequest) (*workerv1.CompleteRunResponse, error) {
	if err := requireWorker(req.GetWorkerId()); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(req.GetRunId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "run id: %v", err)
	}
	if err := w.orch.Complete(ctx, req); err != nil {
		return nil, toStatus(err)
	}
	return &workerv1.CompleteRunResponse{}, nil
}

func toStatus(err error) error {
	switch {
	case errors.Is(err, orchestrator.ErrNotLeaseOwner), errors.Is(err, orchestrator.ErrWriteNotAllowed),
		errors.Is(err, orchestrator.ErrBranchMismatch):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, orchestrator.ErrInvalidOutcome), errors.Is(err, orchestrator.ErrInvalidEvent):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		slog.Error("worker api error", "error", err)
		return status.Error(codes.Internal, "internal error")
	}
}

func requireWorker(id string) error {
	if id == "" {
		return status.Error(codes.InvalidArgument, "worker id is required")
	}
	return nil
}
