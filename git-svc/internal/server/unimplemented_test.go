package server

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
)

// OperationService is a contract only (ADR 0017): git-svc does not register
// it yet, so gRPC answers UNIMPLEMENTED, as operation.proto promises.
// Registering it makes this test fail on purpose: update the proto comments
// and this test with it.
func TestContractOnlyServicesAreUnimplemented(t *testing.T) {
	srv := New(discard, nil, nil, Options{})
	lis, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(lis) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		if err := <-served; err != nil {
			t.Errorf("Serve() error = %v, want nil after Shutdown", err)
		}
	}()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ops := gitv1.NewOperationServiceClient(conn)

	calls := map[string]func(context.Context) error{
		"CheckMerge": func(ctx context.Context) error {
			_, err := ops.CheckMerge(ctx, &gitv1.CheckMergeRequest{})
			return err
		},
		"MergeBranches": func(ctx context.Context) error {
			_, err := ops.MergeBranches(ctx, &gitv1.MergeBranchesRequest{})
			return err
		},
	}
	for name, call := range calls {
		if code := status.Code(call(t.Context())); code != codes.Unimplemented {
			t.Errorf("%s: code = %v, want Unimplemented", name, code)
		}
	}

	if _, ok := srv.grpc.GetServiceInfo()[gitv1.OperationService_ServiceDesc.ServiceName]; ok {
		t.Errorf("%s is registered; it is a contract only", gitv1.OperationService_ServiceDesc.ServiceName)
	}
}
