package adapters

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	"github.com/orchivio/orchestrator/internal/admin"
)

// connectClient is the part both adapter services share.
type connectClient interface {
	capsClient
	BeginConnect(ctx context.Context, in *pluginv1.BeginConnectRequest, opts ...grpc.CallOption) (*pluginv1.BeginConnectResponse, error)
	CompleteConnect(ctx context.Context, in *pluginv1.CompleteConnectRequest, opts ...grpc.CallOption) (*pluginv1.CompleteConnectResponse, error)
	RefreshCredential(ctx context.Context, in *pluginv1.RefreshCredentialRequest, opts ...grpc.CallOption) (*pluginv1.Credential, error)
	Disconnect(ctx context.Context, in *pluginv1.DisconnectRequest, opts ...grpc.CallOption) (*pluginv1.DisconnectResponse, error)
	CheckAccess(ctx context.Context, in *pluginv1.CheckAccessRequest, opts ...grpc.CallOption) (*pluginv1.CheckAccessResponse, error)
}

type connector struct {
	r     *Registry
	addrs map[string]string // adapter name -> address, from ORCHIVIO_ADAPTERS
}

// Connector returns the admin.Connector dialing adapters by name.
func (r *Registry) Connector(addrs map[string]string) admin.Connector {
	return &connector{r: r, addrs: addrs}
}

func (c *connector) client(ctx context.Context, adapter, kind string) (connectClient, error) {
	addr, ok := c.addrs[adapter]
	if !ok {
		return nil, fmt.Errorf("unknown adapter %q", adapter)
	}
	newClient := func(cc *grpc.ClientConn) connectClient { return pluginv1.NewCodeHostAdapterClient(cc) }
	if kind == admin.KindTracker {
		newClient = func(cc *grpc.ClientConn) connectClient { return pluginv1.NewTrackerAdapterClient(cc) }
	}
	cc, err := c.r.connFor(ctx, "(connect "+adapter+")", kind, addr, func(cc *grpc.ClientConn) capsClient { return newClient(cc) })
	if err != nil {
		return nil, fmt.Errorf("%w: %w", admin.ErrUnavailable, err)
	}
	return newClient(cc), nil
}

// mapErr turns adapter gRPC codes into admin errors; it names the adapter, never credentials.
func mapErr(adapter, op string, err error) error {
	switch st := status.Convert(err); st.Code() {
	case codes.Unauthenticated:
		return fmt.Errorf("%s at %s: %w: %w", op, adapter, admin.ErrCredentialRejected, err)
	case codes.PermissionDenied: // the adapter's own explanation, e.g. GitHub's installation-admin check
		return fmt.Errorf("%s at %s: %w", op, adapter, &admin.RejectedError{Reason: st.Message()})
	case codes.FailedPrecondition:
		return &admin.NotConnectedError{Reason: st.Message()}
	case codes.Unavailable, codes.DeadlineExceeded:
		return fmt.Errorf("%s at %s: %w: %w", op, adapter, admin.ErrUnavailable, err)
	}
	return fmt.Errorf("%s at %s: %w: %w", op, adapter, admin.ErrAdapter, err)
}

func (c *connector) BeginConnect(ctx context.Context, adapter, kind, tenant, redirectURI, state string) (string, error) {
	cl, err := c.client(ctx, adapter, kind)
	if err != nil {
		return "", err
	}
	resp, err := cl.BeginConnect(ctx, &pluginv1.BeginConnectRequest{Tenant: tenant, RedirectUri: redirectURI, State: state})
	if err != nil {
		return "", mapErr(adapter, "begin connect", err)
	}
	return resp.GetUrl(), nil
}

func (c *connector) CompleteConnect(ctx context.Context, adapter, kind, redirectURI string, query map[string]string) (admin.Completed, error) {
	cl, err := c.client(ctx, adapter, kind)
	if err != nil {
		return admin.Completed{}, err
	}
	resp, err := cl.CompleteConnect(ctx, &pluginv1.CompleteConnectRequest{RedirectUri: redirectURI, Query: query})
	if err != nil {
		return admin.Completed{}, mapErr(adapter, "complete connect", err)
	}
	done := completed(resp.GetCredential())
	done.Label, done.ExternalID = resp.GetLabel(), resp.GetExternalId()
	return done, nil
}

func (c *connector) Refresh(ctx context.Context, adapter, kind string, data []byte, refreshAfter *time.Time) (admin.Completed, error) {
	cl, err := c.client(ctx, adapter, kind)
	if err != nil {
		return admin.Completed{}, err
	}
	cred, err := cl.RefreshCredential(ctx, &pluginv1.RefreshCredentialRequest{Credential: credential(data, refreshAfter)})
	if err != nil {
		return admin.Completed{}, mapErr(adapter, "refresh credential", err)
	}
	return completed(cred), nil
}

func (c *connector) Disconnect(ctx context.Context, adapter, kind string, data []byte) error {
	cl, err := c.client(ctx, adapter, kind)
	if err != nil {
		return err
	}
	if _, err := cl.Disconnect(ctx, &pluginv1.DisconnectRequest{Credential: credential(data, nil)}); err != nil {
		return mapErr(adapter, "disconnect", err)
	}
	return nil
}

func (c *connector) CheckAccess(ctx context.Context, adapter, kind string, creds [][]byte, items []string) ([]string, error) {
	cl, err := c.client(ctx, adapter, kind)
	if err != nil {
		return nil, err
	}
	pcreds := make([]*pluginv1.Credential, len(creds))
	for i, d := range creds {
		pcreds[i] = credential(d, nil)
	}
	resp, err := cl.CheckAccess(ctx, &pluginv1.CheckAccessRequest{Credentials: pcreds, Items: items})
	if err != nil {
		return nil, mapErr(adapter, "check access", err)
	}
	return resp.GetDenied(), nil
}

func credential(data []byte, refreshAfter *time.Time) *pluginv1.Credential {
	cred := &pluginv1.Credential{Data: data}
	if refreshAfter != nil {
		cred.RefreshAfter = timestamppb.New(*refreshAfter)
	}
	return cred
}

func completed(cred *pluginv1.Credential) admin.Completed {
	done := admin.Completed{Data: cred.GetData()}
	if cred.GetRefreshAfter() != nil {
		t := cred.GetRefreshAfter().AsTime()
		done.RefreshAfter = &t
	}
	return done
}
