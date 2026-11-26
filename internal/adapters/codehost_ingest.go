package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"log/slog"
	"slices"
	"time"

	"google.golang.org/grpc"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	"github.com/orchivio/orchestrator/internal/config"
)

// RunCodeHost reads the code host event stream of every tenant with repos and
// an active code host connection and hands each event to handle. Like Run it
// follows config changes: a tenant's stream is (re)started when its repos or
// code host credentials change. Adapters without supports_review_events get
// no stream.
func RunCodeHost(ctx context.Context, cfg *config.Sync, reg *Registry,
	handle func(ctx context.Context, tenant string, ev *pluginv1.CodeHostEvent) error, log *slog.Logger) {
	runCodeHost(ctx, cfg, reg, handle, log, initialBackoff, maxBackoff)
}

func runCodeHost(ctx context.Context, cfg *config.Sync, reg *Registry,
	handle func(ctx context.Context, tenant string, ev *pluginv1.CodeHostEvent) error, log *slog.Logger,
	initial, limit time.Duration) {
	supervise(ctx, cfg, codeHostKeys, func(runCtx, ctx context.Context, tenant string, repos []string) (bool, error) {
		return codeHostStreamOnce(runCtx, ctx, reg, tenant, repos, handle, log)
	}, "codehost", log, initial, limit)
}

// codeHostKeys maps each tenant with repos and code host credentials to its
// sorted repos and the hash of all its credentials.
func codeHostKeys(c *config.Config) map[string]streamKey {
	out := map[string]streamKey{}
	for name, t := range c.Tenants {
		if len(t.CodeHostCredentials) == 0 {
			continue
		}
		var repos []string
		for _, p := range t.Projects {
			for _, r := range p.Repos {
				repos = append(repos, r.Slug)
			}
		}
		if len(repos) == 0 {
			continue
		}
		slices.Sort(repos)
		h := sha256.New()
		for _, cred := range t.CodeHostCredentials {
			_ = binary.Write(h, binary.BigEndian, uint64(len(cred))) // length prefix keeps ["ab","c"] != ["a","bc"]
			h.Write(cred)
		}
		k := streamKey{keys: slices.Compact(repos)}
		copy(k.cred[:], h.Sum(nil))
		out[name] = k
	}
	return out
}

// codeHostStreamOnce reads one code host stream until it ends; without
// supports_review_events it idles until the stream's ctx ends.
func codeHostStreamOnce(runCtx, ctx context.Context, reg *Registry, tenant string, repos []string,
	handle func(ctx context.Context, tenant string, ev *pluginv1.CodeHostEvent) error, log *slog.Logger) (bool, error) {
	h := reg.CodeHost()
	c, creds, ids, err := h.client(ctx, tenant)
	if err != nil {
		return false, err
	}
	t, err := reg.tenant(tenant)
	if err != nil {
		return false, err
	}
	cn, err := reg.checkedConn(ctx, tenant, "codehost", t.CodeHost.Address,
		func(cc *grpc.ClientConn) capsClient { return pluginv1.NewCodeHostAdapterClient(cc) })
	if err != nil {
		return false, err
	}
	cn.mu.Lock()
	supported := cn.reviews
	cn.mu.Unlock()
	if !supported {
		log.Info("code host adapter has no review events, no stream", "tenant", tenant)
		<-ctx.Done()
		return false, nil
	}
	stream, err := c.StreamEvents(ctx, &pluginv1.StreamCodeHostEventsRequest{Repos: repos, Credentials: creds})
	if err != nil {
		return false, reg.rejected(ctx, tenant, ids, err)
	}
	return receive(runCtx, ctx, reg, tenant, ids, stream.Recv, handle, "codehost", log)
}
