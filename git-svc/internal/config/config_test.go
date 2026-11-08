package config

import (
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// dsn has no password, so it is not a credential.
const dsn = "postgres://git2@localhost:5432/git2"

func TestParse(t *testing.T) {
	// An absolute path on the current OS ("/srv/git" is not absolute on Windows).
	root := t.TempDir()

	valid := func() map[string]string {
		return map[string]string{
			EnvPostgresDSN: dsn,
			EnvStorageRoot: root,
		}
	}

	tests := []struct {
		name    string
		env     func() map[string]string
		want    Config
		wantErr []string // substrings that must all appear in the error
	}{
		{
			name: "defaults",
			env:  valid,
			want: Config{
				GRPCAddr:         DefaultGRPCAddr,
				HTTPAddr:         DefaultHTTPAddr,
				PostgresDSN:      dsn,
				PostgresMaxConns: DefaultPostgresMaxConns,
				StorageRoot:      root,
				LogLevel:         slog.LevelInfo,
			},
		},
		{
			name: "all set",
			env: func() map[string]string {
				e := valid()
				e[EnvGRPCAddr] = "127.0.0.1:7000"
				e[EnvHTTPAddr] = "127.0.0.1:7080"
				e[EnvPostgresMaxConns] = "25"
				e[EnvDev] = "true"
				e[EnvLogLevel] = "debug"
				e[EnvStorageRoot] = root + string(filepath.Separator) + "sub" + string(filepath.Separator) + ".."
				e[EnvEventSinks] = "ci-svc:9090=git.v1.RepositoryPushed"
				return e
			},
			want: Config{
				GRPCAddr:         "127.0.0.1:7000",
				HTTPAddr:         "127.0.0.1:7080",
				PostgresDSN:      dsn,
				PostgresMaxConns: 25,
				StorageRoot:      root, // cleaned
				Dev:              true,
				LogLevel:         slog.LevelDebug,
				EventSinks:       []EventSink{{Addr: "ci-svc:9090", Types: []string{"git.v1.RepositoryPushed"}}},
			},
		},
		{
			name: "blank event sinks mean none",
			env: func() map[string]string {
				e := valid()
				e[EnvEventSinks] = "  "
				return e
			},
			want: Config{
				GRPCAddr:         DefaultGRPCAddr,
				HTTPAddr:         DefaultHTTPAddr,
				PostgresDSN:      dsn,
				PostgresMaxConns: DefaultPostgresMaxConns,
				StorageRoot:      root,
				LogLevel:         slog.LevelInfo,
			},
		},
		{
			name:    "missing required",
			env:     func() map[string]string { return map[string]string{} },
			wantErr: []string{EnvPostgresDSN + ": required", EnvStorageRoot + ": required"},
		},
		{
			name: "relative storage root",
			env: func() map[string]string {
				e := valid()
				e[EnvStorageRoot] = filepath.Join("data", "repos")
				return e
			},
			wantErr: []string{EnvStorageRoot + ": must be an absolute path"},
		},
		{
			name: "bad values reported together",
			env: func() map[string]string {
				e := valid()
				e[EnvGRPCAddr] = "9090"
				e[EnvHTTPAddr] = "localhost"
				e[EnvPostgresMaxConns] = "0"
				e[EnvDev] = "maybe"
				e[EnvLogLevel] = "loud"
				e[EnvEventSinks] = "ci-svc"
				return e
			},
			wantErr: []string{EnvGRPCAddr, EnvHTTPAddr, EnvPostgresMaxConns + ": must be a number from 1 to 1000", EnvDev + ": must be a boolean", EnvLogLevel, EnvEventSinks},
		},
		{
			name: "max conns too large",
			env: func() map[string]string {
				e := valid()
				e[EnvPostgresMaxConns] = "5000"
				return e
			},
			wantErr: []string{EnvPostgresMaxConns},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := tt.env()
			got, err := parse(func(k string) (string, bool) {
				v, ok := env[k]
				return v, ok
			})

			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("parse() error = nil, want error containing %q", tt.wantErr)
				}
				for _, s := range tt.wantErr {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("parse() error = %q, want it to contain %q", err, s)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("parse() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parse() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseEventSinks(t *testing.T) {
	const pushed, created = "git.v1.RepositoryPushed", "git.v1.RepositoryCreated"
	tests := []struct {
		name    string
		in      string
		want    []EventSink
		wantErr []string // substrings that must all appear in the error
	}{
		{
			name: "one subscriber",
			in:   "ci-svc:9090=git.v1.RepositoryPushed",
			want: []EventSink{{Addr: "ci-svc:9090", Types: []string{pushed}}},
		},
		{
			name: "several subscribers and types, with spaces and empty entries",
			in:   " ci-svc:9090 = git.v1.RepositoryPushed ,, dash-api:9090=git.v1.RepositoryCreated|git.v1.RepositoryPushed ",
			want: []EventSink{
				{Addr: "ci-svc:9090", Types: []string{pushed}},
				{Addr: "dash-api:9090", Types: []string{created, pushed}},
			},
		},
		{
			name:    "no types",
			in:      "ci-svc:9090",
			wantErr: []string{`"ci-svc:9090": want host:port=type|type`},
		},
		{
			name:    "no port",
			in:      "ci-svc=git.v1.RepositoryPushed",
			wantErr: []string{`"ci-svc=git.v1.RepositoryPushed"`},
		},
		{
			name:    "empty type",
			in:      "ci-svc:9090=git.v1.RepositoryPushed|",
			wantErr: []string{"ci-svc:9090: empty event type"},
		},
		{
			name:    "a type git-svc does not write",
			in:      "ci-svc:9090=git.v1.RepositoryPushd",
			wantErr: []string{`unknown event type "git.v1.RepositoryPushd"`, pushed},
		},
		{
			name:    "the same type twice",
			in:      "ci-svc:9090=git.v1.RepositoryPushed|git.v1.RepositoryPushed",
			wantErr: []string{"ci-svc:9090: git.v1.RepositoryPushed is listed twice"},
		},
		{
			name:    "the same subscriber twice",
			in:      "ci-svc:9090=git.v1.RepositoryPushed,ci-svc:9090=git.v1.RepositoryCreated",
			wantErr: []string{"ci-svc:9090 is listed twice"},
		},
		{
			name:    "every problem at once",
			in:      "a:1=git.v1.Nope,b=git.v1.RepositoryPushed",
			wantErr: []string{`unknown event type "git.v1.Nope"`, `"b=git.v1.RepositoryPushed"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseEventSinks(tt.in)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("parseEventSinks() = %+v, want an error", got)
				}
				for _, s := range tt.wantErr {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("parseEventSinks() error = %q, want it to contain %q", err, s)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEventSinks() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseEventSinks() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
