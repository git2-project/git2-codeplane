// Command git-svc is git2's Git service: it stores bare repositories on disk
// and exposes them to other git2 services over gRPC.
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
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/git2-project/git2-codeplane/git-svc/internal/config"
	"github.com/git2-project/git2-codeplane/git-svc/internal/db"
	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
	"github.com/git2-project/git2-codeplane/git-svc/internal/server"
	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
)

// shutdownTimeout is how long in-flight RPCs get to finish on SIGTERM.
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

// serve runs the startup checks, then serves gRPC until ctx is canceled.
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
	srv := server.New(log, repos, server.Options{Reflection: cfg.Dev})

	lis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.GRPCAddr, err)
	}
	log.Info("serving gRPC", slog.String("addr", lis.Addr().String()), slog.Bool("dev", cfg.Dev))
	return serveUntilDone(ctx, log, srv, lis)
}

// serveUntilDone serves on lis until ctx is canceled, then shuts the server
// down gracefully.
func serveUntilDone(ctx context.Context, log *slog.Logger, srv *server.Server, lis net.Listener) error {
	served := make(chan error, 1)
	go func() { served <- srv.Serve(lis) }()

	select {
	case err := <-served:
		return fmt.Errorf("gRPC server stopped: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down", slog.String("timeout", shutdownTimeout.String()))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	log.Info("stopped")
	return nil
}
