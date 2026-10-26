package main

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/git2-project/git2-codeplane/git-svc/internal/config"
	"github.com/git2-project/git2-codeplane/git-svc/internal/server"
)

func TestRunRejectsUnknownCommand(t *testing.T) {
	if code := run([]string{"bogus"}); code != 2 {
		t.Errorf("run(bogus) = %d, want 2", code)
	}
}

func TestRunFailsFastOnBadConfig(t *testing.T) {
	t.Setenv(config.EnvPostgresDSN, "")
	t.Setenv(config.EnvStorageRoot, "")
	for _, args := range [][]string{nil, {"serve"}} {
		if code := run(args); code != 1 {
			t.Errorf("run(%q) = %d, want 1", args, code)
		}
	}
}

// git-svc no longer applies migrations: schema changes go through the git2
// repo (ADR 0006).
func TestRunHasNoMigrateCommand(t *testing.T) {
	if code := run([]string{"migrate"}); code != 2 {
		t.Errorf("run(migrate) = %d, want 2 (unknown command)", code)
	}
}

// TestServeUntilDoneStopsOnCancel covers the SIGTERM path:
// signal.NotifyContext cancels the context, and serveUntilDone must drain
// the server and return nil. The full startup with a database is covered
// by the integration tests.
func TestServeUntilDoneStopsOnCancel(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	lis, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serveUntilDone(ctx, log, server.New(log, nil, server.Options{}), lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitServing(t, healthpb.NewHealthClient(conn))

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveUntilDone() error = %v, want nil", err)
		}
	case <-time.After(shutdownTimeout):
		t.Fatal("serveUntilDone() did not return after cancel")
	}
}

func waitServing(t *testing.T, client healthpb.HealthClient) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{})
		if err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("server did not become SERVING")
}
