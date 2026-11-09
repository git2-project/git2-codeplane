package repo

import (
	"encoding/base64"
	"encoding/json"

	"github.com/google/uuid"
)

// Page size rules from the API contract (Google AIP-158). ListAll, which
// only backups call, has larger pages (bundle.proto).
const (
	defaultPageSize    = 50
	maxPageSize        = 100
	defaultAllPageSize = 100
	maxAllPageSize     = 1000
)

// PageSize applies the page size rules: 0 means the default, values above
// the maximum are lowered to it, negative values are rejected.
func PageSize(n int32) (int, error) {
	return pageSize(n, defaultPageSize, maxPageSize)
}

func pageSize(n int32, def, maxSize int) (int, error) {
	switch {
	case n < 0:
		return 0, invalid("page_size", "must not be negative")
	case n == 0:
		return def, nil
	case int(n) > maxSize:
		return maxSize, nil
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

// EncodePageToken turns a cursor into an opaque page token. Every list RPC of
// git-svc uses this format. Clients must not parse a token; the format can
// change at any time.
//
// A value of the cursor that may not be UTF-8 must be a []byte: JSON would
// replace the invalid bytes of a string, and it would not come back the same.
func EncodePageToken(c any) string {
	b, _ := json.Marshal(c) // cannot fail: plain strings, bytes, numbers and UUIDs
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodePageToken reverses EncodePageToken. Tokens come from clients, so the
// caller must check every value it uses.
func DecodePageToken(token string, c any) error {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, c)
}

// allCursor is the position after the last repository of a ListAll page,
// which is sorted by ID.
type allCursor struct {
	All bool      `json:"a"` // always true: tells it from a List token
	ID  uuid.UUID `json:"i"`
}

// decodeAllPageToken decodes a token of ListAll.
func decodeAllPageToken(token string) (allCursor, error) {
	var c allCursor
	if err := DecodePageToken(token, &c); err != nil || !c.All {
		return allCursor{}, invalid("page_token", "is invalid")
	}
	return c, nil
}

// decodePageToken decodes a token of List and checks that it was issued for
// ownerID.
func decodePageToken(token string, ownerID uuid.UUID) (cursor, error) {
	var c cursor
	if err := DecodePageToken(token, &c); err != nil {
		return cursor{}, invalid("page_token", "is invalid")
	}
	if c.OwnerID != ownerID {
		return cursor{}, invalid("page_token", "was issued for a different owner_id")
	}
	return c, nil
}
