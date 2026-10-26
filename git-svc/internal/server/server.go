// Package server wires git-svc's gRPC services, interceptors and the
// mapping from domain errors to gRPC status codes.
package server

import (
	"context"
	"log/slog"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

// healthServices are the names health reports on: "" for the whole server,
// plus each gRPC service git-svc implements.
var healthServices = []string{"", gitv1.RepositoryService_ServiceDesc.ServiceName}

// Options configure New.
type Options struct {
	// Reflection registers the gRPC server reflection service. Dev only.
	Reflection bool
}

// Server is git-svc's gRPC server.
type Server struct {
	grpc   *grpc.Server
	health *health.Server
}

// New builds the gRPC server with its interceptors, RepositoryService
// backed by repos, and the health service. Health reports NOT_SERVING until
// Serve is called.
func New(log *slog.Logger, repos *repo.Service, opts Options) *Server {
	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(logUnary(log), recoverUnary(log)),
		grpc.ChainStreamInterceptor(recoverStream(log)),
	)

	gitv1.RegisterRepositoryServiceServer(gs, &repositoryServer{repos: repos, log: log})

	hs := health.NewServer()
	for _, name := range healthServices {
		hs.SetServingStatus(name, healthpb.HealthCheckResponse_NOT_SERVING)
	}
	healthpb.RegisterHealthServer(gs, hs)

	if opts.Reflection {
		reflection.Register(gs)
	}
	return &Server{grpc: gs, health: hs}
}

// Serve marks the server healthy and serves on lis until Shutdown is called.
func (s *Server) Serve(lis net.Listener) error {
	for _, name := range healthServices {
		s.health.SetServingStatus(name, healthpb.HealthCheckResponse_SERVING)
	}
	return s.grpc.Serve(lis)
}

// Shutdown marks the server unhealthy, then waits for in-flight RPCs to
// finish. If ctx ends first, the remaining RPCs are canceled.
func (s *Server) Shutdown(ctx context.Context) {
	s.health.Shutdown()

	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.grpc.Stop()
		<-done
	}
}
