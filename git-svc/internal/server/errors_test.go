package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

func TestToStatus(t *testing.T) {
	// An internal error as storage could return it: it names a path.
	const path = `C:\srv\git2\repositories\3f\3f2b8c1e.git`
	diskErr := fmt.Errorf("create repository on disk: git init: exit status 128: fatal: cannot mkdir %s", path)

	_, invalidOwner := repo.ParseID("owner_id", "not-a-uuid")

	tests := []struct {
		name     string
		err      error
		wantCode codes.Code
		wantMsg  string
		wantLog  string // substring the server log must contain; "" means nothing is logged
	}{
		{"validation", invalidOwner, codes.InvalidArgument, "owner_id: must be a UUID", ""},
		{"validation wrapped", fmt.Errorf("create: %w", repo.ValidateName("x.git")), codes.InvalidArgument, `name: must not end with ".git"`, ""},
		{"not found wrapped", fmt.Errorf("get repository: %w", repo.ErrNotFound), codes.NotFound, "repository not found", ""},
		{"already exists wrapped", fmt.Errorf("%w: %q", repo.ErrAlreadyExists, "payments"), codes.AlreadyExists, "the owner already has a repository with this name", ""},
		{"canceled", fmt.Errorf("query: %w", context.Canceled), codes.Canceled, "request canceled", ""},
		{"deadline", fmt.Errorf("git: %w", context.DeadlineExceeded), codes.DeadlineExceeded, "deadline exceeded", ""},
		{"internal with path", diskErr, codes.Internal, "internal error", "exit status 128"},
		{"internal plain", errors.New("boom"), codes.Internal, "internal error", "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&logs, nil))

			st, _ := status.FromError(toStatus(t.Context(), log, tt.err))

			if st.Code() != tt.wantCode || st.Message() != tt.wantMsg {
				t.Errorf("toStatus() = %v %q, want %v %q", st.Code(), st.Message(), tt.wantCode, tt.wantMsg)
			}
			if strings.Contains(st.Message(), "repositories") {
				t.Errorf("client message leaks internal details: %q", st.Message())
			}
			switch {
			case tt.wantLog == "" && logs.Len() > 0:
				t.Errorf("client error was logged: %s", logs.String())
			case tt.wantLog != "" && !strings.Contains(logs.String(), tt.wantLog):
				t.Errorf("log = %q, want it to contain %q", logs.String(), tt.wantLog)
			}
		})
	}
}
