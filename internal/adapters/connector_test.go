package adapters

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	"github.com/orchivio/orchestrator/internal/admin"
)

var connectRefresh = time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

// connectServer answers the connection RPCs; name tells tracker and code host apart.
type connectServer struct {
	pluginv1.UnimplementedTrackerAdapterServer
	pluginv1.UnimplementedCodeHostAdapterServer
	name        string
	completeErr error
	disconnects [][]byte
}

func (s *connectServer) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	return &pluginv1.Capabilities{ContractVersion: contractVersion, AdapterName: s.name}, nil
}

func (s *connectServer) BeginConnect(_ context.Context, r *pluginv1.BeginConnectRequest) (*pluginv1.BeginConnectResponse, error) {
	return &pluginv1.BeginConnectResponse{Url: s.name + "?" + r.GetTenant() + "&" + r.GetRedirectUri() + "&" + r.GetState()}, nil
}

func (s *connectServer) CompleteConnect(_ context.Context, r *pluginv1.CompleteConnectRequest) (*pluginv1.CompleteConnectResponse, error) {
	if s.completeErr != nil {
		return nil, s.completeErr
	}
	return &pluginv1.CompleteConnectResponse{Label: s.name + " " + r.GetQuery()["code"], ExternalId: r.GetRedirectUri(),
		Credential: &pluginv1.Credential{Data: []byte("cred"), RefreshAfter: timestamppb.New(connectRefresh)}}, nil
}

func (s *connectServer) RefreshCredential(_ context.Context, r *pluginv1.RefreshCredentialRequest) (*pluginv1.Credential, error) {
	return &pluginv1.Credential{Data: append(r.GetCredential().GetData(), '+')}, nil
}

func (s *connectServer) Disconnect(_ context.Context, r *pluginv1.DisconnectRequest) (*pluginv1.DisconnectResponse, error) {
	s.disconnects = append(s.disconnects, r.GetCredential().GetData())
	return &pluginv1.DisconnectResponse{}, nil
}

func (s *connectServer) CheckAccess(_ context.Context, r *pluginv1.CheckAccessRequest) (*pluginv1.CheckAccessResponse, error) {
	var denied []string
	for _, it := range r.GetItems() {
		if len(r.GetCredentials()) < 2 {
			denied = append(denied, it)
		}
	}
	return &pluginv1.CheckAccessResponse{Denied: denied}, nil
}

func newTestConnector(t *testing.T) (admin.Connector, *connectServer, *connectServer) {
	t.Helper()
	tr, ch := &connectServer{name: "linear"}, &connectServer{name: "github"}
	lisT, lisC := bufconn.Listen(1<<20), bufconn.Listen(1<<20)
	srvT, srvC := grpc.NewServer(), grpc.NewServer()
	pluginv1.RegisterTrackerAdapterServer(srvT, tr)
	pluginv1.RegisterCodeHostAdapterServer(srvC, ch)
	go func() { _ = srvT.Serve(lisT) }()
	go func() { _ = srvC.Serve(lisC) }()
	t.Cleanup(srvT.Stop)
	t.Cleanup(srvC.Stop)
	reg := NewRegistry(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reg.dialer = func(addr string) (*grpc.ClientConn, error) {
		lis := map[string]*bufconn.Listener{"adapter-linear:7101": lisT, "adapter-github:7201": lisC}[addr]
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	t.Cleanup(reg.Close)
	return reg.Connector(map[string]string{"linear": "adapter-linear:7101", "github": "adapter-github:7201"}), tr, ch
}

func TestConnectorRoutesByAdapterAndKind(t *testing.T) {
	c, _, ch := newTestConnector(t)
	ctx := context.Background()
	u, err := c.BeginConnect(ctx, "linear", admin.KindTracker, "acme", "http://cb", "st")
	if err != nil || u != "linear?acme&http://cb&st" {
		t.Fatalf("begin = %q, %v", u, err)
	}
	done, err := c.CompleteConnect(ctx, "github", admin.KindCodeHost, "http://cb", map[string]string{"code": "x"})
	if err != nil || done.Label != "github x" || done.ExternalID != "http://cb" || string(done.Data) != "cred" ||
		done.RefreshAfter == nil || !done.RefreshAfter.Equal(connectRefresh) {
		t.Fatalf("complete = %+v, %v", done, err)
	}
	refreshed, err := c.Refresh(ctx, "linear", admin.KindTracker, []byte("cred"), &connectRefresh)
	if err != nil || string(refreshed.Data) != "cred+" || refreshed.RefreshAfter != nil {
		t.Fatalf("refresh = %+v, %v", refreshed, err)
	}
	if err := c.Disconnect(ctx, "github", admin.KindCodeHost, []byte("cred")); err != nil || len(ch.disconnects) != 1 {
		t.Fatalf("disconnect: %v %q", err, ch.disconnects)
	}
	denied, err := c.CheckAccess(ctx, "github", admin.KindCodeHost, [][]byte{[]byte("a")}, []string{"o/x"})
	if err != nil || !slices.Equal(denied, []string{"o/x"}) {
		t.Fatalf("check = %v, %v", denied, err)
	}
	if _, err := c.BeginConnect(ctx, "jira", admin.KindTracker, "acme", "http://cb", "st"); err == nil {
		t.Fatal("unknown adapter accepted")
	}
}

func TestConnectorMapsAdapterErrors(t *testing.T) {
	c, _, ch := newTestConnector(t)
	ctx := context.Background()
	cases := []struct {
		code  codes.Code
		check func(error) bool
	}{
		{codes.Unauthenticated, func(err error) bool {
			var re *admin.RejectedError
			return errors.Is(err, admin.ErrCredentialRejected) && !errors.As(err, &re)
		}},
		{codes.PermissionDenied, func(err error) bool {
			var re *admin.RejectedError
			return errors.Is(err, admin.ErrCredentialRejected) && errors.As(err, &re) && re.Reason == "org owner required"
		}},
		{codes.Unavailable, func(err error) bool { return errors.Is(err, admin.ErrUnavailable) }},
		{codes.Internal, func(err error) bool { return errors.Is(err, admin.ErrAdapter) }},
		{codes.Unknown, func(err error) bool { return errors.Is(err, admin.ErrAdapter) }},
		{codes.FailedPrecondition, func(err error) bool {
			var nc *admin.NotConnectedError
			return errors.Is(err, admin.ErrNotConnected) && errors.As(err, &nc) && nc.Reason == "org owner required"
		}},
	}
	for _, tc := range cases {
		ch.completeErr = status.Error(tc.code, "org owner required")
		if _, err := c.CompleteConnect(ctx, "github", admin.KindCodeHost, "http://cb", nil); !tc.check(err) {
			t.Errorf("%v: err = %v", tc.code, err)
		}
	}
}
