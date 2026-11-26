package adapters

import (
	"context"
	"io"
	"log/slog"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	"github.com/orchivio/orchestrator/internal/config"
)

// recordingCodeHost records each StreamEvents call, sends one event and
// blocks until the stream's ctx ends.
type recordingCodeHost struct {
	pluginv1.UnimplementedCodeHostAdapterServer
	mu    sync.Mutex
	calls []*streamCall
}

func (f *recordingCodeHost) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	return &pluginv1.Capabilities{ContractVersion: "plugin.v1", AdapterName: "fake-host", SupportsReviewEvents: true}, nil
}

func (f *recordingCodeHost) StreamEvents(req *pluginv1.StreamCodeHostEventsRequest, s pluginv1.CodeHostAdapter_StreamEventsServer) error {
	c := &streamCall{keys: req.GetRepos(), cancelled: make(chan struct{})}
	for _, cr := range req.GetCredentials() {
		c.cred += string(cr.GetData())
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if err := s.Send(&pluginv1.CodeHostEvent{ExternalId: "ev-1"}); err != nil {
		return err
	}
	<-s.Context().Done()
	close(c.cancelled)
	return nil
}

func (f *recordingCodeHost) snapshot() []*streamCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*streamCall(nil), f.calls...)
}

func tenantWithRepos(repos ...string) *config.Config {
	t := testConfig().Tenants["acme"]
	t.CodeHostCredentials = [][]byte{[]byte("ch")}
	t.Projects = []config.Project{{Name: "demo", TrackerKey: "ORC", Milestones: config.DefaultMilestones()}}
	for _, r := range repos {
		t.Projects[0].Repos = append(t.Projects[0].Repos, config.Repo{Slug: r, BaseBranch: "main"})
	}
	return &config.Config{Tenants: map[string]config.Tenant{"acme": t}}
}

func TestCodeHostStreamRestartsOnRepoChange(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	src := &mutableSource{cfg: tenantWithRepos("g/b")}
	cfg, err := config.NewSync(context.Background(), src, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	host := &recordingCodeHost{}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pluginv1.RegisterCodeHostAdapterServer(srv, host)
	go func() { _ = srv.Serve(lis) }()
	reg := NewRegistry(cfg, log)
	reg.dialer = func(string) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	got := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runCodeHost(ctx, cfg, reg, func(_ context.Context, tenant string, ev *pluginv1.CodeHostEvent) error {
			got <- tenant + "/" + ev.GetExternalId()
			return nil
		}, log, time.Millisecond, 10*time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		reg.Close()
		srv.Stop()
	})
	eventually(t, "first stream", func() bool { return len(host.snapshot()) == 1 })
	if g := <-got; g != "acme/ev-1" {
		t.Fatalf("event = %s, want acme/ev-1", g)
	}

	reload(t, src, cfg, tenantWithRepos("g/b")) // unchanged: no restart
	reload(t, src, cfg, tenantWithRepos("g/b", "g/a"))

	eventually(t, "second stream with [g/a g/b]", func() bool {
		calls := host.snapshot()
		return len(calls) == 2 && slices.Equal(calls[1].keys, []string{"g/a", "g/b"})
	})
	eventually(t, "first stream cancelled", func() bool { return isClosed(host.snapshot()[0].cancelled) })
	if calls := host.snapshot(); !slices.Equal(calls[0].keys, []string{"g/b"}) || calls[0].cred != "ch" {
		t.Fatalf("first stream keys=%v cred=%q", calls[0].keys, calls[0].cred)
	}
}
