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

// rejectCodes are the acceptable answers to a missing or invalid credential.
var rejectCodes = []codes.Code{codes.Unauthenticated, codes.InvalidArgument, codes.PermissionDenied}

// invalidCredential is a non-empty credential no adapter should accept.
var invalidCredential = []byte("orchivio-conformance-invalid")

// cancelDeadline is how long a stream may take to end after cancel; a variable so tests can shorten it.
var cancelDeadline = 5 * time.Second

// settleTime is how long a fresh stream may run before cancel, so an immediate server error is seen.
var settleTime = 300 * time.Millisecond

// dialOptions are extra dial options; a seam for tests.
var dialOptions []grpc.DialOption

// Target describes the adapter under test. Status is the status used for the
// SetStatus double call; when empty that check is skipped and Notify (if set)
// is told. The set-status check MUTATES the ticket: it really sets Status.
type Target struct {
	Addr, Kind string
	Credential []byte
	Ticket     string
	Repo       string
	Status     string
	Notify     func(message string) // optional; receives non-failure notes such as skipped checks
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

// expectCancel cancels an open stream and requires its Recv to end with
// codes.Canceled within cancelDeadline. A stream that ends by itself before the
// cancel (e.g. Unimplemented) fails too. Known limit: a server that ignores
// cancellation cannot be detected from the client side.
func (c *collector) expectCancel(ctx context.Context, open func(context.Context) (func() error, error)) {
	const check = "stream-ends-on-cancel"
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	recv, err := open(streamCtx)
	if err != nil {
		c.add(check, "open stream: %v", err)
		return
	}
	final := make(chan error, 1)
	go func() {
		for {
			if err := recv(); err != nil {
				final <- err
				return
			}
		}
	}()
	settle := time.NewTimer(settleTime)
	defer settle.Stop()
	select {
	case err := <-final: // ended on its own before any cancel: that is an error, not a pass
		c.add(check, "stream ended by itself with code %s (err=%v) before cancel", status.Code(err), err)
		return
	case <-settle.C:
	}
	cancel()
	timer := time.NewTimer(cancelDeadline)
	defer timer.Stop()
	select {
	case err := <-final:
		if status.Code(err) != codes.Canceled {
			c.add(check, "stream ended with code %s (err=%v), want Canceled", status.Code(err), err)
		}
	case <-timer.C:
		c.add(check, "stream did not end within %s after cancel", cancelDeadline)
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
	c.expectCode("empty-credential", err, rejectCodes...)
	_, err = client.GetTicket(ctx, &pluginv1.GetTicketRequest{TicketRef: t.Ticket, Credential: &pluginv1.Credential{Data: invalidCredential}})
	c.expectCode("invalid-credential", err, rejectCodes...)

	if caps.GetSupportsStatusUpdate() {
		c.setStatusTwice(ctx, client, t, cred)
	}

	// project key derivation assumes Jira-style keys ("ORC-12" -> "ORC").
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
	c.expectCode("empty-credential", err, rejectCodes...)
	_, err = client.IssueRepoCredentials(ctx, &pluginv1.IssueRepoCredentialsRequest{Repos: []string{t.Repo}, Credentials: []*pluginv1.Credential{{Data: invalidCredential}}})
	c.expectCode("invalid-credential", err, rejectCodes...)

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

func (c *collector) setStatusTwice(ctx context.Context, client pluginv1.TrackerAdapterClient, t Target, cred *pluginv1.Credential) {
	if t.Status == "" {
		if t.Notify != nil {
			t.Notify("SKIP set-status: supports_status_update is set but no status was given")
		}
		return
	}
	req := &pluginv1.SetStatusRequest{TicketRef: t.Ticket, Status: t.Status, Credential: cred}
	_, firstErr := client.SetStatus(ctx, req)
	second, err := client.SetStatus(ctx, req)
	switch {
	case err != nil:
		c.add("set-status", "second call failed: %v (first call error: %v)", err, firstErr)
	case second.GetChanged():
		c.add("set-status", "second identical call returned changed=true (first call error: %v)", firstErr)
	}
}
