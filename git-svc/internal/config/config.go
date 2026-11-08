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
	"slices"
	"strconv"
	"strings"

	"github.com/git2-project/git2-codeplane/git-svc/internal/outbox"
)

// Environment variable names read by Load.
const (
	EnvGRPCAddr         = "GIT2_GITSVC_GRPC_ADDR"
	EnvHTTPAddr         = "GIT2_GITSVC_HTTP_ADDR"
	EnvPostgresDSN      = "POSTGRES_DSN"
	EnvPostgresMaxConns = "POSTGRES_MAX_CONNS"
	EnvStorageRoot      = "GIT2_STORAGE_ROOT"
	EnvDev              = "GIT2_GITSVC_DEV"
	EnvLogLevel         = "GIT2_GITSVC_LOG_LEVEL"
	EnvEventSinks       = "GIT2_GITSVC_EVENT_SINKS"
)

// Defaults used when a variable is unset.
const (
	DefaultGRPCAddr         = ":9090"
	DefaultHTTPAddr         = ":9080"
	DefaultPostgresMaxConns = 10
)

// Config is the validated runtime configuration of git-svc.
type Config struct {
	// GRPCAddr is the host:port the gRPC server listens on.
	GRPCAddr string
	// HTTPAddr is the host:port of the HTTP server: Git's smart HTTP
	// protocol (clone, fetch, push) and the health endpoints.
	HTTPAddr string
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
	// EventSinks are the consumers the event relay delivers to (ADR 0014).
	// Empty means the relay does not run and events stay in the outbox.
	EventSinks []EventSink
}

// EventSink is one consumer of git-svc's events: a service that serves
// events.v1.EventSink.
type EventSink struct {
	// Addr is the consumer's gRPC address, host:port.
	Addr string
	// Types are the event types it receives, full protobuf names such as
	// "git.v1.RepositoryPushed". Each one is an event git-svc writes.
	Types []string
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
		HTTPAddr:         DefaultHTTPAddr,
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

	if v, ok := lookup(EnvHTTPAddr); ok && v != "" {
		cfg.HTTPAddr = v
	}
	if _, _, err := net.SplitHostPort(cfg.HTTPAddr); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvHTTPAddr, err))
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

	if v, ok := lookup(EnvEventSinks); ok && strings.TrimSpace(v) != "" {
		sinks, err := parseEventSinks(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvEventSinks, err))
		}
		cfg.EventSinks = sinks
	}

	return cfg, errors.Join(errs...)
}

// parseEventSinks reads a comma-separated list of consumers, each
// "host:port=type|type", for example
//
//	ci-svc:9090=git.v1.RepositoryPushed
//
// Every problem is reported, not only the first.
func parseEventSinks(v string) ([]EventSink, error) {
	known := outbox.EventTypes()
	var sinks []EventSink
	var errs []error
	seen := map[string]bool{}
	for entry := range strings.SplitSeq(v, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		addr, types, ok := strings.Cut(entry, "=")
		addr = strings.TrimSpace(addr)
		if !ok {
			errs = append(errs, fmt.Errorf("%q: want host:port=type|type", entry))
			continue
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			errs = append(errs, fmt.Errorf("%q: %w", entry, err))
			continue
		}
		if seen[addr] {
			errs = append(errs, fmt.Errorf("%s is listed twice", addr))
			continue
		}
		seen[addr] = true

		sink := EventSink{Addr: addr}
		for t := range strings.SplitSeq(types, "|") {
			t = strings.TrimSpace(t)
			switch {
			case t == "":
				errs = append(errs, fmt.Errorf("%s: empty event type", addr))
			case !slices.Contains(known, t):
				errs = append(errs, fmt.Errorf("%s: unknown event type %q (git-svc writes %s)", addr, t, strings.Join(known, ", ")))
			case slices.Contains(sink.Types, t):
				errs = append(errs, fmt.Errorf("%s: %s is listed twice", addr, t))
			default:
				sink.Types = append(sink.Types, t)
			}
		}
		sinks = append(sinks, sink)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return sinks, nil
}
