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

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
