// Package adapters holds the gRPC clients to the tenants' tracker and code
// host adapters and the loops that ingest their events.
package adapters

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	"github.com/orchivio/orchestrator/internal/config"
)

const (
	contractVersion = "plugin.v1"
	// rejectHookTimeout bounds the needs_reauth hook (DB write and config reload).
	rejectHookTimeout = 15 * time.Second
)

// conn is a dialed adapter whose capabilities were checked once.
type conn struct {
	cc      *grpc.ClientConn
	mu      sync.Mutex
	checked bool
	err     error  // non-nil once checked: adapter refused
	name    string // adapter_name of the capabilities answer
	status  bool   // supports_status_update of the capabilities answer
	reviews bool   // supports_review_events of the capabilities answer
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
	c.name = caps.GetAdapterName()
	c.status = caps.GetSupportsStatusUpdate()
	c.reviews = caps.GetSupportsReviewEvents()
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

	onRejected func(ctx context.Context, tenant string, connectionID uuid.UUID)
}

// OnRejected registers the hook run when an adapter rejects the credential of
// one connection (Unauthenticated), so the caller can mark it needs_reauth.
// Call before first use.
func (r *Registry) OnRejected(f func(ctx context.Context, tenant string, connectionID uuid.UUID)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onRejected = f
}

// rejected passes err through; if the adapter answered Unauthenticated to a
// call carrying exactly one credential it also runs the OnRejected hook.
// ponytail: calls with several code host credentials mark nothing, the error
// does not say which one failed; upgrade: the adapter names the external_id in
// the error detail and we map it to the connection.
func (r *Registry) rejected(ctx context.Context, tenant string, ids []uuid.UUID, err error) error {
	if len(ids) != 1 || status.Code(err) != codes.Unauthenticated {
		return err
	}
	r.mu.Lock()
	hook := r.onRejected
	r.mu.Unlock()
	if hook != nil {
		// detached so a cancelled request still marks the connection, but bounded
		hookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rejectHookTimeout)
		defer cancel()
		hook(hookCtx, tenant, ids[0])
	}
	return err
}

// NewRegistry returns a registry reading adapter addresses from cfg.
func NewRegistry(cfg *config.Sync, log *slog.Logger) *Registry {
	return &Registry{cfg: cfg, log: log, conns: map[string]*conn{}, dialer: func(addr string) (*grpc.ClientConn, error) {
		// ponytail: insecure transport, mTLS later
		return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}}
}

// SetDialer replaces how adapter addresses are dialed; for tests that talk
// to in-memory adapters. Call before first use.
func (r *Registry) SetDialer(d func(addr string) (*grpc.ClientConn, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dialer = d
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
	c, err := r.checkedConn(ctx, tenant, kind, addr, check)
	if err != nil {
		return nil, err
	}
	return c.cc, nil
}

// checkedConn is connFor returning the cached conn itself.
func (r *Registry) checkedConn(ctx context.Context, tenant, kind, addr string, check func(cc *grpc.ClientConn) capsClient) (*conn, error) {
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
	return c, nil
}

func (r *Registry) tenant(name string) (config.Tenant, error) {
	t, ok := r.cfg.Current().Tenants[name]
	if !ok {
		return t, fmt.Errorf("unknown tenant %s", name)
	}
	return t, nil
}

// trackerClient returns the tenant's tracker client, credential and the id of
// the connection it came from; without an active tracker connection the
// adapter is not called.
func (r *Registry) trackerClient(ctx context.Context, tenant string) (pluginv1.TrackerAdapterClient, *pluginv1.Credential, uuid.UUID, error) {
	t, err := r.tenant(tenant)
	if err != nil {
		return nil, nil, uuid.Nil, err
	}
	if t.TrackerCredential == nil {
		return nil, nil, uuid.Nil, fmt.Errorf("tenant %s has no active tracker connection", tenant)
	}
	cc, err := r.connFor(ctx, tenant, "tracker", t.Tracker.Address, func(cc *grpc.ClientConn) capsClient { return pluginv1.NewTrackerAdapterClient(cc) })
	if err != nil {
		return nil, nil, uuid.Nil, err
	}
	return pluginv1.NewTrackerAdapterClient(cc), &pluginv1.Credential{Data: t.TrackerCredential}, t.TrackerConnectionID, nil
}

// AdapterName returns the adapter_name the tenant's adapter of kind ("tracker"
// or "codehost") reported in its capabilities; no credential is needed.
func (r *Registry) AdapterName(ctx context.Context, tenant, kind string) (string, error) {
	t, err := r.tenant(tenant)
	if err != nil {
		return "", err
	}
	ref, mk := t.Tracker, func(cc *grpc.ClientConn) capsClient { return pluginv1.NewTrackerAdapterClient(cc) }
	if kind == "codehost" {
		ref, mk = t.CodeHost, func(cc *grpc.ClientConn) capsClient { return pluginv1.NewCodeHostAdapterClient(cc) }
	}
	c, err := r.checkedConn(ctx, tenant, kind, ref.Address, mk)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.name, nil
}

// Tracker returns the router.Tracker over all tenants.
func (r *Registry) Tracker() *Tracker { return &Tracker{r} }

// TrackerFor returns the comment client of one tenant; never nil, errors on use.
func (r *Registry) TrackerFor(tenant string) *TenantTracker { return &TenantTracker{r, tenant} }

// CodeHost returns the orchestrator.CodeHost over all tenants.
func (r *Registry) CodeHost() *CodeHost { return &CodeHost{r} }

// Tracker implements router.Tracker.
type Tracker struct{ r *Registry }

// AdapterName returns the name of the tenant's tracker adapter.
func (t *Tracker) AdapterName(ctx context.Context, tenant string) (string, error) {
	return t.r.AdapterName(ctx, tenant, "tracker")
}

// GetTicket fetches a ticket from the tenant's tracker.
func (t *Tracker) GetTicket(ctx context.Context, tenant, ref string) (*pluginv1.Ticket, error) {
	c, cred, id, err := t.r.trackerClient(ctx, tenant)
	if err != nil {
		return nil, err
	}
	ticket, err := c.GetTicket(ctx, &pluginv1.GetTicketRequest{TicketRef: ref, Credential: cred})
	return ticket, t.r.rejected(ctx, tenant, []uuid.UUID{id}, err)
}

// SetStatus moves a ticket to the named status; a no-op when the adapter lacks
// supports_status_update (cached capabilities, no extra call).
func (t *Tracker) SetStatus(ctx context.Context, tenant, ref, statusName string) error {
	c, cred, id, err := t.r.trackerClient(ctx, tenant)
	if err != nil {
		return err
	}
	tn, err := t.r.tenant(tenant)
	if err != nil {
		return err
	}
	cn, err := t.r.checkedConn(ctx, tenant, "tracker", tn.Tracker.Address, func(cc *grpc.ClientConn) capsClient { return pluginv1.NewTrackerAdapterClient(cc) })
	if err != nil {
		return err
	}
	cn.mu.Lock()
	supported := cn.status
	cn.mu.Unlock()
	if !supported {
		return nil
	}
	_, err = c.SetStatus(ctx, &pluginv1.SetStatusRequest{TicketRef: ref, Status: statusName, Credential: cred})
	return t.r.rejected(ctx, tenant, []uuid.UUID{id}, err)
}

// TenantTracker implements reporter.TrackerClient.
type TenantTracker struct {
	r      *Registry
	tenant string
}

// PostComment comments on a ticket.
func (t *TenantTracker) PostComment(ctx context.Context, ref, body string) error {
	c, cred, id, err := t.r.trackerClient(ctx, t.tenant)
	if err != nil {
		return err
	}
	_, err = c.PostComment(ctx, &pluginv1.PostCommentRequest{TicketRef: ref, Body: body, Credential: cred})
	return t.r.rejected(ctx, t.tenant, []uuid.UUID{id}, err)
}

// CodeHost implements orchestrator.CodeHost.
type CodeHost struct{ r *Registry }

// client returns the tenant's code host client and all its code host
// credentials with their connection ids; the adapter picks the credential
// whose installation holds the repo.
func (h *CodeHost) client(ctx context.Context, tenant string) (pluginv1.CodeHostAdapterClient, []*pluginv1.Credential, []uuid.UUID, error) {
	t, err := h.r.tenant(tenant)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(t.CodeHostCredentials) == 0 {
		return nil, nil, nil, fmt.Errorf("tenant %s has no active code host connection", tenant)
	}
	cc, err := h.r.connFor(ctx, tenant, "codehost", t.CodeHost.Address, func(cc *grpc.ClientConn) capsClient { return pluginv1.NewCodeHostAdapterClient(cc) })
	if err != nil {
		return nil, nil, nil, err
	}
	creds := make([]*pluginv1.Credential, len(t.CodeHostCredentials))
	for i, d := range t.CodeHostCredentials {
		creds[i] = &pluginv1.Credential{Data: d}
	}
	return pluginv1.NewCodeHostAdapterClient(cc), creds, t.CodeHostConnectionIDs, nil
}

// IssueRepoCredentials asks the code host for short-lived repo credentials.
func (h *CodeHost) IssueRepoCredentials(ctx context.Context, tenant string, repos []string, write bool) (*pluginv1.RepoCredentials, error) {
	c, creds, ids, err := h.client(ctx, tenant)
	if err != nil {
		return nil, err
	}
	rc, err := c.IssueRepoCredentials(ctx, &pluginv1.IssueRepoCredentialsRequest{Repos: repos, Write: write, Credentials: creds})
	return rc, h.r.rejected(ctx, tenant, ids, err)
}

// OpenPullRequest opens a pull request at the code host; req gets the
// tenant's credentials set.
func (h *CodeHost) OpenPullRequest(ctx context.Context, tenant string, req *pluginv1.OpenPullRequestRequest) (*pluginv1.PullRequest, error) {
	c, creds, ids, err := h.client(ctx, tenant)
	if err != nil {
		return nil, err
	}
	req.Credentials = creds
	pr, err := c.OpenPullRequest(ctx, req)
	return pr, h.r.rejected(ctx, tenant, ids, err)
}

// AdapterName returns the name of the tenant's code host adapter.
func (h *CodeHost) AdapterName(ctx context.Context, tenant string) (string, error) {
	return h.r.AdapterName(ctx, tenant, "codehost")
}

// RevokeRepoCredentials withdraws repo credentials issued earlier.
func (h *CodeHost) RevokeRepoCredentials(ctx context.Context, tenant, credentialID string) error {
	c, creds, ids, err := h.client(ctx, tenant)
	if err != nil {
		return err
	}
	_, err = c.RevokeRepoCredentials(ctx, &pluginv1.RevokeRepoCredentialsRequest{CredentialId: credentialID, Credentials: creds})
	return h.r.rejected(ctx, tenant, ids, err)
}
