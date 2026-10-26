package repo

import (
	"errors"

	"github.com/google/uuid"
)

// Errors returned by Service, matched with errors.Is. The gRPC layer maps
// them to status codes.
var (
	// ErrInvalidArgument means the input breaks a rule of the API. Errors
	// that match it are *ValidationError values that name the field.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrNotFound means no live repository matches.
	ErrNotFound = errors.New("repository not found")
	// ErrAlreadyExists means the owner already has a live repository with
	// the same name, compared case-insensitively.
	ErrAlreadyExists = errors.New("repository already exists")
)

// ValidationError says which input field is invalid and why. Its message
// is safe to show to API clients: it never repeats the rejected value.
type ValidationError struct {
	Field  string // field name as in the API, e.g. "default_branch"
	Reason string // e.g. `must not end with ".git"`
}

func (e *ValidationError) Error() string {
	return e.Field + ": " + e.Reason
}

// Is makes errors.Is(err, ErrInvalidArgument) true for a *ValidationError.
func (e *ValidationError) Is(target error) bool {
	return target == ErrInvalidArgument
}

func invalid(field, reason string) error {
	return &ValidationError{Field: field, Reason: reason}
}

// ParseID parses an ID in canonical UUID form
// ("3f2b8c1e-9d4a-4c2b-8e1f-0a1b2c3d4e5f", either letter case). field names
// the input in the error.
func ParseID(field, s string) (uuid.UUID, error) {
	if s == "" {
		return uuid.Nil, invalid(field, "is required")
	}
	// uuid.Parse also accepts forms such as "urn:uuid:..." and "{...}".
	// The API promises the canonical form only, which is 36 characters.
	id, err := uuid.Parse(s)
	if err != nil || len(s) != 36 {
		return uuid.Nil, invalid(field, "must be a UUID")
	}
	return id, nil
}
