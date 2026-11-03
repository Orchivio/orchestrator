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
	"syscall"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	workerv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/worker/v1"
	"github.com/soemii/orchivio-server/internal/adapters"
	"github.com/soemii/orchivio-server/internal/config"
	"github.com/soemii/orchivio-server/internal/grpcapi"
	"github.com/soemii/orchivio-server/internal/httpapi"
	"github.com/soemii/orchivio-server/internal/orchestrator"
	"github.com/soemii/orchivio-server/internal/reporter"
	"github.com/soemii/orchivio-server/internal/router"
	"github.com/soemii/orchivio-server/internal/store"
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

func run(ctx context.Context, log *slog.Logger) error {
	dbURL, configDir := os.Getenv("DATABASE_URL"), os.Getenv("ORCHIVIO_CONFIG_DIR")
	if dbURL == "" || configDir == "" {
		return errors.New("DATABASE_URL and ORCHIVIO_CONFIG_DIR are required")
	}
	grpcAddr, httpAddr := env("ORCHIVIO_GRPC_ADDR", ":7000"), env("ORCHIVIO_HTTP_ADDR", "127.0.0.1:8080")
	publicURL := env("ORCHIVIO_PUBLIC_URL", "http://localhost:8080")

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
	go cfg.Run(ctx)
	go func() { errc <- gs.Serve(lis) }()
	go func() { errc <- hs.ListenAndServe() }()
	go adapters.Run(ctx, cfg, reg, rt.Handle, log)
	log.Info("server started", "grpc", grpcAddr, "http", httpAddr)

	select {
	case err = <-errc:
		err = fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	gs.GracefulStop()
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return errors.Join(err, hs.Shutdown(shutCtx))
}
