// Package config loads git-svc configuration from environment variables.
//
// The database variables (POSTGRES_DSN, POSTGRES_MAX_CONNS) have the same
// names and rules as in the git2 repo, so one .env works for every service.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

// Environment variable names read by Load.
const (
	EnvGRPCAddr         = "GIT2_GITSVC_GRPC_ADDR"
	EnvPostgresDSN      = "POSTGRES_DSN"
	EnvPostgresMaxConns = "POSTGRES_MAX_CONNS"
	EnvStorageRoot      = "GIT2_STORAGE_ROOT"
	EnvDev              = "GIT2_GITSVC_DEV"
	EnvLogLevel         = "GIT2_GITSVC_LOG_LEVEL"
)

// Defaults used when a variable is unset.
const (
	DefaultGRPCAddr         = ":9090"
	DefaultPostgresMaxConns = 10
)

// Config is the validated runtime configuration of git-svc.
type Config struct {
	// GRPCAddr is the host:port the gRPC server listens on.
	GRPCAddr string
	// PostgresDSN is the PostgreSQL connection string. It may contain a
	// password, so it must not be logged.
	PostgresDSN string
	// PostgresMaxConns is the size of the connection pool, 1 to 1000.
	PostgresMaxConns int32
	// StorageRoot is the absolute, cleaned directory that holds all
	// repositories and git-svc's working directories.
	StorageRoot string
	// Dev enables developer-only features such as gRPC server reflection.
	Dev bool
	// LogLevel is the minimum level written to the log.
	LogLevel slog.Level
}

// Load reads and validates the configuration from the process environment.
func Load() (Config, error) {
	return parse(os.LookupEnv)
}

// parse builds a Config from lookup. It reports every invalid or missing
// variable at once, so an operator can fix them in one go.
func parse(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		GRPCAddr:         DefaultGRPCAddr,
		PostgresMaxConns: DefaultPostgresMaxConns,
		LogLevel:         slog.LevelInfo,
	}
	var errs []error

	if v, ok := lookup(EnvGRPCAddr); ok && v != "" {
		cfg.GRPCAddr = v
	}
	if _, _, err := net.SplitHostPort(cfg.GRPCAddr); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvGRPCAddr, err))
	}

	if v, ok := lookup(EnvPostgresDSN); ok && v != "" {
		cfg.PostgresDSN = v
	} else {
		errs = append(errs, fmt.Errorf("%s: required", EnvPostgresDSN))
	}

	if v, ok := lookup(EnvPostgresMaxConns); ok && v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 || n > 1000 {
			errs = append(errs, fmt.Errorf("%s: must be a number from 1 to 1000, got %q", EnvPostgresMaxConns, v))
		} else {
			cfg.PostgresMaxConns = int32(n)
		}
	}

	switch v, ok := lookup(EnvStorageRoot); {
	case !ok || v == "":
		errs = append(errs, fmt.Errorf("%s: required", EnvStorageRoot))
	case !filepath.IsAbs(v):
		errs = append(errs, fmt.Errorf("%s: must be an absolute path, got %q", EnvStorageRoot, v))
	default:
		cfg.StorageRoot = filepath.Clean(v)
	}

	if v, ok := lookup(EnvDev); ok && v != "" {
		dev, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: must be a boolean, got %q", EnvDev, v))
		}
		cfg.Dev = dev
	}

	if v, ok := lookup(EnvLogLevel); ok && v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvLogLevel, err))
		}
	}

	return cfg, errors.Join(errs...)
}
