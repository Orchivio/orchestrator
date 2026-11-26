package conformance

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
)

const (
	knownTicket = "ORC-1"
	knownRepo   = "acme/app"
	doneStatus  = "Done"
)

var goodCredential = []byte("secret")

type fakeTracker struct {
	pluginv1.UnimplementedTrackerAdapterServer
	notFoundCode   codes.Code
	invalidCode    codes.Code // answer to the garbage credential; zero means Unauthenticated
	contractBroken bool
	nameless       bool
	alwaysChanged  bool
	streamErr      error // returned at once by StreamEvents when set
	statusSeen     atomic.Bool
}

func (f *fakeTracker) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	version, fieldName := "plugin.v1", "token"
	if f.contractBroken {
		version = "plugin.v0"
	}
	if f.nameless {
		fieldName = ""
	}
	return &pluginv1.Capabilities{
		ContractVersion:      version,
		AdapterName:          "fake",
		SupportsStatusUpdate: true,
		ConnectFields:        []*pluginv1.ConnectField{{Name: fieldName}},
	}, nil
}

func (f *fakeTracker) GetTicket(_ context.Context, req *pluginv1.GetTicketRequest) (*pluginv1.Ticket, error) {
	if len(req.GetCredential().GetData()) == 0 {
		return nil, status.Error(codes.Unauthenticated, "no credential")
	}
	if string(req.GetCredential().GetData()) == string(invalidCredential) {
		if f.invalidCode != codes.OK {
			return nil, status.Error(f.invalidCode, "bad credential")
		}
		return nil, status.Error(codes.Unauthenticated, "bad credential")
	}
	if req.GetTicketRef() != knownTicket {
		return nil, status.Error(f.notFoundCode, "unknown ticket")
	}
	return &pluginv1.Ticket{TicketRef: knownTicket}, nil
}

func (f *fakeTracker) SetStatus(context.Context, *pluginv1.SetStatusRequest) (*pluginv1.SetStatusResponse, error) {
	changed := f.alwaysChanged || !f.statusSeen.Swap(true)
	return &pluginv1.SetStatusResponse{Changed: changed}, nil
}

func (f *fakeTracker) StreamEvents(_ *pluginv1.StreamEventsRequest, stream grpc.ServerStreamingServer[pluginv1.TrackerEvent]) error {
	if f.streamErr != nil {
		return f.streamErr
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func serve(t *testing.T, register func(*grpc.Server)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	register(server)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

func trackerTarget(t *testing.T, fake *fakeTracker) Target {
	addr := serve(t, func(s *grpc.Server) { pluginv1.RegisterTrackerAdapterServer(s, fake) })
	return Target{Addr: addr, Kind: "tracker", Credential: goodCredential, Ticket: knownTicket, Status: doneStatus}
}

func checks(failures []Failure) []string {
	names := make([]string, 0, len(failures))
	for _, failure := range failures {
		names = append(names, failure.Check)
	}
	return names
}

func TestRunPassesConformingFake(t *testing.T) {
	target := trackerTarget(t, &fakeTracker{notFoundCode: codes.NotFound})

	failures := Run(context.Background(), target)

	if len(failures) != 0 {
		t.Fatalf("expected conform, got %v", failures)
	}
}

func TestRunReportsWrongErrorCode(t *testing.T) {
	target := trackerTarget(t, &fakeTracker{notFoundCode: codes.Internal})

	failures := Run(context.Background(), target)

	if got := checks(failures); len(got) != 1 || got[0] != "unknown-ticket" {
		t.Fatalf("expected only unknown-ticket failure, got %v", failures)
	}
}

func TestRunReportsHangingStream(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	oldOptions, oldDeadline := dialOptions, cancelDeadline
	t.Cleanup(func() { dialOptions, cancelDeadline = oldOptions, oldDeadline })
	cancelDeadline = 50 * time.Millisecond
	dialOptions = []grpc.DialOption{grpc.WithChainStreamInterceptor(hangingRecv(release))}
	target := trackerTarget(t, &fakeTracker{notFoundCode: codes.NotFound})

	failures := Run(context.Background(), target)

	if got := checks(failures); len(got) != 1 || got[0] != "stream-ends-on-cancel" {
		t.Fatalf("expected only stream-cancel failure, got %v", failures)
	}
}

func assertOnly(t *testing.T, failures []Failure, check string) {
	t.Helper()
	if got := checks(failures); len(got) != 1 || got[0] != check {
		t.Fatalf("expected only %s failure, got %v", check, failures)
	}
}

func TestRunReportsImmediateStreamError(t *testing.T) {
	target := trackerTarget(t, &fakeTracker{notFoundCode: codes.NotFound, streamErr: status.Error(codes.Internal, "boom")})

	assertOnly(t, Run(context.Background(), target), "stream-ends-on-cancel")
}

func TestRunReportsInvalidCredentialAccepted(t *testing.T) {
	target := trackerTarget(t, &fakeTracker{notFoundCode: codes.NotFound, invalidCode: codes.Internal})

	assertOnly(t, Run(context.Background(), target), "invalid-credential")
}

func TestRunReportsNonIdempotentSetStatus(t *testing.T) {
	target := trackerTarget(t, &fakeTracker{notFoundCode: codes.NotFound, alwaysChanged: true})

	assertOnly(t, Run(context.Background(), target), "set-status")
}

func TestRunNotifiesWhenStatusMissing(t *testing.T) {
	target := trackerTarget(t, &fakeTracker{notFoundCode: codes.NotFound})
	target.Status = ""
	var notes []string
	target.Notify = func(message string) { notes = append(notes, message) }

	failures := Run(context.Background(), target)

	if len(failures) != 0 || len(notes) != 1 {
		t.Fatalf("expected no failures and one note, got %v / %v", failures, notes)
	}
}

func TestRunReportsContractVersionMismatch(t *testing.T) {
	target := trackerTarget(t, &fakeTracker{notFoundCode: codes.NotFound, contractBroken: true})

	assertOnly(t, Run(context.Background(), target), "contract-version")
}

func TestRunReportsNamelessConnectField(t *testing.T) {
	target := trackerTarget(t, &fakeTracker{notFoundCode: codes.NotFound, nameless: true})

	assertOnly(t, Run(context.Background(), target), "connect-fields")
}

// hangingRecv simulates a client stream whose Recv ignores cancellation until released.
func hangingRecv(release <-chan struct{}) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		stream, err := streamer(ctx, desc, cc, method, opts...)
		return &hangingStream{ClientStream: stream, release: release}, err
	}
}

type hangingStream struct {
	grpc.ClientStream
	release <-chan struct{}
}

func (h *hangingStream) RecvMsg(any) error {
	<-h.release
	return context.Canceled
}

type fakeCodeHost struct {
	pluginv1.UnimplementedCodeHostAdapterServer
	withToken bool
}

func (f *fakeCodeHost) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	return &pluginv1.Capabilities{ContractVersion: "plugin.v1", AdapterName: "fake"}, nil
}

func (f *fakeCodeHost) IssueRepoCredentials(_ context.Context, req *pluginv1.IssueRepoCredentialsRequest) (*pluginv1.RepoCredentials, error) {
	if len(req.GetCredentials()) == 0 || len(req.GetCredentials()[0].GetData()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no credential")
	}
	if string(req.GetCredentials()[0].GetData()) == string(invalidCredential) {
		return nil, status.Error(codes.Unauthenticated, "bad credential")
	}
	if req.GetRepos()[0] != knownRepo {
		return nil, status.Error(codes.NotFound, "unknown repo")
	}
	if !f.withToken {
		return &pluginv1.RepoCredentials{}, nil
	}
	return &pluginv1.RepoCredentials{Tokens: map[string]*pluginv1.RepoToken{knownRepo: {Token: "t"}}}, nil
}

func (f *fakeCodeHost) StreamEvents(_ *pluginv1.StreamCodeHostEventsRequest, stream grpc.ServerStreamingServer[pluginv1.CodeHostEvent]) error {
	<-stream.Context().Done()
	return stream.Context().Err()
}

func runCodeHostTarget(t *testing.T, withToken bool) []Failure {
	addr := serve(t, func(s *grpc.Server) {
		pluginv1.RegisterCodeHostAdapterServer(s, &fakeCodeHost{withToken: withToken})
	})
	return Run(context.Background(), Target{Addr: addr, Kind: "codehost", Credential: goodCredential, Repo: knownRepo})
}

func TestRunPassesConformingCodeHost(t *testing.T) {
	failures := runCodeHostTarget(t, true)

	if len(failures) != 0 {
		t.Fatalf("expected conform, got %v", failures)
	}
}

func TestRunReportsMissingRepoToken(t *testing.T) {
	assertOnly(t, runCodeHostTarget(t, false), "repo-credentials")
}
