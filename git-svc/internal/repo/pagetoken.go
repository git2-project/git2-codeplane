package repo

import (
	"encoding/base64"
	"encoding/json"

	"github.com/google/uuid"
)

// Page size rules from the API contract (Google AIP-158).
const (
	defaultPageSize = 50
	maxPageSize     = 100
)

// pageSize applies the page size rules: 0 means the default, values above
// the maximum are lowered to it, negative values are rejected.
func pageSize(n int32) (int, error) {
	switch {
	case n < 0:
		return 0, invalid("page_size", "must not be negative")
	case n == 0:
		return defaultPageSize, nil
	case n > maxPageSize:
		return maxPageSize, nil
	}
	return int(n), nil
}

// cursor is the position after the last repository of a page. Listing is
// keyset-based: the next page starts after (NameKey, ID) in sort order,
// so it stays correct when repositories are created or deleted between
// requests.
type cursor struct {
	OwnerID uuid.UUID `json:"o"` // the token is only valid for this owner
	NameKey string    `json:"n"` // lower-cased name, the primary sort key
	ID      uuid.UUID `json:"i"` // tie-breaker
}

// encodePageToken turns c into an opaque token. Clients must not parse
// it; the format can change at any time.
func encodePageToken(c cursor) string {
	b, _ := json.Marshal(c) // cannot fail: plain strings and UUIDs
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodePageToken reverses encodePageToken and checks that the token was
// issued for ownerID.
func decodePageToken(token string, ownerID uuid.UUID) (cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return cursor{}, invalid("page_token", "is invalid")
	}
	var c cursor
	if err := json.Unmarshal(b, &c); err != nil {
		return cursor{}, invalid("page_token", "is invalid")
	}
	if c.OwnerID != ownerID {
		return cursor{}, invalid("page_token", "was issued for a different owner_id")
	}
	return c, nil
}
