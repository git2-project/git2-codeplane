package browse

import (
	"encoding/base64"
	"encoding/json"
)

// encodeToken turns a cursor into an opaque page token. Clients must not
// parse it; the format can change at any time.
func encodeToken(cursor any) string {
	b, _ := json.Marshal(cursor) // cannot fail: plain strings, numbers and UUIDs
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeToken reverses encodeToken. Tokens come from clients, so the caller
// must check every value it uses.
func decodeToken(token string, cursor any) error {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, cursor)
}
