package adapters

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	"github.com/orchivio/orchestrator/internal/config"
)

const (
	initialBackoff = time.Second
	maxBackoff     = 30 * time.Second
)

// Run reads the tracker event stream of every tenant with projects and an
// active tracker connection and hands each event to handle. It follows config
// changes: a tenant's stream is (re)started when its project keys or its
// tracker credential change and stopped when it has no keys or credential left.
// Each stream reconnects with backoff. Run returns on ctx done once all
// streams have stopped.
func Run(ctx context.Context, cfg *config.Sync, reg *Registry,
	handle func(ctx context.Context, tenant string, ev *pluginv1.TrackerEvent) error, log *slog.Logger) {
	run(ctx, cfg, reg, handle, log, initialBackoff, maxBackoff)
}

// streamKey identifies what a running stream was started with; the
// credential only as its hash.
type streamKey struct {
	keys []string
	cred [sha256.Size]byte
}

func (k streamKey) equal(o streamKey) bool { return k.cred == o.cred && slices.Equal(k.keys, o.keys) }

type tenantStream struct {
	key    streamKey
	cancel context.CancelFunc
}

func run(ctx context.Context, cfg *config.Sync, reg *Registry,
	handle func(ctx context.Context, tenant string, ev *pluginv1.TrackerEvent) error, log *slog.Logger,
	initial, limit time.Duration) {
	var wg sync.WaitGroup
	streams := map[string]tenantStream{}
	apply := func() {
		want := streamKeys(cfg.Current())
		for tenant, s := range streams {
			if w, ok := want[tenant]; !ok || !s.key.equal(w) {
				s.cancel()
				delete(streams, tenant)
			}
		}
		for tenant, key := range want {
			if _, ok := streams[tenant]; ok {
				continue
			}
			sctx, cancel := context.WithCancel(ctx)
			streams[tenant] = tenantStream{key: key, cancel: cancel}
			wg.Go(func() { ingestTenant(ctx, sctx, reg, tenant, key.keys, handle, log, initial, limit) })
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

// streamKeys maps each tenant with projects and a tracker credential to its
// sorted tracker keys and credential hash.
func streamKeys(c *config.Config) map[string]streamKey {
	out := map[string]streamKey{}
	for name, t := range c.Tenants {
		if len(t.Projects) == 0 || t.TrackerCredential == nil {
			continue
		}
		k := streamKey{cred: sha256.Sum256(t.TrackerCredential)}
		for _, p := range t.Projects {
			k.keys = append(k.keys, p.TrackerKey)
		}
		slices.Sort(k.keys)
		out[name] = k
	}
	return out
}

// ingestTenant reconnects until ctx (the stream's) ends. Events are handled
// with runCtx, so a key change that cancels ctx never aborts a half-handled event.
func ingestTenant(runCtx, ctx context.Context, reg *Registry, tenant string, keys []string,
	handle func(ctx context.Context, tenant string, ev *pluginv1.TrackerEvent) error, log *slog.Logger,
	initial, limit time.Duration) {
	backoff := initial
	for ctx.Err() == nil {
		received, err := streamOnce(runCtx, ctx, reg, tenant, keys, handle, log)
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
func streamOnce(runCtx, ctx context.Context, reg *Registry, tenant string, keys []string,
	handle func(ctx context.Context, tenant string, ev *pluginv1.TrackerEvent) error, log *slog.Logger) (received bool, err error) {
	c, cred, id, err := reg.trackerClient(ctx, tenant)
	if err != nil {
		return false, err
	}
	stream, err := c.StreamEvents(ctx, &pluginv1.StreamEventsRequest{ProjectKeys: keys, Credential: cred})
	if err != nil {
		return false, reg.rejected(ctx, tenant, []uuid.UUID{id}, err)
	}
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return received, nil
		}
		if err != nil {
			return received, reg.rejected(ctx, tenant, []uuid.UUID{id}, err)
		}
		received = true
		if herr := handle(runCtx, tenant, ev); herr != nil {
			log.Error("handle tracker event", "tenant", tenant, "external_id", ev.GetExternalId(), "error", herr)
		}
	}
}
