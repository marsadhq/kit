package notify

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/marsadhq/kit/clock"
)

// Delivery is one claimed delivery: the bookkeeping the worker drives, plus
// the tool's own data for it. Tools enqueue deliveries in their own tables,
// in the same transaction as the event they report, so a matched notification
// survives a crash between the event and its delivery.
type Delivery[T any] struct {
	// ID identifies the delivery in the tool's queue.
	ID int64
	// Attempts is the attempts made so far, INCLUDING the one just claimed.
	Attempts int
	// CreatedAt is when the delivery was enqueued. The worker gives it up
	// once it has been failing for MaxAge, counted from here.
	CreatedAt time.Time
	// Item is the tool's own data: which channel, which event. The worker
	// never looks inside; it hands Item back to Send and to the Queue.
	Item T
}

// Queue is the persistence contract the worker drives, implemented by each
// tool over its own deliveries table.
type Queue[T any] interface {
	// Claim atomically claims up to limit pending deliveries whose next
	// attempt is due (at or before now): each one's attempt count is
	// incremented and its next attempt pushed to now+lease, so a crashed
	// worker's claims become due again after the lease (at-least-once, no
	// stuck "sending" state). A pending delivery should hold back the later
	// ones on its destination, so a destination receives its notifications in
	// order, one at a time, but only until it was first attempted more than
	// hold ago; hold <= 0 turns ordering off.
	Claim(ctx context.Context, now time.Time, lease, hold time.Duration, limit int) ([]Delivery[T], error)
	// MarkSent finalizes a delivery as delivered.
	MarkSent(ctx context.Context, d Delivery[T]) error
	// Retry sets the next attempt time of a still-pending delivery.
	Retry(ctx context.Context, d Delivery[T], at time.Time) error
	// GiveUp finalizes a delivery as permanently failed: the destination
	// refused it for good (IsPermanent(cause)), or it has been failing for
	// MaxAge. It is the hook where the tool records its own failure event,
	// in the same transaction, so a dropped alert is never silent. A
	// delivery that is no longer pending should be left alone. When GiveUp
	// fails nothing should have been written: the delivery stays pending and
	// is given up again after the lease.
	GiveUp(ctx context.Context, d Delivery[T], cause error) error
}

// SendFunc performs one send attempt for d: typically it loads the channel,
// builds its Provider and sends the tool's rendered Message. Its errors are
// logged, so they must never carry a secret (provider errors never do).
type SendFunc[T any] func(ctx context.Context, d Delivery[T]) error

// DefaultHold is Worker.Hold's default: long enough to ride out a short outage
// of the destination in order, short enough that one notification it keeps
// failing does not silence the channel for the day it takes to give it up.
const DefaultHold = 15 * time.Minute

// claimBatch bounds how many due deliveries one drain pass claims at a time.
const claimBatch = 32

// Worker drains a durable deliveries queue: claim due deliveries, send each,
// and either finalize it (delivered), reschedule it with capped exponential
// backoff, or - when the destination refused it for good (IsPermanent) or
// once it is older than MaxAge - give it up through Queue.GiveUp.
//
// Delivery is asynchronous by design: emitters only enqueue (transactionally
// with the event insert) and optionally Nudge the worker, so a slow or down
// destination can never block the code that raised the event.
type Worker[T any] struct {
	queue Queue[T]
	send  SendFunc[T]
	log   *slog.Logger

	// Clock is the time source for claims, backoff and give-up; default the
	// system clock. The poll cadence (Interval) always runs on real time.
	Clock clock.Clock
	// Interval is the poll cadence between drain passes; default 2s.
	Interval time.Duration
	// Lease is how long a claim holds before the delivery becomes due again;
	// it must exceed the slowest single send (HTTP sends time out after 10s,
	// SMTP sends after 30s). Default 1m.
	Lease time.Duration
	// MaxAge is how long a delivery may keep failing before the worker gives
	// it up. Default 24h.
	MaxAge time.Duration
	// BackoffCap caps the exponential retry backoff, and any longer delay a
	// send error asks for (RetryAfter). Default 5m.
	BackoffCap time.Duration
	// Hold is how long a failing delivery may hold back the later ones on its
	// destination, counted from its first attempt; after that they go out
	// while it keeps retrying. 0 turns per-destination ordering off. Default
	// DefaultHold. It is passed to Queue.Claim, which enforces it.
	Hold time.Duration
	// LogAttrs, when set, returns extra slog key-value pairs for the worker's
	// log lines about d, after the delivery id (Tend logs the channel and the
	// event type). They must never carry a secret.
	LogAttrs func(d Delivery[T]) []any

	nudge chan struct{}
}

// NewWorker builds a delivery worker over q that sends with send, with the
// production defaults.
func NewWorker[T any](q Queue[T], send SendFunc[T], log *slog.Logger) *Worker[T] {
	return &Worker[T]{
		queue:      q,
		send:       send,
		log:        log,
		Clock:      clock.RealClock{},
		Interval:   2 * time.Second,
		Lease:      time.Minute,
		MaxAge:     24 * time.Hour,
		BackoffCap: 5 * time.Minute,
		Hold:       DefaultHold,
		nudge:      make(chan struct{}, 1),
	}
}

// Nudge wakes the worker for a prompt drain pass after an emitter enqueued a
// delivery. It never blocks: a nudge while one is already pending is a no-op
// (the pending pass will see the new row anyway).
func (w *Worker[T]) Nudge() {
	select {
	case w.nudge <- struct{}{}:
	default:
	}
}

// Run drains the queue until ctx is cancelled, waking every Interval or on a
// Nudge. Drain errors are logged, never fatal: the next pass retries.
func (w *Worker[T]) Run(ctx context.Context) {
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		if _, err := w.DrainOnce(ctx); err != nil && ctx.Err() == nil {
			w.log.Error("notify: drain deliveries", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.nudge:
		}
	}
}

// DrainOnce claims and processes every currently-due delivery, returning the
// number successfully delivered. It stops early (without error) when ctx is
// cancelled mid-batch; claimed-but-unsent deliveries simply become due again
// after the lease.
func (w *Worker[T]) DrainOnce(ctx context.Context) (int, error) {
	delivered := 0
	for {
		if ctx.Err() != nil {
			return delivered, nil
		}
		ds, err := w.queue.Claim(ctx, w.Clock.Now(), w.Lease, w.Hold, claimBatch)
		if err != nil {
			return delivered, err
		}
		if len(ds) == 0 {
			return delivered, nil
		}
		for _, d := range ds {
			if ctx.Err() != nil {
				return delivered, nil
			}
			if w.deliverOne(ctx, d) {
				delivered++
			}
		}
	}
}

// attrs returns the log key-value pairs for d: the delivery id, the tool's
// own (LogAttrs), then extra.
func (w *Worker[T]) attrs(d Delivery[T], extra ...any) []any {
	out := []any{"delivery", d.ID}
	if w.LogAttrs != nil {
		out = append(out, w.LogAttrs(d)...)
	}
	return append(out, extra...)
}

// deliverOne attempts a single claimed delivery and finalizes/reschedules it.
// It returns true only when the send succeeded.
func (w *Worker[T]) deliverOne(ctx context.Context, d Delivery[T]) bool {
	err := w.send(ctx, d)
	if err == nil {
		if merr := w.queue.MarkSent(ctx, d); merr != nil {
			// The send DID happen; a lease-expiry redelivery is possible
			// (at-least-once). Log so the duplicate is explicable.
			w.log.Error("notify: mark delivered", "delivery", d.ID, "err", merr)
		}
		return true
	}
	if ctx.Err() != nil {
		return false // shutdown mid-send: the lease expiry retries, not a failure
	}

	// Give up when the destination refused this notification for good (a
	// 4xx answer, an SMTP 5xx reply: retrying cannot help, and on an ordered
	// channel it would hold back every later alert), or once the delivery has
	// been failing for MaxAge.
	rejected := IsPermanent(err)
	if rejected || w.Clock.Now().Sub(d.CreatedAt) >= w.MaxAge {
		reason := "retried for too long"
		if rejected {
			reason = "rejected by the destination"
		}
		w.log.Error("notify: delivery failed permanently",
			append([]any{"reason", reason}, w.attrs(d, "attempts", d.Attempts, "err", err)...)...)
		if gerr := w.queue.GiveUp(ctx, d, err); gerr != nil {
			// Nothing was written: the delivery is still pending and is given
			// up again after the lease.
			w.log.Error("notify: fail delivery", "delivery", d.ID, "err", gerr)
		}
		return false
	}

	w.log.Warn("notify: delivery attempt failed", w.attrs(d, "attempt", d.Attempts, "err", err)...)
	if rerr := w.queue.Retry(ctx, d, w.Clock.Now().Add(w.retryDelay(d.Attempts, err))); rerr != nil {
		w.log.Error("notify: reschedule delivery", "delivery", d.ID, "err", rerr)
	}
	return false
}

// retryDelay is the wait before the next attempt after attempts tries that
// ended in err: the backoff, or longer when err asks for it (RetryAfter), but
// never past BackoffCap.
func (w *Worker[T]) retryDelay(attempts int, err error) time.Duration {
	d := w.backoff(attempts)
	if after, ok := RetryAfter(err); ok && after > d {
		d = min(after, w.BackoffCap) // still >= the backoff, which never exceeds the cap
	}
	return d
}

// backoff returns the wait before the next attempt after `attempts` tries:
// 1s, 2s, 4s, ... doubling up to BackoffCap.
func (w *Worker[T]) backoff(attempts int) time.Duration {
	d := time.Second
	for i := 1; i < attempts && d < w.BackoffCap; i++ {
		d *= 2
	}
	if d > w.BackoffCap {
		d = w.BackoffCap
	}
	return d
}

// retryAfterError asks the worker to wait at least after before the next
// attempt.
type retryAfterError struct {
	err   error
	after time.Duration
}

func (e *retryAfterError) Error() string { return e.err.Error() }
func (e *retryAfterError) Unwrap() error { return e.err }

// RetryLater wraps err so the worker waits at least after before retrying,
// for a destination that asks for that (Pushover wants five seconds between
// retries). The worker still never waits longer than its BackoffCap. A nil
// err stays nil.
func RetryLater(err error, after time.Duration) error {
	if err == nil {
		return nil
	}
	return &retryAfterError{err: err, after: after}
}

// RetryAfter reports the wait err asked for through RetryLater, if any.
func RetryAfter(err error) (time.Duration, bool) {
	var r *retryAfterError
	if errors.As(err, &r) {
		return r.after, true
	}
	return 0, false
}
