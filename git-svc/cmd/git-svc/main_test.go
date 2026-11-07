package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/git2-project/git2-codeplane/git-svc/internal/config"
	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/server"
	"github.com/git2-project/git2-codeplane/git-svc/internal/smarthttp"
	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
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
// both servers and return nil. The full startup with a database is covered
// by the integration tests.
func TestServeUntilDoneStopsOnCancel(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	grpcLis, httpLis := listen(t), listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- serveUntilDone(ctx, log,
			listener{name: "gRPC", srv: server.New(log, nil, nil, server.Options{}), lis: grpcLis},
			listener{name: "HTTP", srv: newHTTPServer(t, log), lis: httpLis})
	}()

	conn, err := grpc.NewClient(grpcLis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitServing(t, healthpb.NewHealthClient(conn))
	waitHealthz(t, "http://"+httpLis.Addr().String()+"/healthz")

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

// When one server fails, the others are shut down and the error returned,
// so that the process exits instead of serving half its API.
func TestServeUntilDoneStopsWhenAServerFails(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	broken := listen(t)
	_ = broken.Close() // Serve fails at once

	done := make(chan error, 1)
	go func() {
		done <- serveUntilDone(t.Context(), log,
			listener{name: "gRPC", srv: server.New(log, nil, nil, server.Options{}), lis: listen(t)},
			listener{name: "HTTP", srv: newHTTPServer(t, log), lis: broken})
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "HTTP server stopped") {
			t.Errorf("serveUntilDone() error = %v, want the HTTP server's failure", err)
		}
	case <-time.After(shutdownTimeout):
		t.Fatal("serveUntilDone() did not return after a server failed")
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	lis, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return lis
}

// newHTTPServer returns an HTTP server whose Git routes are not used.
func newHTTPServer(t *testing.T, log *slog.Logger) *smarthttp.Server {
	t.Helper()
	layout := storage.NewLayout(t.TempDir())
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	git, err := gitcmd.New(layout.HomeDir())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := smarthttp.New(smarthttp.Options{Log: log, Layout: layout, Git: git})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func waitHealthz(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("HTTP server did not answer /healthz")
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
