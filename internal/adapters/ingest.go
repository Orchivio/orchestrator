package adapters

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	"github.com/orchivio/orchestrator/internal/config"
)

const (
	initialBackoff = time.Second
	maxBackoff     = 30 * time.Second
)

// Run reads the tracker event stream of every tenant with projects and hands
// each event to handle. It follows config changes: a tenant's stream is
// (re)started when its project keys change and stopped when it has none left.
// Each stream reconnects with backoff. Run returns on ctx done once all
// streams have stopped.
func Run(ctx context.Context, cfg *config.Sync, reg *Registry,
	handle func(ctx context.Context, tenant string, ev *pluginv1.TrackerEvent) error, log *slog.Logger) {
	run(ctx, cfg, reg, handle, log, initialBackoff, maxBackoff)
}

type tenantStream struct {
	keys   []string
	cancel context.CancelFunc
}

func run(ctx context.Context, cfg *config.Sync, reg *Registry,
	handle func(ctx context.Context, tenant string, ev *pluginv1.TrackerEvent) error, log *slog.Logger,
	initial, limit time.Duration) {
	var wg sync.WaitGroup
	streams := map[string]tenantStream{}
	apply := func() {
		want := projectKeys(cfg.Current())
		for tenant, s := range streams {
			if !slices.Equal(s.keys, want[tenant]) {
				s.cancel()
				delete(streams, tenant)
			}
		}
		for tenant, keys := range want {
			if _, ok := streams[tenant]; ok {
				continue
			}
			sctx, cancel := context.WithCancel(ctx)
			streams[tenant] = tenantStream{keys: keys, cancel: cancel}
			wg.Go(func() { ingestTenant(sctx, reg, tenant, keys, handle, log, initial, limit) })
		}
	}
	apply()
	for {
		select {
		case <-ctx.Done():
			wg.Wait() // streams derive from ctx and are already cancelled
			return
		case <-cfg.Updates():
			apply()
		}
	}
}

// projectKeys maps each tenant with projects to its sorted tracker keys.
func projectKeys(c *config.Config) map[string][]string {
	out := map[string][]string{}
	for name, t := range c.Tenants {
		for _, p := range t.Projects {
			out[name] = append(out[name], p.TrackerKey)
		}
		slices.Sort(out[name])
	}
	return out
}

func ingestTenant(ctx context.Context, reg *Registry, tenant string, keys []string,
	handle func(ctx context.Context, tenant string, ev *pluginv1.TrackerEvent) error, log *slog.Logger,
	initial, limit time.Duration) {
	backoff := initial
	for ctx.Err() == nil {
		received, err := streamOnce(ctx, reg, tenant, keys, handle, log)
		if received {
			backoff = initial
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("tracker stream ended, reconnecting", "tenant", tenant, "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, limit)
	}
}

// streamOnce reads one stream until it ends; received reports whether any
// event arrived.
func streamOnce(ctx context.Context, reg *Registry, tenant string, keys []string,
	handle func(ctx context.Context, tenant string, ev *pluginv1.TrackerEvent) error, log *slog.Logger) (received bool, err error) {
	c, err := reg.trackerClient(ctx, tenant)
	if err != nil {
		return false, err
	}
	stream, err := c.StreamEvents(ctx, &pluginv1.StreamEventsRequest{ProjectKeys: keys})
	if err != nil {
		return false, err
	}
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return received, nil
		}
		if err != nil {
			return received, err
		}
		received = true
		if herr := handle(ctx, tenant, ev); herr != nil {
			log.Error("handle tracker event", "tenant", tenant, "external_id", ev.GetExternalId(), "error", herr)
		}
	}
}
