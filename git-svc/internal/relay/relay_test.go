package relay

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/git2-project/git2-codeplane/gen/go/events/v1"
	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
)

// These tests use an in-memory Store and real gRPC servers on bufconn. The
// SQL of PGStore is tested in store_integration_test.go.

var (
	typePushed  = string(proto.MessageName(&gitv1.RepositoryPushed{}))
	typeCreated = string(proto.MessageName(&gitv1.RepositoryCreated{}))
)

func TestDeliversSubscribedEventsAndMarksTheRest(t *testing.T) {
	store := newStore(pushed(1), created(2), pushed(3))
	sink := &fakeSink{}
	r, _, _ := newTestRelay(t, store, sub(t, "ci-svc", sink, typePushed))

	marked, full, err := r.pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if marked != 3 || full {
		t.Errorf("pass() = %d marked, full %v; want 3, false", marked, full)
	}
	if got := sink.eventIDs(); !slices.Equal(got, []string{store.eventID(1), store.eventID(3)}) {
		t.Errorf("delivered %v, want the two RepositoryPushed events in outbox order", got)
	}
	if got := store.publishedIDs(); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("published %v, want 1, 2, 3: an event nobody subscribed to is done at once", got)
	}
}

func TestDeliverRequestCarriesTheOutboxRow(t *testing.T) {
	ev := Event{
		ID:            7,
		EventID:       uuid.New(),
		EventType:     typePushed,
		AggregateType: "repository",
		AggregateID:   uuid.New(),
		Payload:       `{"repositoryId": "x", "refUpdates": []}`,
		OccurredAt:    time.Date(2026, 11, 8, 12, 30, 0, 123000000, time.UTC),
	}
	sink := &fakeSink{}
	r, _, _ := newTestRelay(t, newStore(ev), sub(t, "ci-svc", sink, typePushed))

	if _, _, err := r.pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := &eventsv1.DeliverRequest{
		EventId:       ev.EventID.String(),
		EventType:     typePushed,
		AggregateType: "repository",
		AggregateId:   ev.AggregateID.String(),
		OccurredAt:    timestamppb.New(ev.OccurredAt),
		Payload:       ev.Payload,
	}
	got := sink.requests()
	if len(got) != 1 || !proto.Equal(got[0], want) {
		t.Errorf("Deliver got %v, want %v", got, want)
	}
}

func TestRetryableAnswerKeepsTheEventsAndBacksOff(t *testing.T) {
	store := newStore(pushed(1), pushed(2))
	sink := &fakeSink{answer: func(n int) error {
		if n == 1 {
			return status.Error(codes.Unavailable, "ci-svc is restarting")
		}
		return nil
	}}
	r, clk, logs := newTestRelay(t, store, sub(t, "ci-svc", sink, typePushed))

	mustPass(t, r, 0)
	if n := sink.calls(); n != 1 {
		t.Errorf("%d calls, want 1: after a retryable answer the subscriber's later events wait too", n)
	}
	if !strings.Contains(logs.String(), "delivery failed") || !strings.Contains(logs.String(), "Unavailable") {
		t.Errorf("no warning with the code:\n%s", logs)
	}

	mustPass(t, r, 0)
	if n := sink.calls(); n != 1 {
		t.Errorf("%d calls, want still 1 during the backoff", n)
	}

	clk.advance(DefaultMinBackoff)
	mustPass(t, r, 2)
	if got := sink.eventIDs(); !slices.Equal(got, []string{store.eventID(1), store.eventID(1), store.eventID(2)}) {
		t.Errorf("delivered %v, want event 1 again, then event 2", got)
	}
	if !strings.Contains(logs.String(), "answers again") {
		t.Errorf("no log line when the subscriber recovered:\n%s", logs)
	}
}

// After a subscriber answers again, its next failure starts the backoff
// from the minimum instead of continuing where the last outage ended.
func TestBackoffStartsOverAfterRecovery(t *testing.T) {
	store := newStore(pushed(1), pushed(2))
	sink := &fakeSink{answer: func(n int) error {
		if n == 1 || n == 3 {
			return status.Error(codes.Unavailable, "down")
		}
		return nil
	}}
	r, clk, logs := newTestRelay(t, store, sub(t, "ci-svc", sink, typePushed))

	mustPass(t, r, 0) // call 1 fails
	clk.advance(DefaultMinBackoff)
	mustPass(t, r, 1) // call 2 delivers event 1, call 3 fails on event 2
	clk.advance(DefaultMinBackoff)
	mustPass(t, r, 1) // the pause is the minimum again, so call 4 happens
	if n := sink.calls(); n != 4 {
		t.Errorf("%d calls, want 4", n)
	}
	if n := strings.Count(logs.String(), "answers again"); n != 2 {
		t.Errorf("logged recovery %d times, want 2 (once per outage)", n)
	}
}

// A server that does not serve EventSink answers UNIMPLEMENTED. That must
// not drop the events: a consumer that is not ready yet gets them later.
func TestServerWithoutEventSinkIsRetried(t *testing.T) {
	store := newStore(pushed(1))
	r, _, logs := newTestRelay(t, store, Subscriber{Name: "old-ci-svc", Types: []string{typePushed}, Client: dial(t, nil)})

	mustPass(t, r, 0)
	if got := store.publishedIDs(); len(got) != 0 {
		t.Errorf("published %v, want nothing", got)
	}
	if !strings.Contains(logs.String(), "Unimplemented") {
		t.Errorf("want a warning with code Unimplemented:\n%s", logs)
	}
}

func TestRefusedEventIsMarkedAndLogged(t *testing.T) {
	for _, code := range []codes.Code{codes.InvalidArgument, codes.FailedPrecondition} {
		t.Run(code.String(), func(t *testing.T) {
			store := newStore(pushed(1), pushed(2))
			sink := &fakeSink{answer: func(n int) error {
				if n == 1 {
					return status.Error(code, "cannot handle this event")
				}
				return nil
			}}
			r, _, logs := newTestRelay(t, store, sub(t, "ci-svc", sink, typePushed))

			mustPass(t, r, 2)
			if n := sink.calls(); n != 2 {
				t.Errorf("%d calls, want 2: a refusal does not hold back the next event", n)
			}
			if !strings.Contains(logs.String(), `"level":"ERROR"`) || !strings.Contains(logs.String(), store.eventID(1)) {
				t.Errorf("want an ERROR line naming the refused event:\n%s", logs)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		err  error
		want outcome
	}{
		{nil, delivered},
		{status.Error(codes.InvalidArgument, ""), refused},
		{status.Error(codes.FailedPrecondition, ""), refused},
		{status.Error(codes.Unimplemented, ""), retry},
		{errors.New("not a status"), retry}, // what a handler's plain error becomes: UNKNOWN
		{status.Error(codes.Unknown, ""), retry},
		{status.Error(codes.Unavailable, ""), retry},
		{status.Error(codes.DeadlineExceeded, ""), retry},
		{status.Error(codes.ResourceExhausted, ""), retry},
		{status.Error(codes.Aborted, ""), retry},
		{status.Error(codes.Internal, ""), retry},
		{status.Error(codes.NotFound, ""), retry},
		{status.Error(codes.PermissionDenied, ""), retry},
		{status.Error(codes.Unauthenticated, ""), retry},
		{status.Error(codes.Canceled, ""), retry},
	}
	for _, tt := range tests {
		if got := classify(tt.err); got != tt.want {
			t.Errorf("classify(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

// FR-04: if git-svc stops after a subscriber stored an event but before the
// event was marked, the event is delivered again after the restart, with
// the same event_id, so the subscriber can drop it.
func TestEventIsDeliveredAgainWhenMarkingWasLost(t *testing.T) {
	store := newStore(pushed(1))
	store.markErr = errors.New("connection lost")
	sink := &fakeSink{}
	r, _, _ := newTestRelay(t, store, sub(t, "ci-svc", sink, typePushed))

	if _, _, err := r.pass(t.Context()); err == nil {
		t.Fatal("pass() error = nil, want the marking error")
	}

	store.markErr = nil
	restarted, _, _ := newTestRelay(t, store, sub(t, "ci-svc", sink, typePushed))
	mustPass(t, restarted, 1)
	if got := sink.eventIDs(); !slices.Equal(got, []string{store.eventID(1), store.eventID(1)}) {
		t.Errorf("delivered %v, want the same event twice", got)
	}
}

// git_svc.outbox has one published_at per event. While one subscriber
// fails, the event stays pending, and the next attempt goes to every
// subscriber again; the ones that already have it drop it by event_id.
func TestEventWaitsForEverySubscriber(t *testing.T) {
	store := newStore(pushed(1))
	up := &fakeSink{}
	down := &fakeSink{answer: func(n int) error {
		if n == 1 {
			return status.Error(codes.Unavailable, "down")
		}
		return nil
	}}
	r, clk, _ := newTestRelay(t, store, sub(t, "ci-svc", up, typePushed), sub(t, "dash-api", down, typePushed))

	mustPass(t, r, 0)
	clk.advance(DefaultMinBackoff)
	mustPass(t, r, 1)
	if up.calls() != 2 || down.calls() != 2 {
		t.Errorf("calls: ci-svc %d, dash-api %d; want 2 each", up.calls(), down.calls())
	}
	if a, b := up.eventIDs(), down.eventIDs(); a[0] != a[1] || !slices.Equal(a, b) {
		t.Errorf("event IDs: ci-svc %v, dash-api %v; want the same event to both, twice", a, b)
	}
}

// A relay that is stopping still marks the events it delivered, so they
// are not sent again after the restart.
func TestStoppingStillMarksWhatWasDelivered(t *testing.T) {
	store := newStore(pushed(1), pushed(2))
	ctx, cancel := context.WithCancel(t.Context())
	sink := &fakeSink{answer: func(n int) error {
		if n == 2 {
			cancel() // SIGTERM while the second event is on its way
			return status.Error(codes.Unavailable, "")
		}
		return nil
	}}
	r, _, logs := newTestRelay(t, store, sub(t, "ci-svc", sink, typePushed))

	marked, _, err := r.pass(ctx)
	if err != nil || marked != 1 {
		t.Errorf("pass() = %d marked, %v; want 1, nil", marked, err)
	}
	if got := store.publishedIDs(); !slices.Equal(got, []int64{1}) {
		t.Errorf("published %v, want only the delivered event 1", got)
	}
	if strings.Contains(logs.String(), "delivery failed") {
		t.Errorf("a call cut short by the shutdown was logged as a failure:\n%s", logs)
	}
}

func TestBackoff(t *testing.T) {
	b := backoff{min: time.Second, max: time.Minute}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	for i, w := range want {
		if got := b.after(i + 1); got != w {
			t.Errorf("after(%d) = %s, want %s", i+1, got, w)
		}
	}
	if got := b.after(1 << 20); got != time.Minute {
		t.Errorf("after(many) = %s, want the maximum", got)
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	client := eventsv1.NewEventSinkClient(nil)
	ok := Subscriber{Name: "ci-svc", Types: []string{typePushed}, Client: client}
	tests := []struct {
		name string
		opts Options
	}{
		{"no store", Options{Subscribers: []Subscriber{ok}}},
		{"no subscribers", Options{Store: newStore()}},
		{"no name", Options{Store: newStore(), Subscribers: []Subscriber{{Types: ok.Types, Client: client}}}},
		{"no client", Options{Store: newStore(), Subscribers: []Subscriber{{Name: "x", Types: ok.Types}}}},
		{"no types", Options{Store: newStore(), Subscribers: []Subscriber{{Name: "x", Client: client}}}},
		{"twice", Options{Store: newStore(), Subscribers: []Subscriber{ok, ok}}},
		{"max below min", Options{Store: newStore(), Subscribers: []Subscriber{ok}, MinBackoff: time.Minute, MaxBackoff: time.Second}},
	}
	for _, tt := range tests {
		if _, err := New(tt.opts); err == nil {
			t.Errorf("%s: New() error = nil", tt.name)
		}
	}
}

func TestRunDeliversUntilCanceled(t *testing.T) {
	store := newStore()
	store.pendingErrs = 2 // the database is down at first
	sink := &fakeSink{}
	r, err := New(Options{
		Store: store, Subscribers: []Subscriber{sub(t, "ci-svc", sink, typePushed)}, Log: discard(),
		PollInterval: 5 * time.Millisecond, MinBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()

	store.add(pushed(1))
	waitFor(t, "the event to be published", func() bool { return len(store.publishedIDs()) == 1 })
	store.add(pushed(2))
	waitFor(t, "the second event to be published", func() bool { return len(store.publishedIDs()) == 2 })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// A full batch of events that all wait for a backed-off subscriber must not
// make Run query the outbox in a tight loop.
func TestRunWaitsWhenNothingCanBeDelivered(t *testing.T) {
	store := newStore(pushed(1), pushed(2))
	sink := &fakeSink{answer: func(int) error { return status.Error(codes.Unavailable, "down") }}
	r, err := New(Options{
		Store: store, Subscribers: []Subscriber{sub(t, "ci-svc", sink, typePushed)}, Log: discard(),
		BatchSize: 2, PollInterval: 20 * time.Millisecond, MinBackoff: time.Hour, MaxBackoff: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	r.Run(ctx)

	if n := store.pendingCalls(); n > 20 {
		t.Errorf("%d outbox queries in 200 ms with a 20 ms poll interval, want about 10", n)
	}
	if n := sink.calls(); n != 1 {
		t.Errorf("%d Deliver calls, want 1 during the backoff", n)
	}
}

// --- helpers ---

func newTestRelay(t *testing.T, store Store, subs ...Subscriber) (*Relay, *clock, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	r, err := New(Options{Store: store, Subscribers: subs, Log: slog.New(slog.NewJSONHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	r.now = clk.now
	return r, clk, logs
}

func mustPass(t *testing.T, r *Relay, wantMarked int) {
	t.Helper()
	marked, _, err := r.pass(t.Context())
	if err != nil {
		t.Fatalf("pass() error = %v", err)
	}
	if marked != wantMarked {
		t.Fatalf("pass() marked %d, want %d", marked, wantMarked)
	}
}

// sub serves sink on bufconn, the first time it is used, and returns it as
// a subscriber of types.
func sub(t *testing.T, name string, sink *fakeSink, types ...string) Subscriber {
	t.Helper()
	if sink.client == nil {
		sink.client = dial(t, sink)
	}
	return Subscriber{Name: name, Types: types, Client: sink.client}
}

func pushed(id int64) Event  { return event(id, typePushed) }
func created(id int64) Event { return event(id, typeCreated) }

func event(id int64, eventType string) Event {
	return Event{
		ID: id, EventID: uuid.New(), EventType: eventType, AggregateType: "repository",
		AggregateID: uuid.New(), Payload: "{}", OccurredAt: time.Now().UTC(),
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// memStore is an in-memory Store.
type memStore struct {
	mu          sync.Mutex
	events      []Event
	published   map[int64]bool
	markErr     error
	pendingErrs int // the next Pending calls that fail
	queries     int
}

func newStore(events ...Event) *memStore {
	return &memStore{events: events, published: map[int64]bool{}}
}

func (s *memStore) add(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *memStore) Pending(_ context.Context, limit int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries++
	if s.pendingErrs > 0 {
		s.pendingErrs--
		return nil, errors.New("database down")
	}
	var out []Event
	for _, e := range s.events {
		if !s.published[e.ID] && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

// MarkPublished fails on a canceled context, as a database driver does.
func (s *memStore) MarkPublished(ctx context.Context, ids []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.markErr != nil {
		return s.markErr
	}
	for _, id := range ids {
		s.published[id] = true
	}
	return nil
}

func (s *memStore) publishedIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	for id := range s.published {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (s *memStore) eventID(id int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.ID == id {
			return e.EventID.String()
		}
	}
	return ""
}

func (s *memStore) pendingCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queries
}

// fakeSink is a consumer that records what it receives. answer, if set,
// decides the reply to call n (from 1); nil means OK.
type fakeSink struct {
	eventsv1.UnimplementedEventSinkServer

	answer func(n int) error
	client eventsv1.EventSinkClient

	mu  sync.Mutex
	got []*eventsv1.DeliverRequest
}

func (f *fakeSink) Deliver(_ context.Context, req *eventsv1.DeliverRequest) (*eventsv1.DeliverResponse, error) {
	f.mu.Lock()
	f.got = append(f.got, req)
	n := len(f.got)
	f.mu.Unlock()
	if f.answer != nil {
		if err := f.answer(n); err != nil {
			return nil, err
		}
	}
	return &eventsv1.DeliverResponse{}, nil
}

func (f *fakeSink) requests() []*eventsv1.DeliverRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

func (f *fakeSink) calls() int { return len(f.requests()) }

func (f *fakeSink) eventIDs() []string {
	var ids []string
	for _, r := range f.requests() {
		ids = append(ids, r.GetEventId())
	}
	return ids
}

// dial serves srv on bufconn and returns a client for it. A nil srv gives
// a gRPC server without EventSink, as an old consumer would be.
func dial(t *testing.T, srv eventsv1.EventSinkServer) eventsv1.EventSinkClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer()
	if srv != nil {
		eventsv1.RegisterEventSinkServer(s, srv)
	}
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return eventsv1.NewEventSinkClient(conn)
}

// clock is a time source the test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// lockedBuffer collects log output from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
