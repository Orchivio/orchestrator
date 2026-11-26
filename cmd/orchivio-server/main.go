// Command orchivio-server wires config, store, router, orchestrator and the
// gRPC/HTTP endpoints.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	"github.com/orchivio/orchestrator/contract/secretref"
	"github.com/orchivio/orchestrator/internal/adapters"
	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/config"
	"github.com/orchivio/orchestrator/internal/grpcapi"
	"github.com/orchivio/orchestrator/internal/httpapi"
	"github.com/orchivio/orchestrator/internal/orchestrator"
	"github.com/orchivio/orchestrator/internal/reporter"
	"github.com/orchivio/orchestrator/internal/router"
	"github.com/orchivio/orchestrator/internal/secretbox"
	"github.com/orchivio/orchestrator/internal/store"
)

const (
	configReloadInterval = 30 * time.Second
	defaultTokenName     = "emergency"
	defaultTokenTTL      = 2160 * time.Hour // 90 days
	defaultLease         = 30 * time.Minute
	shutdownTimeout      = 10 * time.Second
	defaultPublicURL     = "http://localhost:8088"
	refreshInterval      = time.Minute
	secretKeyRefVar      = "ORCHIVIO_SECRET_KEY_REF"
	defaultSecretKeyRef  = "env://ORCHIVIO_SECRET_KEY"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var err error
	if len(os.Args) > 1 {
		err = runCommand(ctx, os.Args[1:], os.Stdout, log)
	} else {
		err = run(ctx, log)
	}
	if err != nil {
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
	// insecure gRPC, bind localhost; mTLS before remote workers.
	grpcAddr, httpAddr := env("ORCHIVIO_GRPC_ADDR", "127.0.0.1:7070"), env("ORCHIVIO_HTTP_ADDR", "127.0.0.1:8088")

	db, svc, connReg, err := openService(ctx, log)
	if err != nil {
		return err
	}
	defer db.Close()
	defer connReg.Close()
	if err := bootstrap(ctx, svc, os.Getenv("ORCHIVIO_BOOTSTRAP_TOKEN"), log); err != nil {
		return err
	}
	cfg, err := config.NewSync(ctx, svc, configReloadInterval, log)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	svc.OnChange(cfg.Reload)
	log.Info("config loaded", "tenants", len(cfg.Current().Tenants))
	reg := adapters.NewRegistry(cfg, log)
	defer reg.Close()
	// A rejected token takes effect now: marking reloads the config, which drops the connection's stream.
	reg.OnRejected(func(ctx context.Context, tenant string, id uuid.UUID) {
		if err := svc.MarkNeedsReauth(ctx, tenant, id); err != nil {
			log.Error("mark connection needs_reauth", "tenant", tenant, "connection", id, "error", err)
		}
	})

	var background sync.WaitGroup
	defer background.Wait() // runs before reg.Close and db.Close
	defer cancel()

	rep := reporter.New(func(t string) reporter.TrackerClient { return reg.TrackerFor(t) }, cfg)
	rt := router.New(cfg, db, reg.Tracker(), reg.CodeHost(), rep, time.Now, uuid.New)
	orch := orchestrator.New(cfg, db, reg.CodeHost(), reg.Tracker(), rep, rt.StartFollowUp, time.Now, defaultLease)

	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return fmt.Errorf("listen grpc %s: %w", grpcAddr, err)
	}
	gs := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(gs, grpcapi.NewWorker(orch))
	hs := &http.Server{Addr: httpAddr, Handler: newMux(svc, log), ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 2)
	background.Add(4)
	go func() { defer background.Done(); cfg.Run(ctx) }()
	go func() { defer background.Done(); adapters.Run(ctx, cfg, reg, rt.Handle, log) }()
	go func() { defer background.Done(); adapters.RunCodeHost(ctx, cfg, reg, rt.HandleReview, log) }()
	go func() { defer background.Done(); refreshConnections(ctx, svc, log) }()
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

// refreshConnections renews due connection credentials until ctx is done.
func refreshConnections(ctx context.Context, svc *admin.Service, log *slog.Logger) {
	tick := time.NewTicker(refreshInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := svc.RefreshDue(ctx); err != nil {
				log.Error("refresh connections", "error", err)
			}
		}
	}
}

// openService opens the store and builds the admin service from the
// environment. The returned registry backs the connector and must be closed.
func openService(ctx context.Context, log *slog.Logger) (*store.Store, *admin.Service, *adapters.Registry, error) {
	dbURL, spec := os.Getenv("DATABASE_URL"), os.Getenv("ORCHIVIO_ADAPTERS")
	if dbURL == "" || spec == "" {
		return nil, nil, nil, errors.New("DATABASE_URL and ORCHIVIO_ADAPTERS are required")
	}
	adapterAddrs, err := config.ParseAdapters(spec)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ORCHIVIO_ADAPTERS: %w", err)
	}
	box, err := openBox(os.LookupEnv)
	if err != nil {
		return nil, nil, nil, err
	}
	db, err := store.Open(ctx, dbURL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open store: %w", err)
	}
	// ponytail: the connector gets its own registry (no tenant config needed, the
	// service it serves builds that config); one more gRPC connection per adapter.
	connReg := adapters.NewRegistry(nil, log)
	svc := admin.NewService(db, adapterAddrs, box, connReg.Connector(adapterAddrs), env("ORCHIVIO_PUBLIC_URL", defaultPublicURL),
		time.Now, uuid.New, log)
	return db, svc, connReg, nil
}

// openBox resolves the secret key (ORCHIVIO_SECRET_KEY_REF, default
// env://ORCHIVIO_SECRET_KEY). It is always required; errors never contain the key.
func openBox(lookup func(string) (string, bool)) (*secretbox.Box, error) {
	ref, ok := lookup(secretKeyRefVar)
	if !ok || ref == "" {
		ref = defaultSecretKeyRef
	}
	key, err := secretref.Env{Lookup: lookup}.Resolve(ref)
	if err != nil {
		return nil, fmt.Errorf("ORCHIVIO_SECRET_KEY is required (via %s, default %s): %w", secretKeyRefVar, defaultSecretKeyRef, err)
	}
	box, err := secretbox.New(key)
	if err != nil {
		return nil, fmt.Errorf("ORCHIVIO_SECRET_KEY: %w", err)
	}
	return box, nil
}

// bootstrap installs the optional first admin token. Errors never contain the token.
func bootstrap(ctx context.Context, svc *admin.Service, token string, log *slog.Logger) error {
	if token == "" {
		return nil
	}
	installed, err := svc.Bootstrap(ctx, token)
	switch {
	case errors.Is(err, admin.ErrTaken):
		log.Warn("ORCHIVIO_BOOTSTRAP_TOKEN was installed concurrently by another replica")
	case err != nil:
		return fmt.Errorf("bootstrap token: %w", err)
	case !installed:
		log.Warn("ORCHIVIO_BOOTSTRAP_TOKEN is set but ignored: tokens already exist")
	}
	return nil
}

// runCommand handles "admin create-token"; only the token is written to out.
func runCommand(ctx context.Context, args []string, out io.Writer, log *slog.Logger) error {
	if len(args) < 2 || args[0] != "admin" || args[1] != "create-token" {
		return errors.New("usage: orchivio-server [admin create-token [-name NAME] [-expires-in DURATION]]")
	}
	fs := flag.NewFlagSet("admin create-token", flag.ContinueOnError)
	name := fs.String("name", defaultTokenName, "token name")
	ttl := fs.Duration("expires-in", defaultTokenTTL, "token lifetime")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	db, svc, connReg, err := openService(ctx, log)
	if err != nil {
		return err
	}
	defer db.Close()
	defer connReg.Close()
	_, plaintext, err := svc.IssueToken(ctx, *name, *ttl)
	if err != nil {
		return fmt.Errorf("issue token: %w", err)
	}
	_, err = fmt.Fprintln(out, plaintext)
	return err
}

// newMux serves the admin API and the OAuth callback; everything else is 404.
func newMux(svc *admin.Service, log *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", httpapi.NewAdmin(svc, log))
	mux.Handle("GET /oauth/callback/{adapter}", httpapi.NewCallback(svc, log))
	return mux
}
