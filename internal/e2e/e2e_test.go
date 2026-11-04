//go:build e2e

// Package e2e runs the server core in-process against fake adapters and a
// fake worker. Needs Docker/Podman for the Postgres testcontainer.
package e2e

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pluginv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/plugin/v1"
	workerv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/worker/v1"
	"github.com/soemii/orchivio-server/internal/adapters"
	"github.com/soemii/orchivio-server/internal/config"
	"github.com/soemii/orchivio-server/internal/grpcapi"
	"github.com/soemii/orchivio-server/internal/orchestrator"
	"github.com/soemii/orchivio-server/internal/reporter"
	"github.com/soemii/orchivio-server/internal/router"
	"github.com/soemii/orchivio-server/internal/store"
)

const (
	publicURL = "http://timeline.test"
	userEmail = "dev@example.com"
	workerID  = "w1"
	repoSlug  = "acme/playground"
	waitMax   = 5 * time.Second
)

const configYAML = `tenant: acme
tracker: { name: linear, address: tracker }
codeHost: { name: github, address: codehost }
profiles:
  default: { runtime: claude-code, model: m, maxDuration: 20m }
projects:
  - name: demo
    trackerKey: ORC
    profile: default
    repos:
      - { slug: acme/playground, baseBranch: main, allowedPaths: [""] }
    classifier:
      taskLabels: ["orchivio:aufgabe"]
      questionLabels: ["orchivio:frage"]
      defaultRepo: acme/playground
    roles: { questions: ["dev@example.com"], tasks: ["dev@example.com"] }
    maxDiffLines: 500
`

type fakeTracker struct {
	pluginv1.UnimplementedTrackerAdapterServer
	events   chan *pluginv1.TrackerEvent
	mu       sync.Mutex
	comments []string
}

func (f *fakeTracker) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	return &pluginv1.Capabilities{ContractVersion: "plugin.v1", AdapterName: "fake-tracker"}, nil
}

func (f *fakeTracker) StreamEvents(_ *pluginv1.StreamEventsRequest, s grpc.ServerStreamingServer[pluginv1.TrackerEvent]) error {
	for {
		select {
		case <-s.Context().Done():
			return nil
		case ev := <-f.events:
			if err := s.Send(ev); err != nil {
				return err
			}
		}
	}
}

func (f *fakeTracker) GetTicket(_ context.Context, r *pluginv1.GetTicketRequest) (*pluginv1.Ticket, error) {
	return &pluginv1.Ticket{TicketRef: r.GetTicketRef(), Title: "Titel " + r.GetTicketRef()}, nil
}

func (f *fakeTracker) PostComment(_ context.Context, r *pluginv1.PostCommentRequest) (*pluginv1.PostCommentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, r.GetTicketRef()+"|"+r.GetBody())
	return &pluginv1.PostCommentResponse{}, nil
}

func (f *fakeTracker) commentsFor(ref string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.comments {
		if strings.HasPrefix(c, ref+"|") {
			out = append(out, c)
		}
	}
	return out
}

type fakeCodeHost struct {
	pluginv1.UnimplementedCodeHostAdapterServer
	mu  sync.Mutex
	prs []*pluginv1.OpenPullRequestRequest
}

func (f *fakeCodeHost) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	return &pluginv1.Capabilities{ContractVersion: "plugin.v1", AdapterName: "fake-codehost"}, nil
}

func (f *fakeCodeHost) OpenPullRequest(_ context.Context, r *pluginv1.OpenPullRequestRequest) (*pluginv1.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs = append(f.prs, r)
	return &pluginv1.PullRequest{Url: "https://github.test/acme/playground/pull/7", Number: 7}, nil
}

func (f *fakeCodeHost) opened() []*pluginv1.OpenPullRequestRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pluginv1.OpenPullRequestRequest(nil), f.prs...)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(waitMax)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

func serve(t *testing.T, register func(*grpc.Server)) *bufconn.Listener {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis
}

func dial(t *testing.T, lis *bufconn.Listener) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

func startPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("orchivio"), tcpostgres.WithUsername("u"), tcpostgres.WithPassword("p"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })
	url, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	return url
}

type env struct {
	tracker  *fakeTracker
	codeHost *fakeCodeHost
	db       *store.Store
	worker   workerv1.WorkerServiceClient
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "acme.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewSync(dir, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, startPostgres(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	ft := &fakeTracker{events: make(chan *pluginv1.TrackerEvent, 8)}
	fc := &fakeCodeHost{}
	lisT := serve(t, func(s *grpc.Server) { pluginv1.RegisterTrackerAdapterServer(s, ft) })
	lisC := serve(t, func(s *grpc.Server) { pluginv1.RegisterCodeHostAdapterServer(s, fc) })

	reg := adapters.NewRegistry(cfg, log)
	t.Cleanup(reg.Close)
	reg.SetDialer(func(addr string) (*grpc.ClientConn, error) {
		if addr == "tracker" {
			return dial(t, lisT), nil
		}
		return dial(t, lisC), nil
	})
	rep := reporter.New(func(tn string) reporter.TrackerClient { return reg.TrackerFor(tn) }, cfg, publicURL)
	rt := router.New(cfg, db, reg.Tracker(), rep, time.Now, uuid.New)
	orch := orchestrator.New(cfg, db, reg.CodeHost(), reg.Tracker(), rep, rt.StartFollowUp, time.Now, time.Minute, publicURL)
	lisW := serve(t, func(s *grpc.Server) { workerv1.RegisterWorkerServiceServer(s, grpcapi.NewWorker(orch)) })

	done := make(chan struct{})
	go func() { defer close(done); adapters.Run(ctx, cfg, reg, rt.Handle, log) }()
	t.Cleanup(func() { cancel(); <-done })

	return &env{tracker: ft, codeHost: fc, db: db, worker: workerv1.NewWorkerServiceClient(dial(t, lisW))}
}

func (e *env) send(kind pluginv1.TrackerEvent_Kind, ref, body string, mention bool) {
	e.tracker.events <- &pluginv1.TrackerEvent{
		ExternalId: uuid.NewString(), Kind: kind, TicketRef: ref, ProjectKey: "ORC",
		Actor: &pluginv1.Actor{Id: "u1", Email: userEmail}, Body: body, MentionsBot: mention,
	}
}

func (e *env) lease(t *testing.T) *workerv1.RunAssignment {
	t.Helper()
	var a *workerv1.RunAssignment
	eventually(t, "queued run", func() bool {
		resp, err := e.worker.LeaseRun(context.Background(), &workerv1.LeaseRunRequest{WorkerId: workerID})
		if err != nil {
			t.Fatal(err)
		}
		a = resp.GetRun()
		return a != nil
	})
	return a
}

func (e *env) reportAndComplete(t *testing.T, a *workerv1.RunAssignment, req *workerv1.CompleteRunRequest) {
	t.Helper()
	ctx := context.Background()
	stream, err := e.worker.ReportEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"run.started", "message", "run.finished"} {
		if err := stream.Send(&workerv1.ReportEventsRequest{WorkerId: workerID, RunId: a.GetRunId(), Event: &pluginv1.AgentEvent{Type: typ, Text: "x"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
	req.RunId, req.WorkerId = a.GetRunId(), workerID
	if _, err := e.worker.CompleteRun(ctx, req); err != nil {
		t.Fatal(err)
	}
}

func hasComment(e *env, ref, substr string) func() bool {
	return func() bool {
		for _, c := range e.tracker.commentsFor(ref) {
			if strings.Contains(c, substr) {
				return true
			}
		}
		return false
	}
}

func TestEndToEndWithFakes(t *testing.T) {
	e := setup(t)

	t.Run("question gets an answer comment with timeline link", func(t *testing.T) {
		e.send(pluginv1.TrackerEvent_COMMENTED, "ORC-1", "@Orchivio Was steht in der README?", true)
		a := e.lease(t)
		if a.GetKind() != "question" {
			t.Fatalf("kind = %q, want question", a.GetKind())
		}
		e.reportAndComplete(t, a, &workerv1.CompleteRunRequest{Outcome: workerv1.CompleteRunRequest_ANSWERED, Text: "Die README ist leer."})
		eventually(t, "answer comment", hasComment(e, "ORC-1", "Die README ist leer."))
		eventually(t, "timeline link", hasComment(e, "ORC-1", publicURL+"/runs/"+a.GetRunId()))
	})

	t.Run("task opens a pull request and links it in the ticket", func(t *testing.T) {
		e.send(pluginv1.TrackerEvent_ASSIGNED, "ORC-2", "", false)
		a := e.lease(t)
		wantBranch := "orchivio/orc-2-" + a.GetRunId()[:8]
		if a.GetKind() != "task" || a.GetBranch() != wantBranch {
			t.Fatalf("kind=%q branch=%q, want task %q", a.GetKind(), a.GetBranch(), wantBranch)
		}
		e.reportAndComplete(t, a, &workerv1.CompleteRunRequest{
			Outcome: workerv1.CompleteRunRequest_PUSHED, Text: "Kontakt ergänzt.",
			PushedBranches: map[string]string{repoSlug: a.GetBranch()},
		})
		eventually(t, "PR comment", hasComment(e, "ORC-2", "https://github.test/acme/playground/pull/7"))
		prs := e.codeHost.opened()
		if len(prs) != 1 || prs[0].GetRepo() != repoSlug || prs[0].GetHead() != wantBranch || prs[0].GetBase() != "main" {
			t.Fatalf("OpenPullRequest calls = %v", prs)
		}
	})

	t.Run("comment during running run starts a follow-up run after completion", func(t *testing.T) {
		e.send(pluginv1.TrackerEvent_COMMENTED, "ORC-3", "@Orchivio Erste Frage", true)
		first := e.lease(t)
		e.send(pluginv1.TrackerEvent_COMMENTED, "ORC-3", "Und noch eine Frage", false)
		eventually(t, "open input", func() bool {
			conv, err := e.db.FindConversation(context.Background(), "acme", "ORC-3")
			if err != nil || conv == nil {
				return false
			}
			in, err := e.db.OpenInputs(context.Background(), conv.ID)
			return err == nil && len(in) == 1
		})
		resp, err := e.worker.LeaseRun(context.Background(), &workerv1.LeaseRunRequest{WorkerId: workerID})
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetRun() != nil {
			t.Fatalf("second run leased while first still running: %s", resp.GetRun().GetRunId())
		}
		e.reportAndComplete(t, first, &workerv1.CompleteRunRequest{Outcome: workerv1.CompleteRunRequest_ANSWERED, Text: "Antwort eins."})
		second := e.lease(t)
		if second.GetRunId() == first.GetRunId() || !strings.Contains(second.GetSpec().GetPrompt(), "Und noch eine Frage") {
			t.Fatalf("follow-up run %s prompt lacks second comment", second.GetRunId())
		}
	})
}
