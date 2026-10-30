package server

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
)

var discard = slog.New(slog.DiscardHandler)

// These tests never call RepositoryService, so it needs no repo.Service.
// The RPCs are covered by the integration tests.

func TestHealthLifecycle(t *testing.T) {
	srv := New(discard, nil, Options{})
	lis, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := healthpb.NewHealthClient(conn)

	for _, service := range healthServices {
		resp, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: service})
		if err != nil {
			t.Fatalf("Check(%q) error = %v", service, err)
		}
		if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			t.Errorf("Check(%q) status = %v, want SERVING", service, resp.GetStatus())
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	if err := <-served; err != nil {
		t.Errorf("Serve() error = %v, want nil after Shutdown", err)
	}
}

func TestRegisteredServices(t *testing.T) {
	const reflectionService = "grpc.reflection.v1.ServerReflection"
	for _, dev := range []bool{false, true} {
		services := New(discard, nil, Options{Reflection: dev}).grpc.GetServiceInfo()
		if _, ok := services[reflectionService]; ok != dev {
			t.Errorf("Reflection=%v: reflection registered = %v", dev, ok)
		}
		for _, name := range []string{healthpb.Health_ServiceDesc.ServiceName, gitv1.RepositoryService_ServiceDesc.ServiceName} {
			if _, ok := services[name]; !ok {
				t.Errorf("Reflection=%v: %s not registered", dev, name)
			}
		}
	}
}

func TestRecoverUnary(t *testing.T) {
	interceptor := recoverUnary(discard)
	info := &grpc.UnaryServerInfo{FullMethod: "/test.v1.Test/Panic"}

	_, err := interceptor(t.Context(), nil, info, func(context.Context, any) (any, error) {
		panic("boom")
	})

	st, _ := status.FromError(err)
	if st.Code() != codes.Internal || st.Message() != "internal error" {
		t.Errorf("error = %v, want Internal 'internal error'", err)
	}
}
