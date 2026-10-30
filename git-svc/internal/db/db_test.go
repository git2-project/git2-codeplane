package db

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsUniqueViolation(t *testing.T) {
	const index = "repositories_owner_name_live_uq"
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"match", &pgconn.PgError{Code: "23505", ConstraintName: index}, true},
		{"wrapped", fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505", ConstraintName: index}), true},
		{"other constraint", &pgconn.PgError{Code: "23505", ConstraintName: "repositories_pkey"}, false},
		{"other code", &pgconn.PgError{Code: "23503", ConstraintName: index}, false},
		{"not a PgError", errors.New("23505"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsUniqueViolation(tt.err, index); got != tt.want {
				t.Errorf("IsUniqueViolation() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A false negative is the expensive case: prepared statements left on behind
// a transaction pooler collide on statement names across shared connections.
// The cases are the same as in the git2 repo's internal/database.
func TestIsTransactionPooler(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want bool
	}{
		{"supabase pooler port", "postgres://u.ref:pw@aws-0-eu-central-1.pooler.supabase.com:6543/postgres", true},
		{"pooler.supabase host on 5432", "postgres://u:p@aws-0.pooler.supabase.com:5432/postgres", true},
		{"pgbouncer in host", "postgres://u:p@PGBouncer.internal:5432/app", true},
		{"direct supabase connection", "postgres://postgres:p@db.ref.supabase.co:5432/postgres", false},
		{"localhost", "postgres://postgres:postgres@localhost:5432/git2", false},
		{"6543 in dbname is not the port", "postgres://u:p@h:5432/db6543", false},
		{"port 65431 is not 6543", "postgres://u:p@h:65431/db", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransactionPooler(tt.dsn); got != tt.want {
				t.Errorf("isTransactionPooler(%q) = %v, want %v", tt.dsn, got, tt.want)
			}
		})
	}
}

func TestOpenBadDSNDoesNotLeakPassword(t *testing.T) {
	for _, dsn := range []string{
		"postgres://git2:s3cret@localhost:5432/git2?sslmode=bogus",
		"postgres://git2:s3cret@localhost:notaport/git2",
		"host=localhost password=s3cret port=notaport",
	} {
		_, err := Open(t.Context(), dsn, 2)
		if err == nil {
			t.Fatalf("Open(%q) error = nil, want a parse error", dsn)
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("Open() error leaks the password: %v", err)
		}
	}
}
