// Package config loads git-svc configuration from environment variables.
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
	EnvGRPCAddr    = "GIT2_GITSVC_GRPC_ADDR"
	EnvDatabaseURL = "GIT2_DATABASE_URL"
	EnvStorageRoot = "GIT2_STORAGE_ROOT"
	EnvDev         = "GIT2_GITSVC_DEV"
	EnvLogLevel    = "GIT2_GITSVC_LOG_LEVEL"
)

// DefaultGRPCAddr is the listen address used when EnvGRPCAddr is unset.
const DefaultGRPCAddr = ":9090"

// Config is the validated runtime configuration of git-svc.
type Config struct {
	// GRPCAddr is the host:port the gRPC server listens on.
	GRPCAddr string
	// DatabaseURL is the PostgreSQL connection string. It may contain a
	// password, so it must not be logged.
	DatabaseURL string
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
	cfg := Config{GRPCAddr: DefaultGRPCAddr, LogLevel: slog.LevelInfo}
	var errs []error

	if v, ok := lookup(EnvGRPCAddr); ok && v != "" {
		cfg.GRPCAddr = v
	}
	if _, _, err := net.SplitHostPort(cfg.GRPCAddr); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvGRPCAddr, err))
	}

	if v, ok := lookup(EnvDatabaseURL); ok && v != "" {
		cfg.DatabaseURL = v
	} else {
		errs = append(errs, fmt.Errorf("%s: required", EnvDatabaseURL))
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
