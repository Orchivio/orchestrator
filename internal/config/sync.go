package config

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Source provides the current configuration (the database in production).
type Source interface {
	Load(ctx context.Context) (*Config, error)
}

// Static is a fixed Source; a nil Config means empty.
type Static struct{ Config *Config }

func (s Static) Load(context.Context) (*Config, error) { return s.Config, nil }

// Sync keeps the last valid Config and refreshes it from a Source.
type Sync struct {
	src      Source
	interval time.Duration
	log      *slog.Logger
	updates  chan struct{}

	reloadMu sync.Mutex // serializes Load+swap so an older load never overwrites a newer one
	mu       sync.RWMutex
	current  *Config
	lastErr  error
}

// NewSync performs the initial load, which must succeed. An empty config is valid.
func NewSync(ctx context.Context, src Source, interval time.Duration, log *slog.Logger) (*Sync, error) {
	cfg, err := src.Load(ctx)
	if err != nil {
		return nil, err
	}
	return &Sync{src: src, interval: interval, log: log, updates: make(chan struct{}, 1), current: orEmpty(cfg)}, nil
}

func orEmpty(c *Config) *Config {
	if c == nil {
		return &Config{Tenants: map[string]Tenant{}}
	}
	return c
}

func (s *Sync) Current() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

func (s *Sync) LastError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

// Updates signals (capacity 1, non-blocking) after every successful load.
func (s *Sync) Updates() <-chan struct{} { return s.updates }

// Run reloads every interval until ctx is done.
func (s *Sync) Run(ctx context.Context) {
	tick := time.NewTicker(s.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			_ = s.Reload(ctx) // failure is logged and kept in LastError
		}
	}
}

// Reload loads now. On error the previous config stays and the error is returned.
func (s *Sync) Reload(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	cfg, err := s.src.Load(ctx)
	s.mu.Lock()
	s.lastErr = err
	if err == nil {
		s.current = orEmpty(cfg)
	}
	s.mu.Unlock()
	if err != nil {
		s.log.Error("config reload failed, keeping last valid config", "error", err)
		return err
	}
	select {
	case s.updates <- struct{}{}:
	default:
	}
	return nil
}
