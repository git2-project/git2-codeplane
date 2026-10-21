package config

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// dbURL has no password, so it is not a credential.
const dbURL = "postgres://git2@localhost:5432/git2"

func TestParse(t *testing.T) {
	// An absolute path on the current OS ("/srv/git" is not absolute on Windows).
	root := t.TempDir()

	valid := func() map[string]string {
		return map[string]string{
			EnvDatabaseURL: dbURL,
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
				GRPCAddr:    DefaultGRPCAddr,
				DatabaseURL: dbURL,
				StorageRoot: root,
				LogLevel:    slog.LevelInfo,
			},
		},
		{
			name: "all set",
			env: func() map[string]string {
				e := valid()
				e[EnvGRPCAddr] = "127.0.0.1:7000"
				e[EnvDev] = "true"
				e[EnvLogLevel] = "debug"
				e[EnvStorageRoot] = root + string(filepath.Separator) + "sub" + string(filepath.Separator) + ".."
				return e
			},
			want: Config{
				GRPCAddr:    "127.0.0.1:7000",
				DatabaseURL: dbURL,
				StorageRoot: root, // cleaned
				Dev:         true,
				LogLevel:    slog.LevelDebug,
			},
		},
		{
			name:    "missing required",
			env:     func() map[string]string { return map[string]string{} },
			wantErr: []string{EnvDatabaseURL + ": required", EnvStorageRoot + ": required"},
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
				e[EnvDev] = "maybe"
				e[EnvLogLevel] = "loud"
				return e
			},
			wantErr: []string{EnvGRPCAddr, EnvDev + ": must be a boolean", EnvLogLevel},
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
			if got != tt.want {
				t.Errorf("parse() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
