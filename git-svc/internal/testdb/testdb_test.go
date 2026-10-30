package testdb

import (
	"path/filepath"
	"testing"
)

func TestWithParam(t *testing.T) {
	tests := []struct {
		dsn, want string
	}{
		{"postgres://u@h:5432/db", "postgres://u@h:5432/db?k=v"},
		{"postgres://u@h:5432/db?sslmode=disable", "postgres://u@h:5432/db?sslmode=disable&k=v"},
		{"host=h user=u dbname=db", "host=h user=u dbname=db k=v"},
	}
	for _, tt := range tests {
		if got := withParam(tt.dsn, "k", "v"); got != tt.want {
			t.Errorf("withParam(%q) = %q, want %q", tt.dsn, got, tt.want)
		}
	}
}

func TestMigrationsDir(t *testing.T) {
	dir, err := migrationsDir()
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found in %s (error: %v)", dir, err)
	}
}
