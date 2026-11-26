// Package conformance checks a running adapter against the plugin.v1 contract.
package conformance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
)

const (
	// KindTracker and KindCodeHost select which adapter service is checked.
	KindTracker  = "tracker"
	KindCodeHost = "codehost"

	contractVersion = "plugin.v1"
	unknownRef      = "orchivio-conformance-unknown"
)

// cancelDeadline is how long a stream may take to end after cancel; a variable so tests can shorten it.
var cancelDeadline = 5 * time.Second

// dialOptions are extra dial options; a seam for tests.
var dialOptions []grpc.DialOption

// Target describes the adapter under test. Status is the status used for the
// SetStatus double call; when empty that check is skipped.
type Target struct {
	Addr, Kind string
	Credential []byte
	Ticket     string
	Repo       string
	Status     string
}

// Failure is one violated contract rule.
type Failure struct{ Check, Detail string }

// Run checks the target and returns all violations; empty means conform.
func Run(ctx context.Context, t Target) []Failure {
	conn, err := grpc.NewClient(t.Addr, append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, dialOptions...)...)
	if err != nil {
		return []Failure{{"dial", err.Error()}}
	}
	defer conn.Close()
	switch t.Kind {
	case KindTracker:
		return runTracker(ctx, pluginv1.NewTrackerAdapterClient(conn), t)
	case KindCodeHost:
		return runCodeHost(ctx, pluginv1.NewCodeHostAdapterClient(conn), t)
	}
	return []Failure{{"kind", fmt.Sprintf("unknown kind %q, want tracker or codehost", t.Kind)}}
}

type collector []Failure

func (c *collector) add(check, format string, args ...any) {
	*c = append(*c, Failure{check, fmt.Sprintf(format, args...)})
}

func (c *collector) expectCode(check string, err error, want ...codes.Code) {
	got := status.Code(err)
	for _, code := range want {
		if got == code {
			return
		}
	}
	c.add(check, "got code %s (err=%v), want one of %v", got, err, want)
}

func (c *collector) capabilities(caps *pluginv1.Capabilities) {
	if caps.GetContractVersion() != contractVersion {
		c.add("contract-version", "got %q, want %q", caps.GetContractVersion(), contractVersion)
	}
	if caps.GetAdapterName() == "" {
		c.add("adapter-name", "adapter_name is empty")
	}
	for i, field := range caps.GetConnectFields() {
		if field.GetName() == "" {
			c.add("connect-fields", "connect_fields[%d] has no name", i)
		}
	}
}

// expectCancel ends the stream via context cancel and requires Recv to return in time.
func (c *collector) expectCancel(ctx context.Context, open func(context.Context) (func() error, error)) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	recv, err := open(streamCtx)
	if err != nil {
		c.add("stream-cancel", "open stream: %v", err)
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for recv() == nil { // drain events until the stream ends
		}
	}()
	cancel()
	timer := time.NewTimer(cancelDeadline)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		c.add("stream-cancel", "stream did not end within %s after cancel", cancelDeadline)
	}
}

func runTracker(ctx context.Context, client pluginv1.TrackerAdapterClient, t Target) []Failure {
	var c collector
	caps, err := client.GetCapabilities(ctx, &pluginv1.GetCapabilitiesRequest{})
	if err != nil {
		return []Failure{{"capabilities", err.Error()}}
	}
	c.capabilities(caps)
	cred := &pluginv1.Credential{Data: t.Credential}

	_, err = client.GetTicket(ctx, &pluginv1.GetTicketRequest{TicketRef: unknownRef, Credential: cred})
	c.expectCode("unknown-ticket", err, codes.NotFound, codes.FailedPrecondition)
	_, err = client.GetTicket(ctx, &pluginv1.GetTicketRequest{TicketRef: t.Ticket, Credential: &pluginv1.Credential{}})
	c.expectCode("bad-credential", err, codes.InvalidArgument, codes.Unauthenticated)

	if caps.GetSupportsStatusUpdate() && t.Status != "" {
		req := &pluginv1.SetStatusRequest{TicketRef: t.Ticket, Status: t.Status, Credential: cred}
		_, _ = client.SetStatus(ctx, req)
		second, err := client.SetStatus(ctx, req)
		if err != nil {
			c.add("set-status", "second call failed: %v", err)
		} else if second.GetChanged() {
			c.add("set-status", "second identical call returned changed=true")
		}
	}

	keys := []string(nil)
	if project, _, found := strings.Cut(t.Ticket, "-"); found {
		keys = []string{project}
	}
	c.expectCancel(ctx, func(sctx context.Context) (func() error, error) {
		stream, err := client.StreamEvents(sctx, &pluginv1.StreamEventsRequest{ProjectKeys: keys, Credential: cred})
		if err != nil {
			return nil, err
		}
		return func() error { _, err := stream.Recv(); return err }, nil
	})
	return c
}

func runCodeHost(ctx context.Context, client pluginv1.CodeHostAdapterClient, t Target) []Failure {
	var c collector
	caps, err := client.GetCapabilities(ctx, &pluginv1.GetCapabilitiesRequest{})
	if err != nil {
		return []Failure{{"capabilities", err.Error()}}
	}
	c.capabilities(caps)
	creds := []*pluginv1.Credential{{Data: t.Credential}}

	_, err = client.IssueRepoCredentials(ctx, &pluginv1.IssueRepoCredentialsRequest{Repos: []string{unknownRef}, Credentials: creds})
	c.expectCode("unknown-repo", err, codes.NotFound, codes.FailedPrecondition)
	_, err = client.IssueRepoCredentials(ctx, &pluginv1.IssueRepoCredentialsRequest{Repos: []string{t.Repo}, Credentials: []*pluginv1.Credential{{}}})
	c.expectCode("bad-credential", err, codes.InvalidArgument, codes.Unauthenticated)

	issued, err := client.IssueRepoCredentials(ctx, &pluginv1.IssueRepoCredentialsRequest{Repos: []string{t.Repo}, Credentials: creds})
	if err != nil {
		c.add("repo-credentials", "issue failed: %v", err)
	} else if issued.GetToken() == "" && issued.GetTokens()[t.Repo].GetToken() == "" {
		c.add("repo-credentials", "no token for %q (neither token nor tokens[repo])", t.Repo)
	}

	c.expectCancel(ctx, func(sctx context.Context) (func() error, error) {
		stream, err := client.StreamEvents(sctx, &pluginv1.StreamCodeHostEventsRequest{Repos: []string{t.Repo}, Credentials: creds})
		if err != nil {
			return nil, err
		}
		return func() error { _, err := stream.Recv(); return err }, nil
	})
	return c
}
