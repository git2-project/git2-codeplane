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
)

func TestRunRejectsUnknownCommand(t *testing.T) {
	if code := run([]string{"bogus"}); code != 2 {
		t.Errorf("run(bogus) = %d, want 2", code)
	}
}

func TestRunFailsFastOnBadConfig(t *testing.T) {
	t.Setenv(config.EnvDatabaseURL, "")
	t.Setenv(config.EnvStorageRoot, "")
	if code := run(nil); code != 1 {
		t.Errorf("run() = %d, want 1", code)
	}
}

// TestServeStopsOnCancel covers the SIGTERM path: signal.NotifyContext
// cancels the context, and serve must drain and return nil.
func TestServeStopsOnCancel(t *testing.T) {
	cfg := config.Config{
		GRPCAddr:    freeAddr(t),
		DatabaseURL: "unused",
		StorageRoot: t.TempDir(),
		LogLevel:    slog.LevelInfo,
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, slog.New(slog.DiscardHandler), cfg) }()

	conn, err := grpc.NewClient(cfg.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitServing(t, healthpb.NewHealthClient(conn))

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve() error = %v, want nil", err)
		}
	case <-time.After(shutdownTimeout):
		t.Fatal("serve() did not return after cancel")
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

func freeAddr(t *testing.T) string {
	t.Helper()
	lis, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}
