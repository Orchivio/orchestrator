// Package adapters holds the gRPC clients to the tenants' tracker and code
// host adapters and the loop that ingests tracker events.
package adapters

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pluginv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/plugin/v1"
	"github.com/soemii/orchivio-server/internal/config"
)

const contractVersion = "plugin.v1"

// conn is a dialed adapter whose capabilities were checked once.
type conn struct {
	cc      *grpc.ClientConn
	mu      sync.Mutex
	checked bool
	err     error // non-nil once checked: adapter refused
}

// check calls GetCapabilities until it got an answer; a refusal is cached,
// a transport error is retried on the next use.
func (c *conn) check(ctx context.Context, log *slog.Logger, tenant, kind, addr string, cl capsClient) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checked {
		return c.err
	}
	caps, err := cl.GetCapabilities(ctx, &pluginv1.GetCapabilitiesRequest{})
	if err != nil {
		return fmt.Errorf("capabilities of %s adapter of tenant %s: %w", kind, tenant, err)
	}
	c.checked = true
	if caps.GetContractVersion() != contractVersion {
		c.err = fmt.Errorf("%s adapter of tenant %s speaks %q, want %q", kind, tenant, caps.GetContractVersion(), contractVersion)
		log.Error("adapter refused", "tenant", tenant, "kind", kind, "addr", addr, "contract_version", caps.GetContractVersion())
		return c.err
	}
	log.Info("adapter connected", "tenant", tenant, "kind", kind, "adapter", caps.GetAdapterName())
	return nil
}

// Registry lazily dials the adapters named in the current config.
type Registry struct {
	cfg    *config.Sync
	log    *slog.Logger
	dialer func(addr string) (*grpc.ClientConn, error)

	mu    sync.Mutex
	conns map[string]*conn
}

// NewRegistry returns a registry reading adapter addresses from cfg.
func NewRegistry(cfg *config.Sync, log *slog.Logger) *Registry {
	return &Registry{cfg: cfg, log: log, conns: map[string]*conn{}, dialer: func(addr string) (*grpc.ClientConn, error) {
		// ponytail: insecure transport, mTLS later
		return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}}
}

// Close closes all connections.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		_ = c.cc.Close()
	}
	r.conns = map[string]*conn{}
}

type capsClient interface {
	GetCapabilities(ctx context.Context, in *pluginv1.GetCapabilitiesRequest, opts ...grpc.CallOption) (*pluginv1.Capabilities, error)
}

// connFor returns the connection for addr; the capability check runs once
// and its outcome is cached. check is called with a client on the conn.
func (r *Registry) connFor(ctx context.Context, tenant, kind, addr string, check func(cc *grpc.ClientConn) capsClient) (*grpc.ClientConn, error) {
	r.mu.Lock()
	c, ok := r.conns[addr]
	if !ok {
		cc, err := r.dialer(addr)
		if err != nil {
			r.mu.Unlock()
			return nil, fmt.Errorf("dial %s adapter of tenant %s at %s: %w", kind, tenant, addr, err)
		}
		c = &conn{cc: cc}
		r.conns[addr] = c
	}
	r.mu.Unlock()
	if err := c.check(ctx, r.log, tenant, kind, addr, check(c.cc)); err != nil {
		return nil, err
	}
	return c.cc, nil
}

func (r *Registry) tenant(name string) (config.Tenant, error) {
	t, ok := r.cfg.Current().Tenants[name]
	if !ok {
		return t, fmt.Errorf("unknown tenant %s", name)
	}
	return t, nil
}

func (r *Registry) trackerClient(ctx context.Context, tenant string) (pluginv1.TrackerAdapterClient, error) {
	t, err := r.tenant(tenant)
	if err != nil {
		return nil, err
	}
	cc, err := r.connFor(ctx, tenant, "tracker", t.Tracker.Address, func(cc *grpc.ClientConn) capsClient { return pluginv1.NewTrackerAdapterClient(cc) })
	if err != nil {
		return nil, err
	}
	return pluginv1.NewTrackerAdapterClient(cc), nil
}

// Tracker returns the router.Tracker over all tenants.
func (r *Registry) Tracker() *Tracker { return &Tracker{r} }

// TrackerFor returns the comment client of one tenant; never nil, errors on use.
func (r *Registry) TrackerFor(tenant string) *TenantTracker { return &TenantTracker{r, tenant} }

// CodeHost returns the orchestrator.CodeHost over all tenants.
func (r *Registry) CodeHost() *CodeHost { return &CodeHost{r} }

// Tracker implements router.Tracker.
type Tracker struct{ r *Registry }

// GetTicket fetches a ticket from the tenant's tracker.
func (t *Tracker) GetTicket(ctx context.Context, tenant, ref string) (*pluginv1.Ticket, error) {
	c, err := t.r.trackerClient(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return c.GetTicket(ctx, &pluginv1.GetTicketRequest{TicketRef: ref})
}

// TenantTracker implements reporter.TrackerClient.
type TenantTracker struct {
	r      *Registry
	tenant string
}

// PostComment comments on a ticket.
func (t *TenantTracker) PostComment(ctx context.Context, ref, body string) error {
	c, err := t.r.trackerClient(ctx, t.tenant)
	if err != nil {
		return err
	}
	_, err = c.PostComment(ctx, &pluginv1.PostCommentRequest{TicketRef: ref, Body: body})
	return err
}

// CodeHost implements orchestrator.CodeHost.
type CodeHost struct{ r *Registry }

func (h *CodeHost) client(ctx context.Context, tenant string) (pluginv1.CodeHostAdapterClient, error) {
	t, err := h.r.tenant(tenant)
	if err != nil {
		return nil, err
	}
	cc, err := h.r.connFor(ctx, tenant, "codehost", t.CodeHost.Address, func(cc *grpc.ClientConn) capsClient { return pluginv1.NewCodeHostAdapterClient(cc) })
	if err != nil {
		return nil, err
	}
	return pluginv1.NewCodeHostAdapterClient(cc), nil
}

// IssueRepoCredentials asks the code host for short-lived repo credentials.
func (h *CodeHost) IssueRepoCredentials(ctx context.Context, tenant string, repos []string, write bool) (*pluginv1.RepoCredentials, error) {
	c, err := h.client(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return c.IssueRepoCredentials(ctx, &pluginv1.IssueRepoCredentialsRequest{Repos: repos, Write: write})
}

// OpenPullRequest opens a pull request at the code host.
func (h *CodeHost) OpenPullRequest(ctx context.Context, tenant string, req *pluginv1.OpenPullRequestRequest) (*pluginv1.PullRequest, error) {
	c, err := h.client(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return c.OpenPullRequest(ctx, req)
}
