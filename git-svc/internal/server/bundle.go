package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"

	"google.golang.org/grpc"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

// bundleChunk is the most bundle data one CreateBundleResponse carries.
const bundleChunk = 64 << 10

// bundleServer implements gitv1.BundleServiceServer on top of repo.Service
// (ADR 0015).
type bundleServer struct {
	gitv1.UnimplementedBundleServiceServer

	repos *repo.Service
	log   *slog.Logger
}

func (s *bundleServer) ListAllRepositories(ctx context.Context, req *gitv1.ListAllRepositoriesRequest) (*gitv1.ListAllRepositoriesResponse, error) {
	page, err := s.repos.ListAll(ctx, repo.ListAllParams{PageSize: req.GetPageSize(), PageToken: req.GetPageToken()})
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	resp := &gitv1.ListAllRepositoriesResponse{
		Repositories:  make([]*gitv1.Repository, 0, len(page.Repositories)),
		NextPageToken: page.NextPageToken,
	}
	for _, r := range page.Repositories {
		resp.Repositories = append(resp.Repositories, toProto(r))
	}
	return resp, nil
}

func (s *bundleServer) CreateBundle(req *gitv1.CreateBundleRequest, stream grpc.ServerStreamingServer[gitv1.CreateBundleResponse]) error {
	ctx := stream.Context()
	id, err := repo.ParseID("repository_id", req.GetRepositoryId())
	if err != nil {
		return toStatus(ctx, s.log, err)
	}
	// git writes in small pieces; the buffer turns them into messages of
	// bundleChunk bytes.
	w := bufio.NewWriterSize(&bundleSender{stream: stream}, bundleChunk)
	empty, err := s.repos.WriteBundle(ctx, id, w)
	if err != nil {
		return toStatus(ctx, s.log, streamError(ctx, err))
	}
	if empty {
		return stream.Send(&gitv1.CreateBundleResponse{Empty: true})
	}
	if err := w.Flush(); err != nil {
		return toStatus(ctx, s.log, streamError(ctx, err))
	}
	return nil
}

// bundleSender sends each write as one CreateBundleResponse.
type bundleSender struct {
	stream grpc.ServerStreamingServer[gitv1.CreateBundleResponse]
}

func (b *bundleSender) Write(p []byte) (int, error) {
	// bufio reuses p, so the message gets its own copy.
	if err := b.stream.Send(&gitv1.CreateBundleResponse{Data: append([]byte(nil), p...)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *bundleServer) RestoreBundle(stream grpc.ClientStreamingServer[gitv1.RestoreBundleRequest, gitv1.RestoreBundleResponse]) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		return toStatus(ctx, s.log, &repo.ValidationError{Field: "repository_id", Reason: "is required"})
	}
	if err != nil {
		return toStatus(ctx, s.log, streamError(ctx, err))
	}
	id, err := repo.ParseID("repository_id", first.GetRepositoryId())
	if err != nil {
		return toStatus(ctx, s.log, err)
	}
	data := &bundleReceiver{ctx: ctx, stream: stream, pending: first.GetData()}
	if err := s.repos.RestoreBundle(ctx, id, data); err != nil {
		return toStatus(ctx, s.log, err)
	}
	return stream.SendAndClose(&gitv1.RestoreBundleResponse{})
}

// bundleReceiver reads the bundle data of RestoreBundle's stream, message
// by message, starting with the first message's data.
type bundleReceiver struct {
	ctx     context.Context // the stream's context, to tell why Recv failed
	stream  grpc.ClientStreamingServer[gitv1.RestoreBundleRequest, gitv1.RestoreBundleResponse]
	pending []byte
}

func (r *bundleReceiver) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		msg, err := r.stream.Recv()
		if err != nil {
			return 0, streamError(r.ctx, err) // io.EOF at the end of the stream
		}
		if msg.GetRepositoryId() != "" {
			return 0, &repo.ValidationError{Field: "repository_id", Reason: "must be set in the first message only"}
		}
		r.pending = msg.GetData()
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// streamError returns the context's error if a stream failed because the
// call ended, so that toStatus reports it as such instead of INTERNAL.
func streamError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, io.EOF) {
		return ctxErr
	}
	return err
}
