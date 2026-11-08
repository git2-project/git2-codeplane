// Package relay delivers the events in git_svc.outbox to the services that
// subscribed to them (ADR 0014, git2-runner ADR 0010).
//
// The relay polls the outbox. For each pending event it calls
// events.v1.EventSink.Deliver on every subscriber of the event's type, and
// marks the event published once each of them has answered OK or refused it
// for good. An event that nobody subscribed to is marked at once. Delivery
// is at least once: after a crash, or while one subscriber keeps failing,
// a subscriber can receive an event again and drops it by its event ID.
package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/git2-project/git2-codeplane/gen/go/events/v1"
)

// Defaults for the zero values in Options (ADR 0014).
const (
	DefaultPollInterval = time.Second
	DefaultBatchSize    = 100
	// DefaultCallTimeout is the deadline of one Deliver call, as agreed
	// with ci-svc (git2-runner ADR 0010).
	DefaultCallTimeout = 10 * time.Second
	DefaultMinBackoff  = time.Second
	DefaultMaxBackoff  = time.Minute
)

// markTimeout bounds marking the delivered events of a pass, also when the
// relay is stopping.
const markTimeout = 5 * time.Second

// Event is one row of git_svc.outbox.
type Event struct {
	// ID is the row's position in the outbox. Rows are read in ID order.
	ID int64
	// EventID is the event's UUID, which consumers use to drop repeats.
	EventID uuid.UUID
	// EventType is the full protobuf name of the payload message.
	EventType string
	// AggregateType is the kind of object, for example "repository".
	AggregateType string
	// AggregateID is that object's ID.
	AggregateID uuid.UUID
	// Payload is the event message in protobuf JSON.
	Payload string
	// OccurredAt is when the change was committed (the row's created_at).
	OccurredAt time.Time
}

// Store reads pending events and marks them published. PGStore is the real
// one.
type Store interface {
	// Pending returns at most limit unpublished events in ID order.
	Pending(ctx context.Context, limit int) ([]Event, error)
	// MarkPublished records that the events with these IDs need no more
	// delivery.
	MarkPublished(ctx context.Context, ids []int64) error
}

// Subscriber is one consumer and the event types it receives.
type Subscriber struct {
	// Name identifies the subscriber in logs, usually its address.
	Name string
	// Types are the full protobuf names of the events it receives.
	Types []string
	// Client calls the subscriber's EventSink.
	Client eventsv1.EventSinkClient
}

// Options configure a Relay. Zero durations and sizes take the defaults.
type Options struct {
	Store       Store
	Subscribers []Subscriber
	Log         *slog.Logger

	PollInterval time.Duration
	BatchSize    int
	CallTimeout  time.Duration
	MinBackoff   time.Duration
	MaxBackoff   time.Duration
}

// Relay delivers the outbox. Its Run method must not be called twice at
// once.
type Relay struct {
	store Store
	subs  []*subscriber
	log   *slog.Logger

	poll        time.Duration
	batch       int
	callTimeout time.Duration
	backoff     backoff

	// now is time.Now; tests replace it to move the clock.
	now func() time.Time
}

// subscriber is a Subscriber with its delivery state.
type subscriber struct {
	name   string
	types  map[string]bool
	client eventsv1.EventSinkClient

	// failures counts retryable answers in a row; retryAt is when the
	// subscriber gets its next call.
	failures int
	retryAt  time.Time
}

// New checks the options and returns a relay. It does not start it.
func New(o Options) (*Relay, error) {
	if o.Store == nil {
		return nil, errors.New("relay: no store")
	}
	if len(o.Subscribers) == 0 {
		return nil, errors.New("relay: no subscribers")
	}
	r := &Relay{
		store:       o.Store,
		log:         o.Log,
		poll:        or(o.PollInterval, DefaultPollInterval),
		batch:       or(o.BatchSize, DefaultBatchSize),
		callTimeout: or(o.CallTimeout, DefaultCallTimeout),
		backoff:     backoff{min: or(o.MinBackoff, DefaultMinBackoff), max: or(o.MaxBackoff, DefaultMaxBackoff)},
		now:         time.Now,
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.backoff.max < r.backoff.min {
		return nil, fmt.Errorf("relay: max backoff %s is below min backoff %s", r.backoff.max, r.backoff.min)
	}
	names := map[string]bool{}
	for _, s := range o.Subscribers {
		switch {
		case s.Name == "":
			return nil, errors.New("relay: a subscriber has no name")
		case names[s.Name]:
			return nil, fmt.Errorf("relay: subscriber %s is listed twice", s.Name)
		case s.Client == nil:
			return nil, fmt.Errorf("relay: subscriber %s has no client", s.Name)
		case len(s.Types) == 0:
			return nil, fmt.Errorf("relay: subscriber %s receives no event types", s.Name)
		}
		names[s.Name] = true
		sub := &subscriber{name: s.Name, client: s.Client, types: map[string]bool{}}
		for _, t := range s.Types {
			sub.types[t] = true
		}
		r.subs = append(r.subs, sub)
	}
	return r, nil
}

// Run delivers events until ctx is canceled. A failing database is retried
// with a growing pause, never fatal: events wait in the outbox meanwhile.
func (r *Relay) Run(ctx context.Context) {
	var storeFailures int
	for {
		marked, full, err := r.pass(ctx)
		if ctx.Err() != nil {
			return
		}
		wait := r.poll
		switch {
		case err != nil:
			storeFailures++
			wait = r.backoff.after(storeFailures)
			r.log.Error("event relay: outbox not reachable; events wait there",
				slog.String("err", err.Error()), slog.String("retry_in", wait.String()))
		case full && marked > 0:
			// More events are waiting and the last pass made progress.
			storeFailures = 0
			continue
		default:
			storeFailures = 0
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// pass reads one batch of pending events, delivers them and marks the ones
// that need no more delivery. It returns how many it marked and whether
// the batch was full, so Run knows whether more may be waiting.
func (r *Relay) pass(ctx context.Context) (marked int, full bool, err error) {
	events, err := r.store.Pending(ctx, r.batch)
	if err != nil {
		return 0, false, fmt.Errorf("read pending events: %w", err)
	}
	var done []int64
	for _, ev := range events {
		if r.deliver(ctx, ev) {
			done = append(done, ev.ID)
		}
		if ctx.Err() != nil {
			break
		}
	}
	if len(done) > 0 {
		// Mark them even when the relay is stopping: they were delivered,
		// and marking them now saves a redelivery after the restart.
		markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
		defer cancel()
		if err := r.store.MarkPublished(markCtx, done); err != nil {
			return 0, false, fmt.Errorf("mark %d events published: %w", len(done), err)
		}
	}
	return len(done), len(events) == r.batch, nil
}

// deliver sends ev to every subscriber of its type that is not waiting out
// a backoff. It reports whether ev is finished: every subscriber of its
// type answered OK or refused it for good, which includes the case of no
// subscribers at all.
func (r *Relay) deliver(ctx context.Context, ev Event) bool {
	finished := true
	for _, s := range r.subs {
		if !s.types[ev.EventType] {
			continue
		}
		if r.now().Before(s.retryAt) {
			finished = false
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, r.callTimeout)
		_, err := s.client.Deliver(callCtx, request(ev))
		cancel()
		if ctx.Err() != nil {
			return false // stopping; the event stays pending
		}
		attrs := []any{
			slog.String("subscriber", s.name),
			slog.String("event_id", ev.EventID.String()),
			slog.String("event_type", ev.EventType),
		}
		switch classify(err) {
		case delivered:
			r.recovered(s)
		case refused:
			r.recovered(s)
			r.log.Error("event relay: subscriber refused an event for good; it is not sent there again",
				append(attrs, slog.String("code", status.Code(err).String()), slog.String("err", err.Error()))...)
		case retry:
			s.failures++
			wait := r.backoff.after(s.failures)
			s.retryAt = r.now().Add(wait)
			r.log.Warn("event relay: delivery failed; the subscriber's events wait",
				append(attrs, slog.String("code", status.Code(err).String()), slog.String("err", err.Error()),
					slog.Int("failures", s.failures), slog.String("retry_in", wait.String()))...)
			finished = false
		}
	}
	return finished
}

// recovered resets s after an answer that shows it is up. retryAt is
// already in the past, or s would not have been called.
func (r *Relay) recovered(s *subscriber) {
	if s.failures > 0 {
		r.log.Info("event relay: subscriber answers again",
			slog.String("subscriber", s.name), slog.Int("failures", s.failures))
	}
	s.failures = 0
}

// request turns an outbox row into a DeliverRequest.
func request(ev Event) *eventsv1.DeliverRequest {
	return &eventsv1.DeliverRequest{
		EventId:       ev.EventID.String(),
		EventType:     ev.EventType,
		AggregateType: ev.AggregateType,
		AggregateId:   ev.AggregateID.String(),
		OccurredAt:    timestamppb.New(ev.OccurredAt),
		Payload:       ev.Payload,
	}
}

// outcome is what the relay does after a Deliver answer.
type outcome int

const (
	delivered outcome = iota // OK: done with this subscriber
	refused                  // the subscriber can never handle it: done, logged
	retry                    // try again later, never drop
)

// classify maps a Deliver answer to an outcome (ADR 0014). Only
// INVALID_ARGUMENT and FAILED_PRECONDITION are permanent. UNIMPLEMENTED is
// retried: gRPC also answers it when the server does not serve EventSink
// at all, and dropping every event because a consumer is not ready yet
// would lose them. Every other code is retried too, so nothing is dropped
// by mistake.
func classify(err error) outcome {
	switch status.Code(err) {
	case codes.OK:
		return delivered
	case codes.InvalidArgument, codes.FailedPrecondition:
		return refused
	default:
		return retry
	}
}

// backoff is the pause after a number of failures in a row: min, doubled
// for each further failure, at most max.
type backoff struct {
	min, max time.Duration
}

func (b backoff) after(failures int) time.Duration {
	d := b.min
	for i := 1; i < failures && d < b.max; i++ {
		d *= 2
	}
	return min(d, b.max)
}

func or[T comparable](v, fallback T) T {
	var zero T
	if v == zero {
		return fallback
	}
	return v
}
