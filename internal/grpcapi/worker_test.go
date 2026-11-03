package grpcapi

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/soemii/orchivio-server/internal/config"
	"github.com/soemii/orchivio-server/internal/orchestrator"
	"github.com/soemii/orchivio-server/internal/router"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pluginv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/plugin/v1"
	workerv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/worker/v1"
)

const cfgYAML = `tenant: acme
tracker: { name: linear, address: x }
codeHost: { name: github, address: y }
profiles:
  default: { runtime: claude-code, model: m, maxDuration: 20m }
projects:
  - name: demo
    trackerKey: ORC
    profile: default
    repos: [ { slug: a/b, baseBranch: main, allowedPaths: ["src/"] } ]
`

type store struct{ runs map[uuid.UUID]*router.Run }

func (s *store) LeaseNext(_ context.Context, w string, until time.Time) (*router.Run, error) {
	for _, r := range s.runs {
		if r.State == router.StateQueued {
			r.State, r.LeaseOwner, r.LeaseUntil = router.StateRunning, w, &until
			c := *r
			return &c, nil
		}
	}
	return nil, nil
}
func (s *store) GetRun(_ context.Context, id uuid.UUID) (*router.Run, error) {
	if r := s.runs[id]; r != nil {
		c := *r
		return &c, nil
	}
	return nil, nil
}
func (s *store) UpdateRun(_ context.Context, r router.Run) error                   { s.runs[r.ID] = &r; return nil }
func (s *store) AppendEvent(context.Context, string, uuid.UUID, string, any) error { return nil }

type host struct{}

func (host) IssueRepoCredentials(context.Context, string, []string, bool) (*pluginv1.RepoCredentials, error) {
	return &pluginv1.RepoCredentials{Token: "t"}, nil
}
func (host) OpenPullRequest(context.Context, string, *pluginv1.OpenPullRequestRequest) (*pluginv1.PullRequest, error) {
	return &pluginv1.PullRequest{}, nil
}

type tracker struct{}

func (tracker) GetTicket(context.Context, string, string) (*pluginv1.Ticket, error) {
	return &pluginv1.Ticket{Title: "T"}, nil
}

type reporter struct{}

func (reporter) Milestone(context.Context, router.Run, string, string) error { return nil }
func (reporter) Notice(context.Context, string, string, string) error        { return nil }

func setup(t *testing.T) (workerv1.WorkerServiceClient, uuid.UUID) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "acme.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewSync(dir, time.Hour, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	run := &router.Run{ID: uuid.New(), Tenant: "acme", Project: "ORC", TicketRef: "ORC-1", Kind: "question",
		State: router.StateQueued, Repos: []string{"a/b"}}
	st := &store{runs: map[uuid.UUID]*router.Run{run.ID: run}}
	o := orchestrator.New(cfg, st, host{}, tracker{}, reporter{}, func(context.Context, uuid.UUID) error { return nil },
		time.Now, time.Hour, "http://x")
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(srv, NewWorker(o))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return workerv1.NewWorkerServiceClient(conn), run.ID
}

func TestLeaseRunRoundTrip(t *testing.T) {
	c, id := setup(t)
	ctx := context.Background()
	resp, err := c.LeaseRun(ctx, &workerv1.LeaseRunRequest{WorkerId: "w1"})
	if err != nil || resp.GetRun().GetRunId() != id.String() || resp.GetRun().GetTicketTitle() != "T" {
		t.Fatalf("LeaseRun = %v, %v", resp, err)
	}
	resp, err = c.LeaseRun(ctx, &workerv1.LeaseRunRequest{WorkerId: "w1"})
	if err != nil || resp.GetRun() != nil {
		t.Fatalf("second LeaseRun = %v, %v", resp, err)
	}
}

func TestCompleteRunForeignWorkerPermissionDenied(t *testing.T) {
	c, id := setup(t)
	ctx := context.Background()
	if _, err := c.LeaseRun(ctx, &workerv1.LeaseRunRequest{WorkerId: "w1"}); err != nil {
		t.Fatal(err)
	}
	_, err := c.CompleteRun(ctx, &workerv1.CompleteRunRequest{RunId: id.String(), WorkerId: "evil", Outcome: workerv1.CompleteRunRequest_ANSWERED})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.CompleteRun(ctx, &workerv1.CompleteRunRequest{RunId: "x", WorkerId: "w1"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad id: %v", err)
	}
	_, err = c.CompleteRun(ctx, &workerv1.CompleteRunRequest{RunId: id.String(), WorkerId: "w1", Outcome: workerv1.CompleteRunRequest_ANSWERED, Text: "a"})
	if err != nil {
		t.Errorf("owner complete: %v", err)
	}
}

func TestWriteCredentialsForQuestionPermissionDenied(t *testing.T) {
	c, id := setup(t)
	ctx := context.Background()
	_, _ = c.LeaseRun(ctx, &workerv1.LeaseRunRequest{WorkerId: "w1"})
	_, err := c.GetRepoCredentials(ctx, &workerv1.GetRepoCredentialsRequest{RunId: id.String(), WorkerId: "w1", Write: true})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("err = %v", err)
	}
}

func TestRegisterChecksContractVersion(t *testing.T) {
	c, _ := setup(t)
	ctx := context.Background()
	if _, err := c.Register(ctx, &workerv1.RegisterRequest{WorkerId: "w", ContractVersion: "worker.v1"}); err != nil {
		t.Errorf("ok version: %v", err)
	}
	if _, err := c.Register(ctx, &workerv1.RegisterRequest{WorkerId: "w", ContractVersion: "v0"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("bad version: %v", err)
	}
}

func TestReportEventsStreamStoresAndDeniesForeign(t *testing.T) {
	c, id := setup(t)
	ctx := context.Background()
	_, _ = c.LeaseRun(ctx, &workerv1.LeaseRunRequest{WorkerId: "w1"})
	stream, err := c.ReportEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ev := &pluginv1.AgentEvent{Type: "message", Text: "hi"}
	_ = stream.Send(&workerv1.ReportEventsRequest{RunId: id.String(), WorkerId: "w1", Event: ev})
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatalf("owner stream: %v", err)
	}
	stream, _ = c.ReportEvents(ctx)
	_ = stream.Send(&workerv1.ReportEventsRequest{RunId: id.String(), WorkerId: "evil", Event: ev})
	if _, err := stream.CloseAndRecv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign stream: %v", err)
	}
}
