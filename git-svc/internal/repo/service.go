// Package repo implements repository management: input validation and the
// create, get, find, list and delete flows.
//
// Metadata lives in PostgreSQL (git_svc.repositories), the repositories
// themselves on disk (package storage), and every change records an event
// in the outbox in the same transaction.
package repo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
	"github.com/git2-project/git2-codeplane/git-svc/internal/db"
	"github.com/git2-project/git2-codeplane/git-svc/internal/outbox"
	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
)

// Repository is the metadata of a live repository.
type Repository struct {
	ID            uuid.UUID
	OwnerID       uuid.UUID
	Name          string
	DefaultBranch string
	Description   string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Service creates, reads and deletes repositories.
type Service struct {
	pool  *pgxpool.Pool
	store *storage.Store
	log   *slog.Logger
}

// NewService returns a Service that keeps metadata in pool and
// repositories in store.
func NewService(pool *pgxpool.Pool, store *storage.Store, log *slog.Logger) *Service {
	return &Service{pool: pool, store: store, log: log}
}

// nameIndex is the unique index that enforces one live repository per
// owner and case-insensitive name (migrations/002_git_repositories.sql).
const nameIndex = "repositories_owner_name_live_uq"

// columns are the repository columns, in the order scanRepository reads
// them.
const columns = `id, owner_id, name, default_branch, description, created_at, updated_at`

const (
	insertSQL = `
INSERT INTO git_svc.repositories (id, owner_id, name, default_branch, description)
VALUES ($1, $2, $3, $4, $5)
RETURNING created_at, updated_at`

	getSQL = `
SELECT ` + columns + `
FROM git_svc.repositories
WHERE id = $1 AND deleted_at IS NULL`

	// lower(name) on both sides matches the unique index.
	findSQL = `
SELECT ` + columns + `
FROM git_svc.repositories
WHERE owner_id = $1 AND lower(name) = lower($2) AND deleted_at IS NULL`

	// COLLATE "C" sorts by bytes, so the order is the same whatever
	// collation the database was created with.
	listFirstSQL = `
SELECT ` + columns + `
FROM git_svc.repositories
WHERE owner_id = $1 AND deleted_at IS NULL
ORDER BY lower(name) COLLATE "C", id
LIMIT $2`

	listAfterSQL = `
SELECT ` + columns + `
FROM git_svc.repositories
WHERE owner_id = $1 AND deleted_at IS NULL
  AND (lower(name) COLLATE "C", id) > ($2::text, $3::uuid)
ORDER BY lower(name) COLLATE "C", id
LIMIT $4`

	deleteSQL = `
UPDATE git_svc.repositories
SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND deleted_at IS NULL
RETURNING owner_id, name, deleted_at`
)

// CreateParams is the input of Create.
type CreateParams struct {
	OwnerID       uuid.UUID
	Name          string
	DefaultBranch *string // nil means DefaultBranch
	Description   *string // nil means empty
}

// Create validates p, creates an empty bare repository on disk and records
// it, together with a RepositoryCreated event, in one transaction.
func (s *Service) Create(ctx context.Context, p CreateParams) (Repository, error) {
	r := Repository{OwnerID: p.OwnerID, Name: p.Name, DefaultBranch: DefaultBranch}
	if p.DefaultBranch != nil {
		r.DefaultBranch = *p.DefaultBranch
	}
	if p.Description != nil {
		r.Description = *p.Description
	}
	if err := ValidateName(r.Name); err != nil {
		return Repository{}, err
	}
	if err := ValidateBranch(r.DefaultBranch); err != nil {
		return Repository{}, err
	}
	if err := ValidateDescription(r.Description); err != nil {
		return Repository{}, err
	}
	r.ID = uuid.New()

	// Disk first, then the database: a row never points to a missing
	// directory.
	if err := s.store.Init(ctx, r.ID, r.DefaultBranch); err != nil {
		return Repository{}, fmt.Errorf("create repository %s on disk: %w", r.ID, err)
	}
	// TODO(sweeper): if the process dies between Init and the commit below,
	// the directory has no row. A sweeper will remove such orphans
	// (ADR 0005).

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, insertSQL, r.ID, r.OwnerID, r.Name, r.DefaultBranch, r.Description).
			Scan(&r.CreatedAt, &r.UpdatedAt)
		if err != nil {
			return fmt.Errorf("insert repository: %w", err)
		}
		return outbox.Write(ctx, tx, outbox.AggregateRepository, r.ID, &gitv1.RepositoryCreated{
			Id:         r.ID.String(),
			OwnerId:    r.OwnerID.String(),
			Name:       r.Name,
			OccurredAt: timestamppb.New(r.CreatedAt),
		})
	})
	if err != nil {
		if rmErr := s.store.Remove(r.ID); rmErr != nil {
			s.log.ErrorContext(ctx, "remove directory of repository that was not created",
				slog.String("repository_id", r.ID.String()), slog.String("err", rmErr.Error()))
		}
		if db.IsUniqueViolation(err, nameIndex) {
			return Repository{}, fmt.Errorf("%w: %q", ErrAlreadyExists, r.Name)
		}
		return Repository{}, err
	}
	return r, nil
}

// Get returns the live repository with the given ID.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Repository, error) {
	r, err := scanRepository(s.pool.QueryRow(ctx, getSQL, id))
	if err != nil {
		return Repository{}, fmt.Errorf("get repository %s: %w", id, err)
	}
	return r, nil
}

// Find returns the owner's live repository with the given name, compared
// case-insensitively.
func (s *Service) Find(ctx context.Context, ownerID uuid.UUID, name string) (Repository, error) {
	if name == "" {
		return Repository{}, invalid("name", "is required")
	}
	r, err := scanRepository(s.pool.QueryRow(ctx, findSQL, ownerID, name))
	if err != nil {
		return Repository{}, fmt.Errorf("find repository %q: %w", name, err)
	}
	return r, nil
}

// ListParams is the input of List.
type ListParams struct {
	OwnerID   uuid.UUID
	PageSize  int32  // 0 means the default page size
	PageToken string // empty for the first page
}

// Page is one page of List results.
type Page struct {
	Repositories  []Repository
	NextPageToken string // empty on the last page
}

// List returns one page of the owner's live repositories, sorted by name
// (case-insensitive).
func (s *Service) List(ctx context.Context, p ListParams) (Page, error) {
	size, err := pageSize(p.PageSize)
	if err != nil {
		return Page{}, err
	}

	// One extra row tells whether another page follows.
	var rows pgx.Rows
	if p.PageToken == "" {
		rows, err = s.pool.Query(ctx, listFirstSQL, p.OwnerID, size+1)
	} else {
		c, decodeErr := decodePageToken(p.PageToken, p.OwnerID)
		if decodeErr != nil {
			return Page{}, decodeErr
		}
		rows, err = s.pool.Query(ctx, listAfterSQL, p.OwnerID, c.NameKey, c.ID, size+1)
	}
	if err != nil {
		return Page{}, fmt.Errorf("list repositories: %w", err)
	}
	repos, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Repository, error) {
		return scanRepository(row)
	})
	if err != nil {
		return Page{}, fmt.Errorf("list repositories: %w", err)
	}

	page := Page{Repositories: repos}
	if len(repos) > size {
		page.Repositories = repos[:size]
		last := page.Repositories[size-1]
		// Names are ASCII, so strings.ToLower matches PostgreSQL's lower().
		page.NextPageToken = encodePageToken(cursor{OwnerID: p.OwnerID, NameKey: strings.ToLower(last.Name), ID: last.ID})
	}
	return page, nil
}

// Delete soft-deletes the repository and records a RepositoryDeleted event
// in one transaction, then moves its directory to the trash.
//
// Once the transaction commits, the delete has happened: if moving the
// directory fails, the error is logged and Delete still succeeds. A retry
// would only get ErrNotFound, and the sweeper moves leftovers later
// (ADR 0005).
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			ownerID   uuid.UUID
			name      string
			deletedAt time.Time
		)
		err := tx.QueryRow(ctx, deleteSQL, id).Scan(&ownerID, &name, &deletedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("mark repository deleted: %w", err)
		}
		return outbox.Write(ctx, tx, outbox.AggregateRepository, id, &gitv1.RepositoryDeleted{
			Id:         id.String(),
			OwnerId:    ownerID.String(),
			Name:       name,
			OccurredAt: timestamppb.New(deletedAt),
		})
	})
	if err != nil {
		return fmt.Errorf("delete repository %s: %w", id, err)
	}

	if err := s.store.Trash(id); err != nil {
		// TODO(sweeper): move directories of deleted repositories to the trash.
		s.log.ErrorContext(ctx, "move deleted repository to trash",
			slog.String("repository_id", id.String()), slog.String("err", err.Error()))
	}
	return nil
}

// scanRepository reads one row in the order of columns.
func scanRepository(row pgx.Row) (Repository, error) {
	var r Repository
	err := row.Scan(&r.ID, &r.OwnerID, &r.Name, &r.DefaultBranch, &r.Description, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Repository{}, ErrNotFound
	}
	return r, err
}
