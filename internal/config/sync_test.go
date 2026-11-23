package config

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

type flakySource struct {
	cfgs []*Config
	errs []error
	i    int
}

func (f *flakySource) Load(context.Context) (*Config, error) {
	i := f.i
	f.i++
	return f.cfgs[i], f.errs[i]
}

func TestSyncStartsWithEmptyConfig(t *testing.T) {
	s, err := NewSync(context.Background(), Static{}, time.Hour, slog.Default())
	if err != nil || len(s.Current().Tenants) != 0 {
		t.Fatalf("err=%v tenants=%d", err, len(s.Current().Tenants))
	}
}

func TestReloadFailureKeepsLastConfig(t *testing.T) {
	one := &Config{Tenants: map[string]Tenant{"acme": {Name: "acme"}}}
	src := &flakySource{cfgs: []*Config{one, nil}, errs: []error{nil, errors.New("db down")}}
	s, _ := NewSync(context.Background(), src, time.Hour, slog.Default())
	if err := s.Reload(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if _, ok := s.Current().Tenants["acme"]; !ok || s.LastError() == nil {
		t.Fatal("lost last valid config")
	}
}

func TestReloadSignalsUpdate(t *testing.T) {
	s, _ := NewSync(context.Background(), Static{}, time.Hour, slog.Default())
	_ = s.Reload(context.Background())
	select {
	case <-s.Updates():
	default:
		t.Fatal("no update signal")
	}
}
