package repo

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr string // reason substring; "" means valid
	}{
		{"payments", ""},
		{"a", ""},
		{"My_Repo-2.0", ""},
		{"x.gitignore", ""},
		{"a.git.b", ""},
		{"api_v2", ""},
		{strings.Repeat("a", 100), ""},
		{"", "is required"},
		{strings.Repeat("a", 101), "at most 100"},
		{"pay ments", "may contain only"},
		{"pay/ments", "may contain only"},
		{"pay\\ments", "may contain only"},
		{"café", "may contain only"},
		{"repo\n", "may contain only"},
		{".hidden", "must not start"},
		{"-flag", "must not start"},
		{".", "must not start"},
		{"..", "must not start"},
		{"repo.git", `must not end with ".git"`},
		{"repo.GIT", `must not end with ".git"`},
		{"repo.Git", `must not end with ".git"`},
		{".git", "must not start"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkValidation(t, ValidateName(tt.name), "name", tt.wantErr)
		})
	}
}

func TestValidateDescription(t *testing.T) {
	tests := []struct {
		name        string
		description string
		wantErr     string
	}{
		{"empty", "", ""},
		{"text", "Payment service.\nOwned by team A.", ""},
		{"2000 runes", strings.Repeat("ü", 2000), ""}, // 4000 bytes: the limit counts characters
		{"2001 runes", strings.Repeat("a", 2001), "at most 2000"},
		{"invalid UTF-8", "bad \xff byte", "valid UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkValidation(t, ValidateDescription(tt.description), "description", tt.wantErr)
		})
	}
}

func TestParseID(t *testing.T) {
	const canonical = "3f2b8c1e-9d4a-4c2b-8e1f-0a1b2c3d4e5f"
	tests := []struct {
		in      string
		wantErr string
	}{
		{canonical, ""},
		{strings.ToUpper(canonical), ""},
		{"", "is required"},
		{"3f2b8c1e", "must be a UUID"},
		{"3f2b8c1e9d4a4c2b8e1f0a1b2c3d4e5f", "must be a UUID"},     // no dashes
		{"urn:uuid:" + canonical, "must be a UUID"},                // accepted by uuid.Parse
		{"{" + canonical + "}", "must be a UUID"},                  // accepted by uuid.Parse
		{"3f2b8c1e-9d4a-4c2b-8e1f-0a1b2c3d4e5g", "must be a UUID"}, // not hex
		{"../../etc/passwd/0000-0000-000000000", "must be a UUID"}, // 36 characters, like a UUID
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			id, err := ParseID("id", tt.in)
			checkValidation(t, err, "id", tt.wantErr)
			if tt.wantErr == "" && id.String() != canonical {
				t.Errorf("ParseID(%q) = %s, want %s", tt.in, id, canonical)
			}
		})
	}
}

// checkValidation asserts that err is nil when wantReason is empty, and
// otherwise a *ValidationError for field whose reason contains wantReason.
func checkValidation(t *testing.T, err error, field, wantReason string) {
	t.Helper()
	if wantReason == "" {
		if err != nil {
			t.Errorf("error = %v, want nil", err)
		}
		return
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("error = %v, want *ValidationError", err)
	}
	if !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("errors.Is(err, ErrInvalidArgument) = false for %v", err)
	}
	if verr.Field != field || !strings.Contains(verr.Reason, wantReason) {
		t.Errorf("error = %q, want field %q and reason containing %q", err, field, wantReason)
	}
}
