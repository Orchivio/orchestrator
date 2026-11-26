package adapters

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	"github.com/orchivio/orchestrator/internal/config"
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
	cfg, err := config.NewSync(context.Background(), config.Static{Config: testConfig()}, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	cfg, err := config.NewSync(context.Background(), config.Static{Config: testConfig()}, time.Hour, log)
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

func TestIngestContinuesAfterHandlerError(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := config.NewSync(context.Background(), config.Static{Config: testConfig()}, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pluginv1.RegisterTrackerAdapterServer(srv, &fakeTracker{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	reg := NewRegistry(cfg, log)
	reg.dialer = func(string) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	t.Cleanup(reg.Close)

	got := make(chan string, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go run(ctx, cfg, reg, func(_ context.Context, _ string, ev *pluginv1.TrackerEvent) error {
		got <- ev.GetExternalId()
		return errors.New("boom")
	}, log, time.Millisecond, 10*time.Millisecond)
	for _, want := range []string{"e1", "e2", "e3"} {
		select {
		case g := <-got:
			if g != want {
				t.Fatalf("got %s, want %s", g, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for %s", want)
		}
	}
}

func testConfig() *config.Config {
	return &config.Config{Tenants: map[string]config.Tenant{"acme": {
		Name:              "acme",
		Tracker:           config.AdapterRef{Name: "linear", Address: "adapter-linear:7101"},
		CodeHost:          config.AdapterRef{Name: "github", Address: "adapter-github:7201"},
		Projects:          []config.Project{{Name: "demo", TrackerKey: "ORC", Milestones: config.DefaultMilestones()}},
		TrackerCredential: []byte("trk"),
	}}}
}

// mutableSource is a config.Source whose config the test can swap.
type mutableSource struct {
	mu  sync.Mutex
	cfg *config.Config
}

func (s *mutableSource) Load(context.Context) (*config.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg, nil
}

func (s *mutableSource) set(c *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = c
}

type streamCall struct {
	keys      []string
	cred      string
	cancelled chan struct{}
}

// recordingTracker records each StreamEvents call and blocks until the
// stream's ctx ends.
type recordingTracker struct {
	pluginv1.UnimplementedTrackerAdapterServer
	mu    sync.Mutex
	calls []*streamCall
	event *pluginv1.TrackerEvent // sent on every stream when set
}

func (f *recordingTracker) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	return &pluginv1.Capabilities{ContractVersion: "plugin.v1", AdapterName: "fake"}, nil
}

func (f *recordingTracker) StreamEvents(req *pluginv1.StreamEventsRequest, s pluginv1.TrackerAdapter_StreamEventsServer) error {
	c := &streamCall{keys: req.GetProjectKeys(), cred: string(req.GetCredential().GetData()), cancelled: make(chan struct{})}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.event != nil {
		if err := s.Send(f.event); err != nil {
			return err
		}
	}
	<-s.Context().Done()
	close(c.cancelled)
	return nil
}

func (f *recordingTracker) snapshot() []*streamCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*streamCall(nil), f.calls...)
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

func tenantWith(keys ...string) *config.Config {
	t := testConfig().Tenants["acme"]
	t.Projects = nil
	for _, k := range keys {
		t.Projects = append(t.Projects, config.Project{Name: strings.ToLower(k), TrackerKey: k, Milestones: config.DefaultMilestones()})
	}
	return &config.Config{Tenants: map[string]config.Tenant{"acme": t}}
}

// startIngest runs the ingest supervisor against a recordingTracker and stops
// it (waiting for Run to return) on cleanup.
func startIngest(t *testing.T, initial *config.Config) (*mutableSource, *config.Sync, *recordingTracker) {
	t.Helper()
	return startIngestWith(t, initial, &recordingTracker{},
		func(context.Context, string, *pluginv1.TrackerEvent) error { return nil })
}

func startIngestWith(t *testing.T, initial *config.Config, tr *recordingTracker,
	handle func(context.Context, string, *pluginv1.TrackerEvent) error) (*mutableSource, *config.Sync, *recordingTracker) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	src := &mutableSource{cfg: initial}
	cfg, err := config.NewSync(context.Background(), src, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pluginv1.RegisterTrackerAdapterServer(srv, tr)
	go func() { _ = srv.Serve(lis) }()
	reg := NewRegistry(cfg, log)
	reg.dialer = func(string) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx, cfg, reg, handle,
			log, time.Millisecond, 10*time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("ingest did not stop on cancel")
		}
		reg.Close()
		srv.Stop()
	})
	return src, cfg, tr
}

func reload(t *testing.T, src *mutableSource, cfg *config.Sync, c *config.Config) {
	t.Helper()
	src.set(c)
	if err := cfg.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestIngestStartsStreamForTenantAddedAtRuntime(t *testing.T) {
	src, cfg, tr := startIngest(t, nil)

	reload(t, src, cfg, tenantWith("RES"))

	eventually(t, "stream with [RES]", func() bool {
		calls := tr.snapshot()
		return len(calls) == 1 && slices.Equal(calls[0].keys, []string{"RES"})
	})
}

func TestIngestRestartsStreamWhenKeysChange(t *testing.T) {
	src, cfg, tr := startIngest(t, tenantWith("RES"))
	eventually(t, "first stream", func() bool { return len(tr.snapshot()) == 1 })

	reload(t, src, cfg, tenantWith("RES")) // unchanged content: no restart
	reload(t, src, cfg, tenantWith("RES", "ORC"))

	eventually(t, "second stream with [ORC RES]", func() bool {
		calls := tr.snapshot()
		return len(calls) == 2 && slices.Equal(calls[1].keys, []string{"ORC", "RES"})
	})
	eventually(t, "first stream cancelled", func() bool { return isClosed(tr.snapshot()[0].cancelled) })
	if isClosed(tr.snapshot()[1].cancelled) {
		t.Fatal("second stream cancelled")
	}
}

func TestIngestStopsStreamOfRemovedTenant(t *testing.T) {
	src, cfg, tr := startIngest(t, tenantWith("RES"))
	eventually(t, "first stream", func() bool { return len(tr.snapshot()) == 1 })

	reload(t, src, cfg, nil)

	eventually(t, "stream cancelled", func() bool { return isClosed(tr.snapshot()[0].cancelled) })
	if n := len(tr.snapshot()); n != 1 {
		t.Fatalf("streams = %d, want 1", n)
	}
}

func TestIngestSkipsTenantWithoutProjects(t *testing.T) {
	src, cfg, tr := startIngest(t, tenantWith())

	reload(t, src, cfg, tenantWith("RES"))

	eventually(t, "stream with [RES]", func() bool { return len(tr.snapshot()) >= 1 })
	if calls := tr.snapshot(); len(calls) != 1 || !slices.Equal(calls[0].keys, []string{"RES"}) {
		t.Fatalf("calls = %d, first keys = %v; want exactly one stream with [RES]", len(calls), calls[0].keys)
	}
}

func TestIngestHandlerKeepsLiveContextWhenKeysChange(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	handlerCtxErr := make(chan error, 1)
	var once sync.Once
	tr := &recordingTracker{event: &pluginv1.TrackerEvent{ExternalId: "e1"}}
	src, cfg, _ := startIngestWith(t, tenantWith("RES"), tr,
		func(ctx context.Context, _ string, _ *pluginv1.TrackerEvent) error {
			first := false
			once.Do(func() { first = true })
			if !first { // event of the restarted stream
				return nil
			}
			close(started)
			<-release
			handlerCtxErr <- ctx.Err()
			return nil
		})
	<-started

	reload(t, src, cfg, tenantWith("RES", "ORC")) // cancels the tenant's stream
	eventually(t, "first stream cancelled", func() bool { return isClosed(tr.snapshot()[0].cancelled) })
	close(release)

	if err := <-handlerCtxErr; err != nil {
		t.Fatalf("handler ctx was cancelled mid-event: %v", err)
	}
}

func withCredential(c *config.Config, cred string) *config.Config {
	t := c.Tenants["acme"]
	t.TrackerCredential = nil
	if cred != "" {
		t.TrackerCredential = []byte(cred)
	}
	c.Tenants["acme"] = t
	return c
}

func TestIngestRestartsStreamOnCredentialChange(t *testing.T) {
	src, cfg, tr := startIngest(t, withCredential(tenantWith("RES"), "old"))
	eventually(t, "first stream", func() bool { return len(tr.snapshot()) == 1 })

	reload(t, src, cfg, withCredential(tenantWith("RES"), "old")) // unchanged: no restart
	reload(t, src, cfg, withCredential(tenantWith("RES"), "new"))

	eventually(t, "second stream with new credential", func() bool {
		calls := tr.snapshot()
		return len(calls) == 2 && calls[1].cred == "new"
	})
	if calls := tr.snapshot(); calls[0].cred != "old" {
		t.Fatalf("first stream credential = %q", calls[0].cred)
	}
	eventually(t, "first stream cancelled", func() bool { return isClosed(tr.snapshot()[0].cancelled) })
}

func TestIngestStopsStreamWhenTrackerCredentialGone(t *testing.T) {
	src, cfg, tr := startIngest(t, tenantWith("RES"))
	eventually(t, "first stream", func() bool { return len(tr.snapshot()) == 1 })

	reload(t, src, cfg, withCredential(tenantWith("RES"), "")) // e.g. connection needs_reauth

	eventually(t, "stream cancelled", func() bool { return isClosed(tr.snapshot()[0].cancelled) })
	reload(t, src, cfg, withCredential(tenantWith("RES", "ORC"), "")) // key change alone starts nothing
	if n := len(tr.snapshot()); n != 1 {
		t.Fatalf("streams = %d, want 1", n)
	}
}

type namedCodeHost struct {
	pluginv1.UnimplementedCodeHostAdapterServer
	capsCalls *atomic.Int32
}

func (n namedCodeHost) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	n.capsCalls.Add(1)
	return &pluginv1.Capabilities{ContractVersion: "plugin.v1", AdapterName: "fake-host"}, nil
}

func TestRegistryAdapterNameOfCodeHostIsCachedWithConnection(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := config.NewSync(context.Background(), config.Static{Config: testConfig()}, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	var capsCalls atomic.Int32
	pluginv1.RegisterCodeHostAdapterServer(srv, namedCodeHost{capsCalls: &capsCalls})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	reg := NewRegistry(cfg, log)
	reg.dialer = func(string) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	t.Cleanup(reg.Close)

	for range 3 {
		got, err := reg.AdapterName(context.Background(), "acme", "codehost")
		if err != nil || got != "fake-host" {
			t.Fatalf("AdapterName = %q, %v", got, err)
		}
	}
	if n := capsCalls.Load(); n != 1 {
		t.Fatalf("GetCapabilities called %d times, want 1", n)
	}
}

func TestRegistryAdapterNameIsCachedWithConnection(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := config.NewSync(context.Background(), config.Static{Config: testConfig()}, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pluginv1.RegisterTrackerAdapterServer(srv, &fakeTracker{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	reg := NewRegistry(cfg, log)
	reg.dialer = func(string) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	t.Cleanup(reg.Close)

	got, err := reg.AdapterName(context.Background(), "acme", "tracker")
	if err != nil || got != "fake" {
		t.Fatalf("AdapterName = %q, %v", got, err)
	}
	if _, err := reg.AdapterName(context.Background(), "nope", "tracker"); err == nil {
		t.Fatal("unknown tenant must fail")
	}
}
