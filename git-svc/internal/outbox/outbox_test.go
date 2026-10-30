package outbox

import (
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
)

// TestEncode pins the event types and payload format that consumers rely
// on (events.proto, ADR 0007).
func TestEncode(t *testing.T) {
	at := timestamppb.New(time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC))
	tests := []struct {
		event    proto.Message
		wantType string
	}{
		{
			event:    &gitv1.RepositoryCreated{Id: "3f2b8c1e-0000-4000-8000-000000000001", OwnerId: "9a1d2e3f-0000-4000-8000-000000000002", Name: "payments", OccurredAt: at},
			wantType: "git.v1.RepositoryCreated",
		},
		{
			event:    &gitv1.RepositoryDeleted{Id: "3f2b8c1e-0000-4000-8000-000000000001", OwnerId: "9a1d2e3f-0000-4000-8000-000000000002", Name: "payments", OccurredAt: at},
			wantType: "git.v1.RepositoryDeleted",
		},
	}
	for _, tt := range tests {
		t.Run(tt.wantType, func(t *testing.T) {
			gotType, payload, err := encode(tt.event)
			if err != nil {
				t.Fatalf("encode() error = %v", err)
			}
			if gotType != tt.wantType {
				t.Errorf("event type = %q, want %q", gotType, tt.wantType)
			}

			// A consumer decodes the payload with protojson into the type
			// named by the event type.
			decoded := tt.event.ProtoReflect().New().Interface()
			if err := protojson.Unmarshal([]byte(payload), decoded); err != nil {
				t.Fatalf("protojson.Unmarshal() error = %v", err)
			}
			if !proto.Equal(decoded, tt.event) {
				t.Errorf("decoded payload = %v, want %v", decoded, tt.event)
			}
		})
	}
}
