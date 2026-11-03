package adapters

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	pluginv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/plugin/v1"
	"github.com/soemii/orchivio-server/internal/config"
)

const (
	initialBackoff = time.Second
	maxBackoff     = 30 * time.Second
)

// Run reads the tracker event stream of every tenant in the config and hands
// each event to handle. It reconnects with backoff and returns on ctx done.
// ponytail: tenants added to the config later need a restart.
func Run(ctx context.Context, cfg *config.Sync, reg *Registry,
	handle func(ctx context.Context, tenant string, ev *pluginv1.TrackerEvent) error, log *slog.Logger) {
	run(ctx, cfg, reg, handle, log, initialBackoff, maxBackoff)
}

func run(ctx context.Context, cfg *config.Sync, reg *Registry,
	handle func(ctx context.Context, tenant string, ev *pluginv1.TrackerEvent) error, log *slog.Logger,
	initial, limit time.Duration) {
	var wg sync.WaitGroup
	for tenant := range cfg.Current().Tenants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ingestTenant(ctx, reg, tenant, handle, log, initial, limit)
		}()
	}
	wg.Wait()
}

func ingestTenant(ctx context.Context, reg *Registry, tenant string,
	handle func(ctx context.Context, tenant string, ev *pluginv1.TrackerEvent) error, log *slog.Logger,
	initial, limit time.Duration) {
	backoff := initial
	for ctx.Err() == nil {
		received, err := streamOnce(ctx, reg, tenant, handle, log)
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
func streamOnce(ctx context.Context, reg *Registry, tenant string,
	handle func(ctx context.Context, tenant string, ev *pluginv1.TrackerEvent) error, log *slog.Logger) (received bool, err error) {
	c, err := reg.trackerClient(ctx, tenant)
	if err != nil {
		return false, err
	}
	stream, err := c.StreamEvents(ctx, &pluginv1.StreamEventsRequest{})
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
