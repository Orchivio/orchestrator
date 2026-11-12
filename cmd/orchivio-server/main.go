// Command orchivio-server wires config, store, router, orchestrator and the
// gRPC/HTTP endpoints.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	workerv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/worker/v1"
	"github.com/orchivio/orchestrator/internal/adapters"
	"github.com/orchivio/orchestrator/internal/config"
	"github.com/orchivio/orchestrator/internal/grpcapi"
	"github.com/orchivio/orchestrator/internal/httpapi"
	"github.com/orchivio/orchestrator/internal/orchestrator"
	"github.com/orchivio/orchestrator/internal/reporter"
	"github.com/orchivio/orchestrator/internal/router"
	"github.com/orchivio/orchestrator/internal/store"
)

const (
	configReloadInterval = 30 * time.Second
	defaultLease         = 30 * time.Minute
	shutdownTimeout      = 10 * time.Second
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log); err != nil {
		log.Error("server failed", "error", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run(parent context.Context, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	dbURL, configDir := os.Getenv("DATABASE_URL"), os.Getenv("ORCHIVIO_CONFIG_DIR")
	if dbURL == "" || configDir == "" {
		return errors.New("DATABASE_URL and ORCHIVIO_CONFIG_DIR are required")
	}
	// insecure gRPC, bind localhost; mTLS before remote workers.
	grpcAddr, httpAddr := env("ORCHIVIO_GRPC_ADDR", "127.0.0.1:7070"), env("ORCHIVIO_HTTP_ADDR", "127.0.0.1:8088")
	publicURL := env("ORCHIVIO_PUBLIC_URL", "http://localhost:8088")

	cfg, err := config.NewSync(configDir, configReloadInterval, log)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	log.Info("config loaded", "tenants", len(cfg.Current().Tenants))
	db, err := store.Open(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer db.Close()
	reg := adapters.NewRegistry(cfg, log)
	defer reg.Close()

	var background sync.WaitGroup
	defer background.Wait() // runs before reg.Close and db.Close
	defer cancel()

	rep := reporter.New(func(t string) reporter.TrackerClient { return reg.TrackerFor(t) }, cfg, publicURL)
	rt := router.New(cfg, db, reg.Tracker(), rep, time.Now, uuid.New)
	orch := orchestrator.New(cfg, db, reg.CodeHost(), reg.Tracker(), rep, rt.StartFollowUp, time.Now, defaultLease, publicURL)

	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return fmt.Errorf("listen grpc %s: %w", grpcAddr, err)
	}
	gs := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(gs, grpcapi.NewWorker(orch))
	hs := &http.Server{Addr: httpAddr, Handler: httpapi.NewTimeline(db), ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 2)
	background.Add(2)
	go func() { defer background.Done(); cfg.Run(ctx) }()
	go func() { defer background.Done(); adapters.Run(ctx, cfg, reg, rt.Handle, log) }()
	go func() { errc <- gs.Serve(lis) }()
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("server started", "grpc", grpcAddr, "http", httpAddr)

	select {
	case err = <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			err = fmt.Errorf("serve: %w", err)
		} else {
			err = nil
		}
	case <-ctx.Done():
	}
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutCancel()
	stopped := make(chan struct{})
	go func() { gs.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-shutCtx.Done():
		gs.Stop()
	}
	return errors.Join(err, hs.Shutdown(shutCtx))
}
