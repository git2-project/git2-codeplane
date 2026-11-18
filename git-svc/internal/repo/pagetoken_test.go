package repo

import (
	"encoding/base64"
	"testing"

	"github.com/google/uuid"
)

func TestPageSize(t *testing.T) {
	tests := []struct {
		in      int32
		want    int
		wantErr bool
	}{
		{in: 0, want: 50},
		{in: 1, want: 1},
		{in: 100, want: 100},
		{in: 101, want: 100},
		{in: 1000, want: 100},
		{in: -1, wantErr: true},
	}
	for _, tt := range tests {
		got, err := PageSize(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("PageSize(%d) = %d, %v; want %d, error %v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestPageTokenRoundTrip(t *testing.T) {
	owner := uuid.New()
	want := cursor{OwnerID: owner, NameKey: "payments", ID: uuid.New()}

	got, err := decodePageToken(EncodePageToken(want), owner)
	if err != nil {
		t.Fatalf("decodePageToken() error = %v", err)
	}
	if got != want {
		t.Errorf("decodePageToken() = %+v, want %+v", got, want)
	}
}

func TestPageTokenRejected(t *testing.T) {
	owner := uuid.New()
	tests := []struct {
		name       string
		token      string
		wantReason string
	}{
		{"not base64", "%%%", "is invalid"},
		{"not JSON", base64.RawURLEncoding.EncodeToString([]byte("payments")), "is invalid"},
		{"bad UUID", base64.RawURLEncoding.EncodeToString([]byte(`{"o":"x","n":"a","i":"y"}`)), "is invalid"},
		{"other owner", EncodePageToken(cursor{OwnerID: uuid.New(), NameKey: "a", ID: uuid.New()}), "different owner_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodePageToken(tt.token, owner)
			checkValidation(t, err, "page_token", tt.wantReason)
		})
	}
}

func TestAllPageSize(t *testing.T) {
	tests := []struct {
		in      int32
		want    int
		wantErr bool
	}{
		{in: 0, want: 100},
		{in: 1, want: 1},
		{in: 1000, want: 1000},
		{in: 1001, want: 1000},
		{in: -1, wantErr: true},
	}
	for _, tt := range tests {
		got, err := pageSize(tt.in, defaultAllPageSize, maxAllPageSize)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("pageSize(%d) for ListAll = %d, %v; want %d, error %v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestAllPageToken(t *testing.T) {
	want := allCursor{All: true, ID: uuid.New()}
	got, err := decodeAllPageToken(EncodePageToken(want))
	if err != nil || got != want {
		t.Fatalf("decodeAllPageToken() = %+v, %v; want %+v", got, err, want)
	}

	owner := uuid.New()
	tests := []struct {
		name  string
		token string
	}{
		{"not base64", "%%%"},
		{"not JSON", base64.RawURLEncoding.EncodeToString([]byte("payments"))},
		{"bad UUID", base64.RawURLEncoding.EncodeToString([]byte(`{"a":true,"i":"y"}`))},
		{"a List token", EncodePageToken(cursor{OwnerID: owner, NameKey: "a", ID: uuid.New()})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeAllPageToken(tt.token)
			checkValidation(t, err, "page_token", "is invalid")
		})
	}

	// And the other way round: List refuses a ListAll token.
	_, err = decodePageToken(EncodePageToken(want), owner)
	checkValidation(t, err, "page_token", "different owner_id")
}
