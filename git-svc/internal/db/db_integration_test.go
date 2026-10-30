package db_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/git2-project/git2-codeplane/git-svc/internal/db"
	"github.com/git2-project/git2-codeplane/git-svc/internal/testdb"
)

// The package is db_test because testdb imports db.

func TestCheckSchema(t *testing.T) {
	for _, mode := range testdb.Modes {
		t.Run(string(mode), func(t *testing.T) {
			pool := testdb.Open(t, mode)
			if err := db.CheckSchema(t.Context(), pool); err != nil {
				t.Fatalf("CheckSchema() error = %v, want nil after the migrations", err)
			}

			saved := *db.RequiredTables
			t.Cleanup(func() { *db.RequiredTables = saved })
			*db.RequiredTables = append(slices.Clone(saved), db.Schema+".missing_table")

			err := db.CheckSchema(t.Context(), pool)
			if err == nil {
				t.Fatal("CheckSchema() error = nil, want an error for a missing table")
			}
			msg := err.Error()
			if !strings.Contains(msg, "git_svc.missing_table") || !strings.Contains(msg, "git2 repo's migrations") {
				t.Errorf("error = %q, want it to name the missing table and the git2 migrations", msg)
			}
			if strings.Contains(msg, "git_svc.repositories") {
				t.Errorf("error = %q names a table that exists", msg)
			}
		})
	}
}

// TestIsUniqueViolationOnRealError checks IsUniqueViolation against the
// error PostgreSQL actually returns for the repository name index.
func TestIsUniqueViolationOnRealError(t *testing.T) {
	const insert = `
INSERT INTO git_svc.repositories (id, owner_id, name, default_branch)
VALUES ($1, $2, $3, 'main')`

	for _, mode := range testdb.Modes {
		t.Run(string(mode), func(t *testing.T) {
			pool := testdb.Open(t, mode)
			owner := uuid.New()

			// Rolled back at the end, so nothing is left in the database.
			errRollback := errors.New("roll back")
			err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(t.Context(), insert, uuid.New(), owner, "payments"); err != nil {
					t.Fatalf("first insert: %v", err)
				}
				_, err := tx.Exec(t.Context(), insert, uuid.New(), owner, "PAYMENTS")
				if !db.IsUniqueViolation(err, "repositories_owner_name_live_uq") {
					t.Errorf("IsUniqueViolation(%v, name index) = false, want true", err)
				}
				if db.IsUniqueViolation(err, "some_other_index") {
					t.Errorf("IsUniqueViolation(%v, other index) = true, want false", err)
				}
				return errRollback
			})
			if !errors.Is(err, errRollback) {
				t.Fatalf("transaction error = %v", err)
			}
		})
	}
}
