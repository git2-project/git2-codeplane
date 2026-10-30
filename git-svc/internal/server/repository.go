package server

import (
	"context"
	"log/slog"

	"google.golang.org/protobuf/types/known/timestamppb"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

// repositoryServer implements gitv1.RepositoryServiceServer on top of
// repo.Service. It only translates between protobuf and Go types; the rules
// live in package repo.
type repositoryServer struct {
	// Embedded so that RPCs added to the contract later answer
	// UNIMPLEMENTED instead of breaking the build.
	gitv1.UnimplementedRepositoryServiceServer

	repos *repo.Service
	log   *slog.Logger
}

func (s *repositoryServer) CreateRepository(ctx context.Context, req *gitv1.CreateRepositoryRequest) (*gitv1.CreateRepositoryResponse, error) {
	ownerID, err := repo.ParseID("owner_id", req.GetOwnerId())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	r, err := s.repos.Create(ctx, repo.CreateParams{
		OwnerID: ownerID,
		Name:    req.GetName(),
		// Fields, not getters: the getters turn "not set" into "".
		DefaultBranch: req.DefaultBranch,
		Description:   req.Description,
	})
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	return &gitv1.CreateRepositoryResponse{Repository: toProto(r)}, nil
}

func (s *repositoryServer) GetRepository(ctx context.Context, req *gitv1.GetRepositoryRequest) (*gitv1.GetRepositoryResponse, error) {
	id, err := repo.ParseID("id", req.GetId())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	r, err := s.repos.Get(ctx, id)
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	return &gitv1.GetRepositoryResponse{Repository: toProto(r)}, nil
}

func (s *repositoryServer) FindRepository(ctx context.Context, req *gitv1.FindRepositoryRequest) (*gitv1.FindRepositoryResponse, error) {
	ownerID, err := repo.ParseID("owner_id", req.GetOwnerId())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	r, err := s.repos.Find(ctx, ownerID, req.GetName())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	return &gitv1.FindRepositoryResponse{Repository: toProto(r)}, nil
}

func (s *repositoryServer) ListRepositories(ctx context.Context, req *gitv1.ListRepositoriesRequest) (*gitv1.ListRepositoriesResponse, error) {
	ownerID, err := repo.ParseID("owner_id", req.GetOwnerId())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	page, err := s.repos.List(ctx, repo.ListParams{
		OwnerID:   ownerID,
		PageSize:  req.GetPageSize(),
		PageToken: req.GetPageToken(),
	})
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	resp := &gitv1.ListRepositoriesResponse{
		Repositories:  make([]*gitv1.Repository, 0, len(page.Repositories)),
		NextPageToken: page.NextPageToken,
	}
	for _, r := range page.Repositories {
		resp.Repositories = append(resp.Repositories, toProto(r))
	}
	return resp, nil
}

func (s *repositoryServer) DeleteRepository(ctx context.Context, req *gitv1.DeleteRepositoryRequest) (*gitv1.DeleteRepositoryResponse, error) {
	id, err := repo.ParseID("id", req.GetId())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	if err := s.repos.Delete(ctx, id); err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	return &gitv1.DeleteRepositoryResponse{}, nil
}

func toProto(r repo.Repository) *gitv1.Repository {
	return &gitv1.Repository{
		Id:            r.ID.String(),
		OwnerId:       r.OwnerID.String(),
		Name:          r.Name,
		DefaultBranch: r.DefaultBranch,
		Description:   r.Description,
		CreatedAt:     timestamppb.New(r.CreatedAt),
		UpdatedAt:     timestamppb.New(r.UpdatedAt),
	}
}
