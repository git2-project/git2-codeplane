// Package db connects git-svc to PostgreSQL.
//
// git-svc owns the PostgreSQL schema git_svc and nothing outside it. Every
// query names its tables with the schema (git_svc.repositories) instead of
// relying on search_path.
//
// git-svc never changes the schema itself. Migrations live in the git2
// repo and a GitHub Action applies them after merge (ADR 0006); this repo
// keeps a copy in migrations/ for its tests.
package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Schema is the PostgreSQL schema owned by git-svc.
const Schema = "git_svc"

// requiredTables are the tables git-svc reads and writes.
var requiredTables = []string{Schema + ".repositories", Schema + ".outbox"}

// pingTimeout bounds the first connection attempt at startup.
const pingTimeout = 5 * time.Second

// errBadDSN is returned for a malformed DSN. pgx's own parse error quotes
// the DSN and hides the password only on a best-effort basis, so it is not
// passed on: the message ends up in logs.
var errBadDSN = errors.New("parse POSTGRES_DSN: not a valid PostgreSQL connection string")

// Open creates a connection pool of at most maxConns connections and checks
// that the database is reachable.
//
// Pool settings and the pooler handling are the same as in the git2 repo's
// internal/database, so every git2 service behaves alike.
func Open(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errBadDSN
	}
	cfg.MaxConns = maxConns
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	// A transaction-pooling PgBouncer (Supabase's port 6543) hands each
	// transaction to any server connection, so server-side prepared
	// statements would collide on their names.
	if isTransactionPooler(dsn) {
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
		cfg.ConnConfig.StatementCacheCapacity = 0
		cfg.ConnConfig.DescriptionCacheCapacity = 0
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}

// isTransactionPooler reports whether the DSN targets Supabase's
// transaction pooler (port 6543) or any host that advertises pgbouncer. It
// is the same rule as isTransactionPooler in the git2 repo.
func isTransactionPooler(dsn string) bool {
	lower := strings.ToLower(dsn)
	return strings.Contains(dsn, ":6543/") ||
		strings.Contains(lower, "pgbouncer") ||
		strings.Contains(lower, "pooler.supabase")
}

// CheckSchema returns an error if a table git-svc needs is missing, which
// means the git2 repo's migrations have not been applied to this database.
func CheckSchema(ctx context.Context, pool *pgxpool.Pool) error {
	var missing []string
	for _, table := range requiredTables {
		var found *string // to_regclass returns NULL for a missing table
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1::text)::text", table).Scan(&found); err != nil {
			return fmt.Errorf("check table %s: %w", table, err)
		}
		if found == nil {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("database schema is not ready, missing %s: apply the git2 repo's migrations (migrations/*_git_*.sql)",
			strings.Join(missing, ", "))
	}
	return nil
}

// uniqueViolation is PostgreSQL's SQLSTATE for a unique constraint violation.
const uniqueViolation = "23505"

// IsUniqueViolation reports whether err is a violation of the unique index
// or constraint named constraint.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}
