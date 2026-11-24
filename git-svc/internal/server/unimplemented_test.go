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

// DiffService and OperationService are a contract only (ADR 0017): git-svc
// does not register them yet, so gRPC answers UNIMPLEMENTED, as diff.proto
// and operation.proto promise. Registering one of them makes this test fail
// on purpose: update the proto comments and this test with it.
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
	diff := gitv1.NewDiffServiceClient(conn)
	ops := gitv1.NewOperationServiceClient(conn)

	calls := map[string]func(context.Context) error{
		"CompareRefs": func(ctx context.Context) error {
			_, err := diff.CompareRefs(ctx, &gitv1.CompareRefsRequest{})
			return err
		},
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

	services := srv.grpc.GetServiceInfo()
	for _, name := range []string{
		gitv1.DiffService_ServiceDesc.ServiceName,
		gitv1.OperationService_ServiceDesc.ServiceName,
	} {
		if _, ok := services[name]; ok {
			t.Errorf("%s is registered; it is a contract only", name)
		}
	}
}
