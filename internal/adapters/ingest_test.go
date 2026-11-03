package adapters

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pluginv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/plugin/v1"
	"github.com/soemii/orchivio-server/internal/config"
)

type fakeTracker struct {
	pluginv1.UnimplementedTrackerAdapterServer
	mu    sync.Mutex
	calls int
}

func (f *fakeTracker) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	return &pluginv1.Capabilities{ContractVersion: "plugin.v1", AdapterName: "fake"}, nil
}

// First stream: two events then EOF. Second stream: one event, then blocks.
func (f *fakeTracker) StreamEvents(_ *pluginv1.StreamEventsRequest, s pluginv1.TrackerAdapter_StreamEventsServer) error {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	ids := []string{"e1", "e2"}
	if n > 1 {
		ids = []string{"e3"}
	}
	for _, id := range ids {
		if err := s.Send(&pluginv1.TrackerEvent{ExternalId: id}); err != nil {
			return err
		}
	}
	if n > 1 {
		<-s.Context().Done()
	}
	return nil
}

func TestIngestForwardsEventsAndReconnects(t *testing.T) {
	cfg, err := config.NewSync("../../dev/config", time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pluginv1.RegisterTrackerAdapterServer(srv, &fakeTracker{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := NewRegistry(cfg, log)
	reg.dialer = func(string) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	t.Cleanup(reg.Close)

	got := make(chan string, 3)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx, cfg, reg, func(_ context.Context, tenant string, ev *pluginv1.TrackerEvent) error {
			got <- tenant + "/" + ev.GetExternalId()
			return nil
		}, log, time.Millisecond, 10*time.Millisecond)
	}()

	for _, want := range []string{"acme/e1", "acme/e2", "acme/e3"} {
		select {
		case g := <-got:
			if g != want {
				t.Fatalf("got %s, want %s", g, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for %s", want)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ingest did not stop on cancel")
	}
}

type oldTracker struct{ fakeTracker }

func (*oldTracker) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	return &pluginv1.Capabilities{ContractVersion: "plugin.v0"}, nil
}

func TestRegistryRefusesWrongContractVersion(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := config.NewSync("../../dev/config", time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pluginv1.RegisterTrackerAdapterServer(srv, &oldTracker{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	reg := NewRegistry(cfg, log)
	reg.dialer = func(string) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	t.Cleanup(reg.Close)

	if _, err := reg.Tracker().GetTicket(context.Background(), "acme", "ORC-1"); err == nil {
		t.Fatal("expected refusal")
	}
	if _, err := reg.Tracker().GetTicket(context.Background(), "nope", "ORC-1"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown tenant error = %v", err)
	}
}
