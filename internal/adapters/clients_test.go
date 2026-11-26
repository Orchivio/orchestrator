package adapters

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	"github.com/orchivio/orchestrator/internal/config"
)

// credRecorder records the credential data each adapter call carried.
type credRecorder struct {
	mu    sync.Mutex
	calls []string // "Method:cred1,cred2"
}

func (r *credRecorder) record(method string, creds ...*pluginv1.Credential) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := method + ":"
	for i, c := range creds {
		if i > 0 {
			call += ","
		}
		call += string(c.GetData())
	}
	r.calls = append(r.calls, call)
}

type recTracker struct {
	pluginv1.UnimplementedTrackerAdapterServer
	*credRecorder
}

type recCodeHost struct {
	pluginv1.UnimplementedCodeHostAdapterServer
	*credRecorder
}

func (recTracker) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	return &pluginv1.Capabilities{ContractVersion: contractVersion}, nil
}

func (recCodeHost) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	return &pluginv1.Capabilities{ContractVersion: contractVersion}, nil
}

func (r recTracker) GetTicket(_ context.Context, req *pluginv1.GetTicketRequest) (*pluginv1.Ticket, error) {
	r.record("GetTicket", req.GetCredential())
	return &pluginv1.Ticket{}, nil
}

func (r recTracker) PostComment(_ context.Context, req *pluginv1.PostCommentRequest) (*pluginv1.PostCommentResponse, error) {
	r.record("PostComment", req.GetCredential())
	return &pluginv1.PostCommentResponse{}, nil
}

func (r recCodeHost) IssueRepoCredentials(_ context.Context, req *pluginv1.IssueRepoCredentialsRequest) (*pluginv1.RepoCredentials, error) {
	r.record("IssueRepoCredentials", req.GetCredentials()...)
	return &pluginv1.RepoCredentials{}, nil
}

func (r recCodeHost) OpenPullRequest(_ context.Context, req *pluginv1.OpenPullRequestRequest) (*pluginv1.PullRequest, error) {
	r.record("OpenPullRequest", req.GetCredentials()...)
	return &pluginv1.PullRequest{}, nil
}

func TestRegistryAttachesTenantCredentials(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := testConfig()
	acme := c.Tenants["acme"]
	acme.TrackerCredential = []byte("trk")
	acme.CodeHostCredentials = [][]byte{[]byte("ch1"), []byte("ch2")}
	c.Tenants["acme"] = acme
	bare := acme
	bare.Name, bare.TrackerCredential, bare.CodeHostCredentials = "bare", nil, nil
	c.Tenants["bare"] = bare
	cfg, err := config.NewSync(context.Background(), config.Static{Config: c}, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	rec := &credRecorder{}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pluginv1.RegisterTrackerAdapterServer(srv, recTracker{credRecorder: rec})
	pluginv1.RegisterCodeHostAdapterServer(srv, recCodeHost{credRecorder: rec})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	reg := NewRegistry(cfg, log)
	reg.dialer = func(string) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	t.Cleanup(reg.Close)
	ctx := context.Background()

	_, err1 := reg.Tracker().GetTicket(ctx, "acme", "ORC-1")
	err2 := reg.TrackerFor("acme").PostComment(ctx, "ORC-1", "hi")
	_, err3 := reg.CodeHost().IssueRepoCredentials(ctx, "acme", []string{"o/x"}, true)
	_, err4 := reg.CodeHost().OpenPullRequest(ctx, "acme", &pluginv1.OpenPullRequestRequest{Repo: "o/x"})
	if err := fmt.Sprint(err1, err2, err3, err4); err != "<nil> <nil> <nil> <nil>" {
		t.Fatal(err)
	}
	want := "[GetTicket:trk PostComment:trk IssueRepoCredentials:ch1,ch2 OpenPullRequest:ch1,ch2]"
	if got := fmt.Sprint(rec.calls); got != want {
		t.Fatalf("calls = %s, want %s", got, want)
	}

	if _, err := reg.Tracker().GetTicket(ctx, "bare", "ORC-1"); err == nil {
		t.Fatal("GetTicket without tracker connection succeeded")
	}
	if _, err := reg.CodeHost().IssueRepoCredentials(ctx, "bare", []string{"o/x"}, false); err == nil {
		t.Fatal("IssueRepoCredentials without code host connection succeeded")
	}
	if len(rec.calls) != 4 {
		t.Fatalf("adapter called without credential: %v", rec.calls)
	}
}

// rejectingTracker answers every call with code (OK = success).
type rejectingTracker struct {
	pluginv1.UnimplementedTrackerAdapterServer
	code codes.Code
}

func (rejectingTracker) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	return &pluginv1.Capabilities{ContractVersion: contractVersion}, nil
}

func (r rejectingTracker) GetTicket(context.Context, *pluginv1.GetTicketRequest) (*pluginv1.Ticket, error) {
	return nil, status.Error(r.code, "nope")
}

func (r rejectingTracker) StreamEvents(*pluginv1.StreamEventsRequest, pluginv1.TrackerAdapter_StreamEventsServer) error {
	return status.Error(r.code, "nope")
}

type rejectedCall struct {
	tenant string
	id     uuid.UUID
}

// rejectEnv is a registry over a tracker answering code; hook calls land in rejected.
func rejectEnv(t *testing.T, code codes.Code, mutate func(*config.Tenant)) (*Registry, chan rejectedCall) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := testConfig()
	acme := c.Tenants["acme"]
	acme.TrackerConnectionID = uuid.New()
	acme.CodeHostCredentials = [][]byte{[]byte("ch1")}
	acme.CodeHostConnectionIDs = []uuid.UUID{uuid.New()}
	if mutate != nil {
		mutate(&acme)
	}
	c.Tenants["acme"] = acme
	cfg, err := config.NewSync(context.Background(), config.Static{Config: c}, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pluginv1.RegisterTrackerAdapterServer(srv, rejectingTracker{code: code})
	pluginv1.RegisterCodeHostAdapterServer(srv, rejectingCodeHost{code: code})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	reg := NewRegistry(cfg, log)
	reg.dialer = func(string) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	t.Cleanup(reg.Close)
	rejected := make(chan rejectedCall, 4)
	reg.OnRejected(func(_ context.Context, tenant string, id uuid.UUID) { rejected <- rejectedCall{tenant, id} })
	return reg, rejected
}

type rejectingCodeHost struct {
	pluginv1.UnimplementedCodeHostAdapterServer
	code codes.Code
}

func (rejectingCodeHost) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	return &pluginv1.Capabilities{ContractVersion: contractVersion}, nil
}

func (r rejectingCodeHost) IssueRepoCredentials(context.Context, *pluginv1.IssueRepoCredentialsRequest) (*pluginv1.RepoCredentials, error) {
	return nil, status.Error(r.code, "nope")
}

func TestUnauthenticatedTrackerCallMarksNeedsReauth(t *testing.T) {
	reg, rejected := rejectEnv(t, codes.Unauthenticated, nil)
	want := reg.cfg.Current().Tenants["acme"].TrackerConnectionID

	_, err := reg.Tracker().GetTicket(context.Background(), "acme", "ORC-1")

	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v, want the adapter's Unauthenticated", err)
	}
	if got := <-rejected; got != (rejectedCall{"acme", want}) {
		t.Fatalf("hook = %+v, want acme/%s", got, want)
	}
}

func TestUnauthenticatedStreamMarksNeedsReauth(t *testing.T) {
	reg, rejected := rejectEnv(t, codes.Unauthenticated, nil)
	want := reg.cfg.Current().Tenants["acme"].TrackerConnectionID
	handle := func(context.Context, string, *pluginv1.TrackerEvent) error { return nil }

	_, err := streamOnce(context.Background(), context.Background(), reg, "acme", []string{"ORC"}, handle, reg.log)

	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v", err)
	}
	if got := <-rejected; got.id != want {
		t.Fatalf("hook id = %s, want %s", got.id, want)
	}
}

func TestOtherErrorsKeepConnectionActive(t *testing.T) {
	reg, rejected := rejectEnv(t, codes.Unavailable, nil)

	_, err := reg.Tracker().GetTicket(context.Background(), "acme", "ORC-1")

	if status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v", err)
	}
	if len(rejected) != 0 {
		t.Fatal("hook ran for Unavailable")
	}
}

func TestUnauthenticatedCodeHostCallMarksOnlyWithSingleCredential(t *testing.T) {
	reg, rejected := rejectEnv(t, codes.Unauthenticated, nil)
	want := reg.cfg.Current().Tenants["acme"].CodeHostConnectionIDs[0]
	if _, err := reg.CodeHost().IssueRepoCredentials(context.Background(), "acme", []string{"o/x"}, false); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v", err)
	}
	if got := <-rejected; got.id != want {
		t.Fatalf("hook id = %s, want %s", got.id, want)
	}

	multi, multiRejected := rejectEnv(t, codes.Unauthenticated, func(tn *config.Tenant) {
		tn.CodeHostCredentials = [][]byte{[]byte("ch1"), []byte("ch2")}
		tn.CodeHostConnectionIDs = []uuid.UUID{uuid.New(), uuid.New()}
	})
	if _, err := multi.CodeHost().IssueRepoCredentials(context.Background(), "acme", []string{"o/x"}, false); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v", err)
	}
	if len(multiRejected) != 0 {
		t.Fatal("hook ran although two credentials were sent")
	}
}

// statusTracker supports (or not) status updates and records SetStatus calls.
type statusTracker struct {
	pluginv1.UnimplementedTrackerAdapterServer
	supported bool
	calls     *[]string
	code      codes.Code
}

func (s statusTracker) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	return &pluginv1.Capabilities{ContractVersion: contractVersion, SupportsStatusUpdate: s.supported}, nil
}

func (s statusTracker) SetStatus(_ context.Context, req *pluginv1.SetStatusRequest) (*pluginv1.SetStatusResponse, error) {
	*s.calls = append(*s.calls, req.GetTicketRef()+"|"+req.GetStatus()+"|"+string(req.GetCredential().GetData()))
	if s.code != codes.OK {
		return nil, status.Error(s.code, "nope")
	}
	return &pluginv1.SetStatusResponse{Changed: true}, nil
}

func statusEnv(t *testing.T, srvImpl statusTracker) (*Registry, chan rejectedCall) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := testConfig()
	acme := c.Tenants["acme"]
	acme.TrackerCredential = []byte("trk")
	acme.TrackerConnectionID = uuid.New()
	c.Tenants["acme"] = acme
	cfg, err := config.NewSync(context.Background(), config.Static{Config: c}, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pluginv1.RegisterTrackerAdapterServer(srv, srvImpl)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	reg := NewRegistry(cfg, log)
	reg.dialer = func(string) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	t.Cleanup(reg.Close)
	rejected := make(chan rejectedCall, 4)
	reg.OnRejected(func(_ context.Context, tenant string, id uuid.UUID) { rejected <- rejectedCall{tenant, id} })
	return reg, rejected
}

func TestSetStatusCallsAdapterWhenSupported(t *testing.T) {
	var calls []string
	reg, _ := statusEnv(t, statusTracker{supported: true, calls: &calls})
	if err := reg.Tracker().SetStatus(context.Background(), "acme", "ORC-1", "In Review"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(calls); got != "[ORC-1|In Review|trk]" {
		t.Fatalf("calls = %s", got)
	}
}

func TestSetStatusIsNoopWithoutCapability(t *testing.T) {
	var calls []string
	reg, _ := statusEnv(t, statusTracker{supported: false, calls: &calls})
	if err := reg.Tracker().SetStatus(context.Background(), "acme", "ORC-1", "In Review"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("adapter called: %v", calls)
	}
}

func TestSetStatusUnauthenticatedMarksConnection(t *testing.T) {
	var calls []string
	reg, rejected := statusEnv(t, statusTracker{supported: true, calls: &calls, code: codes.Unauthenticated})
	if err := reg.Tracker().SetStatus(context.Background(), "acme", "ORC-1", "In Review"); err == nil {
		t.Fatal("want error")
	}
	if r := <-rejected; r.tenant != "acme" {
		t.Fatalf("rejected = %v", r)
	}
}

func TestRejectedHookRunsDetachedWithDeadline(t *testing.T) {
	reg, _ := rejectEnv(t, codes.Unauthenticated, nil)
	bounded := make(chan bool, 1)
	reg.OnRejected(func(ctx context.Context, _ string, _ uuid.UUID) {
		_, ok := ctx.Deadline()
		bounded <- ok && ctx.Err() == nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, _ = reg.Tracker().GetTicket(ctx, "acme", "ORC-1")

	if !<-bounded {
		t.Fatal("hook context must carry a deadline and be live")
	}
}
