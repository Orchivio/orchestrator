package config

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Sync keeps the last valid Config and refreshes it periodically.
type Sync struct {
	dir      string
	interval time.Duration
	log      *slog.Logger

	mu      sync.RWMutex
	current *Config
	lastErr error
}

// NewSync performs the initial load, which must be valid.
func NewSync(dir string, interval time.Duration, log *slog.Logger) (*Sync, error) {
	cfg, err := Load(dir)
	if err != nil {
		return nil, err
	}
	return &Sync{dir: dir, interval: interval, log: log, current: cfg}, nil
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

// Run reloads every interval until ctx is done.
func (s *Sync) Run(ctx context.Context) {
	tick := time.NewTicker(s.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.reload()
		}
	}
}

// reload swaps in a freshly loaded config, or keeps the old one and records the error.
func (s *Sync) reload() {
	cfg, err := Load(s.dir)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = err
	if err != nil {
		s.log.Error("config reload failed, keeping last valid config", "error", err)
		return
	}
	s.current = cfg
}
