// Command git-svc is git2's Git service: it stores bare repositories on disk
// and exposes them to other git2 services over gRPC, and to git clients
// over Git's smart HTTP protocol (behind the gateway).
//
// Usage:
//
//	git-svc [serve]
//
// Configuration comes from environment variables; see package config. The
// database schema comes from the git2 repo's migrations: git-svc checks
// that it is there but never changes it.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/git2-project/git2-codeplane/git-svc/internal/config"
	"github.com/git2-project/git2-codeplane/git-svc/internal/db"
	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
	"github.com/git2-project/git2-codeplane/git-svc/internal/server"
	"github.com/git2-project/git2-codeplane/git-svc/internal/smarthttp"
	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
)

// shutdownTimeout is how long in-flight RPCs, clones and pushes get to
// finish on SIGTERM.
const shutdownTimeout = 20 * time.Second

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if len(args) > 0 && args[0] != "serve" {
		log.Error("unknown command", slog.String("command", args[0]), slog.String("usage", "git-svc [serve]"))
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		log.Error("invalid configuration", slog.String("err", err.Error()))
		return 1
	}
	log = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx, log, cfg); err != nil {
		log.Error("git-svc failed", slog.String("err", err.Error()))
		return 1
	}
	return 0
}

// serve runs the startup checks, then serves gRPC and HTTP until ctx is
// canceled.
func serve(ctx context.Context, log *slog.Logger, cfg config.Config) error {
	layout := storage.NewLayout(cfg.StorageRoot)
	if err := layout.EnsureDirs(); err != nil {
		return err
	}

	git, err := gitcmd.New(layout.HomeDir())
	if err != nil {
		return err
	}
	version, err := git.CheckVersion(ctx, gitcmd.MinVersion)
	if err != nil {
		return err
	}
	log.Info("git found", slog.String("path", git.Path()), slog.String("version", version.String()))

	pool, err := db.Open(ctx, cfg.PostgresDSN, cfg.PostgresMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.CheckSchema(ctx, pool); err != nil {
		return err
	}
	log.Info("database ready")

	repos := repo.NewService(pool, storage.NewStore(layout, git), log)
	grpcSrv := server.New(log, repos, server.Options{Reflection: cfg.Dev})
	httpSrv, err := smarthttp.New(smarthttp.Options{
		Log:    log,
		Layout: layout,
		Git:    git,
		Repos:  repos,
		Pushes: repos,
		Ready:  pool.Ping,
	})
	if err != nil {
		return err
	}

	listen := (&net.ListenConfig{}).Listen
	grpcLis, err := listen(ctx, "tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.GRPCAddr, err)
	}
	httpLis, err := listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		_ = grpcLis.Close()
		return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}
	log.Info("serving",
		slog.String("grpc_addr", grpcLis.Addr().String()),
		slog.String("http_addr", httpLis.Addr().String()),
		slog.Bool("dev", cfg.Dev))
	return serveUntilDone(ctx, log,
		listener{name: "gRPC", srv: grpcSrv, lis: grpcLis},
		listener{name: "HTTP", srv: httpSrv, lis: httpLis})
}

// servable is what serveUntilDone runs: server.Server and smarthttp.Server.
type servable interface {
	// Serve serves on lis until Shutdown; then it returns nil.
	Serve(lis net.Listener) error
	// Shutdown waits for in-flight requests until ctx ends.
	Shutdown(ctx context.Context)
}

// listener is a server and the listener it serves on.
type listener struct {
	name string
	srv  servable
	lis  net.Listener
}

// serveUntilDone runs every server until ctx is canceled or one of them
// fails, then shuts them all down gracefully.
func serveUntilDone(ctx context.Context, log *slog.Logger, listeners ...listener) error {
	served := make(chan error, len(listeners))
	for _, l := range listeners {
		go func() {
			if err := l.srv.Serve(l.lis); err != nil {
				served <- fmt.Errorf("%s server stopped: %w", l.name, err)
				return
			}
			served <- nil
		}()
	}

	var failed error
	select {
	case failed = <-served:
		if failed == nil {
			failed = errors.New("a server stopped unexpectedly")
		}
	case <-ctx.Done():
	}

	log.Info("shutting down", slog.String("timeout", shutdownTimeout.String()))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, l := range listeners {
		wg.Go(func() { l.srv.Shutdown(shutdownCtx) })
	}
	wg.Wait()
	log.Info("stopped")
	return failed
}
