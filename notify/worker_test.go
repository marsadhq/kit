package notify

// These tests are in package notify (internal) so they can reach the worker's
// backoff directly. They drive the worker over memQueue, an in-memory Queue,
// with a fake clock, so backoff and give-up tests neither sleep nor touch the
// network or a database. They are Tend's worker tests, adapted from its store
// interface to Queue.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marsadhq/kit/clock"
)

// item is the test tool's delivery data: the channel and the event it
// reports, as Tend's deliveries carry.
type item struct {
	channel   int64
	eventType string
	payload   string
}

// failureType is the test tool's failure event, Tend's notification.failed.
const failureType = "notification.failed"

// claimCall records the arguments of one Claim.
type claimCall struct {
	now         time.Time
	lease, hold time.Duration
	limit       int
}

// memQueue is an in-memory Queue. It records every call so tests can assert
// the worker's finalize/reschedule decisions. Its GiveUp does what Tend's
// does: it fails the delivery and records a failure event for it, except for
// a delivery that is itself a failure report (Tend's loop guard).
type memQueue struct {
	due       []Delivery[item] // claimed (and removed) by the next Claim, up to its limit
	claimErr  error
	giveUpErr error

	claims      []claimCall
	claimed     []Delivery[item]
	delivered   []int64
	rescheduled map[int64]time.Time
	failed      []int64
	causes      []error
	emitted     []item
}

func (q *memQueue) Claim(_ context.Context, now time.Time, lease, hold time.Duration, limit int) ([]Delivery[item], error) {
	q.claims = append(q.claims, claimCall{now, lease, hold, limit})
	if q.claimErr != nil {
		return nil, q.claimErr
	}
	n := min(limit, len(q.due))
	ds := q.due[:n:n]
	q.due = q.due[n:]
	for i := range ds {
		ds[i].Attempts++ // mirror the real claim: attempts includes this one
	}
	q.claimed = append(q.claimed, ds...)
	return ds, nil
}

func (q *memQueue) MarkSent(_ context.Context, d Delivery[item]) error {
	q.delivered = append(q.delivered, d.ID)
	return nil
}

func (q *memQueue) Retry(_ context.Context, d Delivery[item], at time.Time) error {
	if q.rescheduled == nil {
		q.rescheduled = map[int64]time.Time{}
	}
	q.rescheduled[d.ID] = at
	return nil
}

func (q *memQueue) GiveUp(_ context.Context, d Delivery[item], cause error) error {
	if q.giveUpErr != nil {
		return q.giveUpErr
	}
	q.failed = append(q.failed, d.ID)
	q.causes = append(q.causes, cause)
	if d.Item.eventType != failureType {
		q.emitted = append(q.emitted, item{eventType: failureType, payload: d.Item.eventType})
	}
	return nil
}

// fakeProvider is a scripted Provider: it fails the first failN Send calls and
// succeeds thereafter, counting every call.
type fakeProvider struct {
	failN int
	calls int
	last  Message
}

func (p *fakeProvider) Send(_ context.Context, m Message) error {
	p.calls++
	p.last = m
	if p.calls <= p.failN {
		return errors.New("boom")
	}
	return nil
}

// discardLogger returns a slog.Logger that drops all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// sendVia is the test tool's SendFunc: it renders the delivery's event and
// sends it with p, as Tend's does with the channel's provider.
func sendVia(p Provider) SendFunc[item] {
	return func(ctx context.Context, d Delivery[item]) error {
		return p.Send(ctx, Message{Subject: d.Item.eventType + ": " + d.Item.payload, Body: d.Item.payload})
	}
}

// newTestWorker wires a worker over q sending through p, on a fake clock set
// to now.
func newTestWorker(q *memQueue, p Provider, now time.Time) (*Worker[item], *clock.FakeClock) {
	w := NewWorker[item](q, sendVia(p), discardLogger())
	fc := clock.NewFake(now)
	w.Clock = fc
	return w, fc
}

func due(id int64, evType, payload string, createdAt time.Time) Delivery[item] {
	return Delivery[item]{ID: id, CreatedAt: createdAt, Item: item{channel: 7, eventType: evType, payload: payload}}
}

var testNow = time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

func TestWorkerDeliversAndFinalizes(t *testing.T) {
	q := &memQueue{due: []Delivery[item]{due(1, "heartbeat.missed", "db-backup", testNow)}}
	p := &fakeProvider{}
	w, _ := newTestWorker(q, p, testNow)

	n, err := w.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if n != 1 || p.calls != 1 {
		t.Fatalf("delivered=%d sends=%d, want 1/1", n, p.calls)
	}
	if len(q.delivered) != 1 || q.delivered[0] != 1 {
		t.Errorf("MarkSent calls: %v", q.delivered)
	}
	if len(q.failed) != 0 || len(q.rescheduled) != 0 || len(q.emitted) != 0 {
		t.Errorf("unexpected finalizations: failed=%v rescheduled=%v emitted=%v",
			q.failed, q.rescheduled, q.emitted)
	}
	if p.last.Subject != "heartbeat.missed: db-backup" {
		t.Errorf("the send did not get the claimed delivery: %q", p.last.Subject)
	}
}

func TestWorkerReschedulesWithCappedBackoff(t *testing.T) {
	q := &memQueue{due: []Delivery[item]{due(1, "run.failed", `{"job_name":"x"}`, testNow)}}
	p := &fakeProvider{failN: 1000}
	w, _ := newTestWorker(q, p, testNow)

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	at, ok := q.rescheduled[1]
	if !ok {
		t.Fatal("failed delivery was not rescheduled")
	}
	// First attempt -> 1s backoff.
	if got := at.Sub(testNow); got != time.Second {
		t.Errorf("first backoff: got %v want 1s", got)
	}
	if len(q.failed) != 0 || len(q.emitted) != 0 {
		t.Errorf("young delivery must not be finalized: failed=%v emitted=%v", q.failed, q.emitted)
	}

	// A high attempt count must cap at BackoffCap.
	d := due(2, "run.failed", "", testNow)
	d.Attempts = 40 // as if claimed 40 times already
	q.due = []Delivery[item]{d}
	// Fake claim adds 1; backoff(41) must equal the cap.
	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if got := q.rescheduled[2].Sub(testNow); got != w.BackoffCap {
		t.Errorf("capped backoff: got %v want %v", got, w.BackoffCap)
	}
}

// TestWorkerBackoffSequence pins the schedule: 1s doubling per attempt up to
// the cap.
func TestWorkerBackoffSequence(t *testing.T) {
	w := NewWorker[item](&memQueue{}, nil, discardLogger())
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		32 * time.Second, 64 * time.Second, 128 * time.Second, 256 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, d := range want {
		if got := w.backoff(i + 1); got != d {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, d)
		}
	}
}

func TestWorkerGivesUpAfterMaxAgeAndRecordsTheFailure(t *testing.T) {
	old := testNow.Add(-25 * time.Hour) // older than the 24h MaxAge
	q := &memQueue{due: []Delivery[item]{due(1, "heartbeat.missed", "db-backup", old)}}
	p := &fakeProvider{failN: 1000}
	w, _ := newTestWorker(q, p, testNow)

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if len(q.failed) != 1 || q.failed[0] != 1 {
		t.Fatalf("GiveUp calls: %v", q.failed)
	}
	if len(q.emitted) != 1 {
		t.Fatalf("recorded failures: %d, want 1", len(q.emitted))
	}
	if ev := q.emitted[0]; ev.eventType != failureType || ev.payload != "heartbeat.missed" {
		t.Errorf("failure event: %+v", ev)
	}
	if q.causes[0] == nil || q.causes[0].Error() != "boom" {
		t.Errorf("GiveUp cause = %v, want the send error", q.causes[0])
	}
	if len(q.rescheduled) != 0 {
		t.Errorf("expired delivery must not be rescheduled: %v", q.rescheduled)
	}
}

// TestWorkerGivesUpJustBeforeMaxAgeIsNotYet: the give-up is at MaxAge, not a
// moment before.
func TestWorkerGivesUpJustBeforeMaxAgeIsNotYet(t *testing.T) {
	q := &memQueue{due: []Delivery[item]{due(1, "heartbeat.missed", "db-backup", testNow.Add(-24*time.Hour+time.Second))}}
	w, fc := newTestWorker(q, &fakeProvider{failN: 1000}, testNow)
	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if len(q.failed) != 0 || len(q.rescheduled) != 1 {
		t.Fatalf("failed=%v rescheduled=%v, want a retry", q.failed, q.rescheduled)
	}
	fc.Advance(time.Second)
	q.due = []Delivery[item]{due(1, "heartbeat.missed", "db-backup", testNow.Add(-24*time.Hour+time.Second))}
	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if len(q.failed) != 1 {
		t.Fatalf("failed=%v, want the give-up once MaxAge is reached", q.failed)
	}
}

// cancelingProvider cancels the worker's context from inside Send and then
// fails with the context's error, the way a send interrupted by shutdown does.
type cancelingProvider struct {
	cancel context.CancelFunc
	calls  int
}

func (p *cancelingProvider) Send(ctx context.Context, _ Message) error {
	p.calls++
	p.cancel()
	return ctx.Err()
}

// TestWorkerShutdownIsNotAFailure pins that a shutdown is not a delivery
// failure. The deliveries are older than MaxAge, so a genuine send error would
// be final (GiveUp); an error caused by the context being cancelled must
// instead leave the delivery exactly as claimed, for the lease expiry to retry
// after the restart.
func TestWorkerShutdownIsNotAFailure(t *testing.T) {
	old := testNow.Add(-25 * time.Hour)

	untouched := func(t *testing.T, q *memQueue) {
		t.Helper()
		if len(q.delivered) != 0 || len(q.failed) != 0 || len(q.rescheduled) != 0 || len(q.emitted) != 0 {
			t.Errorf("shutdown must not finalize or reschedule anything: delivered=%v failed=%v rescheduled=%v emitted=%v",
				q.delivered, q.failed, q.rescheduled, q.emitted)
		}
	}

	t.Run("cancelled mid-send", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		q := &memQueue{due: []Delivery[item]{
			due(1, "heartbeat.missed", "db-backup", old),
			due(2, "heartbeat.missed", "db-backup", old),
		}}
		p := &cancelingProvider{cancel: cancel}
		w, _ := newTestWorker(q, p, testNow)

		n, err := w.DrainOnce(ctx)
		if err != nil || n != 0 {
			t.Fatalf("DrainOnce = %d, %v; want 0, nil", n, err)
		}
		// The first send is interrupted; the rest of the batch is not attempted.
		if p.calls != 1 {
			t.Errorf("sends = %d, want 1", p.calls)
		}
		untouched(t, q)
	})

	t.Run("cancelled before the pass", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		q := &memQueue{due: []Delivery[item]{due(1, "heartbeat.missed", "db-backup", old)}}
		p := &fakeProvider{failN: 1000}
		w, _ := newTestWorker(q, p, testNow)

		n, err := w.DrainOnce(ctx)
		if err != nil || n != 0 {
			t.Fatalf("DrainOnce = %d, %v; want 0, nil", n, err)
		}
		if p.calls != 0 || len(q.claimed) != 0 {
			t.Errorf("a cancelled pass must not claim or send: claimed=%d sends=%d", len(q.claimed), p.calls)
		}
		untouched(t, q)
	})
}

// TestWorkerLeavesTheFailureRecordToGiveUp: giving up is one GiveUp call and
// nothing else; what is recorded is the queue's decision. Here the queue
// applies Tend's loop guard, so a failure report that cannot be delivered is
// finalized as failed with nothing further recorded.
func TestWorkerLeavesTheFailureRecordToGiveUp(t *testing.T) {
	old := testNow.Add(-25 * time.Hour)
	q := &memQueue{due: []Delivery[item]{due(1, failureType, "run.failed", old)}}
	w, _ := newTestWorker(q, &fakeProvider{failN: 1000}, testNow)

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if len(q.failed) != 1 || q.failed[0] != 1 {
		t.Fatalf("failed deliveries = %v, want [1]", q.failed)
	}
	if len(q.emitted) != 0 {
		t.Fatalf("emitted %+v, want nothing for a failed failure report", q.emitted)
	}
	if len(q.rescheduled) != 0 || len(q.delivered) != 0 {
		t.Fatalf("rescheduled=%v delivered=%v, want neither", q.rescheduled, q.delivered)
	}
}

// rejectingProvider refuses every send for good, the way a receiver that
// answers 400 to this notification does.
type rejectingProvider struct{ calls int }

func (p *rejectingProvider) Send(context.Context, Message) error {
	p.calls++
	return Permanent(errors.New("webhook POST ward.example: status 400"))
}

// TestWorkerGivesUpARejectedDeliveryAtOnce: a delivery refused for good is
// given up on its first attempt, long before MaxAge, with the failure
// recorded; it is not rescheduled.
func TestWorkerGivesUpARejectedDeliveryAtOnce(t *testing.T) {
	q := &memQueue{due: []Delivery[item]{due(1, "run.failed", `{"job_id":1}`, testNow)}}
	p := &rejectingProvider{}
	w, _ := newTestWorker(q, p, testNow)

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if p.calls != 1 {
		t.Errorf("sends = %d, want 1", p.calls)
	}
	if len(q.failed) != 1 || q.failed[0] != 1 {
		t.Fatalf("failed deliveries = %v, want [1]", q.failed)
	}
	if len(q.emitted) != 1 || q.emitted[0].eventType != failureType || q.emitted[0].payload != "run.failed" {
		t.Fatalf("emitted = %+v, want one failure event for run.failed", q.emitted)
	}
	if !IsPermanent(q.causes[0]) {
		t.Errorf("GiveUp cause = %v, want the permanent send error", q.causes[0])
	}
	if len(q.rescheduled) != 0 {
		t.Errorf("a rejected delivery was rescheduled: %v", q.rescheduled)
	}
}

// TestWorkerRejectedFailureReportIsNotReported keeps the loop guard for the
// immediate give-up too.
func TestWorkerRejectedFailureReportIsNotReported(t *testing.T) {
	q := &memQueue{due: []Delivery[item]{due(1, failureType, "run.failed", testNow)}}
	w, _ := newTestWorker(q, &rejectingProvider{}, testNow)

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if len(q.failed) != 1 || len(q.emitted) != 0 {
		t.Fatalf("failed=%v emitted=%+v, want the delivery failed and nothing recorded", q.failed, q.emitted)
	}
}

// TestWorkerGiveUpErrorLeavesItPending: when GiveUp fails nothing else is
// done; the lease brings the delivery back to be given up again.
func TestWorkerGiveUpErrorLeavesItPending(t *testing.T) {
	q := &memQueue{due: []Delivery[item]{due(1, "run.failed", "", testNow)}, giveUpErr: errors.New("db locked")}
	var logs bytes.Buffer
	w, _ := newTestWorker(q, &rejectingProvider{}, testNow)
	w.log = slog.New(slog.NewTextHandler(&logs, nil))

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if len(q.rescheduled) != 0 || len(q.delivered) != 0 {
		t.Fatalf("rescheduled=%v delivered=%v, want neither", q.rescheduled, q.delivered)
	}
	if !strings.Contains(logs.String(), `msg="notify: fail delivery" delivery=1 err="db locked"`) {
		t.Errorf("logs = %s", logs.String())
	}
}

// TestWorkerClaimArguments: every pass claims at the clock's time with the
// worker's lease and hold, in batches of claimBatch until the queue is empty.
func TestWorkerClaimArguments(t *testing.T) {
	q := &memQueue{}
	for i := range claimBatch + 5 {
		q.due = append(q.due, due(int64(i+1), "heartbeat.missed", "db-backup", testNow))
	}
	w, _ := newTestWorker(q, &fakeProvider{}, testNow)
	w.Lease, w.Hold = 90*time.Second, 0

	n, err := w.DrainOnce(context.Background())
	if err != nil || n != claimBatch+5 {
		t.Fatalf("DrainOnce = %d, %v; want %d delivered", n, err, claimBatch+5)
	}
	if len(q.claims) != 3 { // a full batch, the rest, and an empty claim
		t.Fatalf("claims = %d, want 3", len(q.claims))
	}
	for _, c := range q.claims {
		if !c.now.Equal(testNow) || c.lease != 90*time.Second || c.hold != 0 || c.limit != claimBatch {
			t.Errorf("claim = %+v", c)
		}
	}
	if d := NewWorker[item](q, nil, discardLogger()); d.Hold != DefaultHold || d.Lease != time.Minute ||
		d.MaxAge != 24*time.Hour || d.BackoffCap != 5*time.Minute || d.Interval != 2*time.Second {
		t.Errorf("defaults = hold %v lease %v maxAge %v cap %v interval %v", d.Hold, d.Lease, d.MaxAge, d.BackoffCap, d.Interval)
	}
	if DefaultHold != 15*time.Minute {
		t.Errorf("DefaultHold = %v, want Tend's 15m", DefaultHold)
	}
}

// TestWorkerClaimError ends the pass with the error and sends nothing.
func TestWorkerClaimError(t *testing.T) {
	q := &memQueue{claimErr: errors.New("db down")}
	p := &fakeProvider{}
	w, _ := newTestWorker(q, p, testNow)
	if _, err := w.DrainOnce(context.Background()); err == nil || p.calls != 0 {
		t.Fatalf("DrainOnce err = %v, sends = %d; want the claim error and no send", err, p.calls)
	}
}

// TestWorkerLogLines pins the log lines Tend's worker wrote, with the tool's
// attributes (here channel and event, as Tend logs them) after the delivery.
func TestWorkerLogLines(t *testing.T) {
	q := &memQueue{due: []Delivery[item]{due(1, "run.failed", "", testNow), due(2, "run.failed", "", testNow)}}
	var logs bytes.Buffer
	calls := 0
	w := NewWorker[item](q, func(context.Context, Delivery[item]) error {
		calls++
		if calls == 1 {
			return errors.New("boom")
		}
		return Permanent(errors.New("status 400"))
	}, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{ReplaceAttr: dropTime})))
	w.Clock = clock.NewFake(testNow)
	w.LogAttrs = func(d Delivery[item]) []any { return []any{"channel", d.Item.channel, "event", d.Item.eventType} }

	if _, err := w.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	for _, want := range []string{
		`level=WARN msg="notify: delivery attempt failed" delivery=1 channel=7 event=run.failed attempt=1 err=boom`,
		`level=ERROR msg="notify: delivery failed permanently" reason="rejected by the destination" delivery=2 channel=7 event=run.failed attempts=1 err="status 400"`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %s:\n%s", want, logs.String())
		}
	}
}

func dropTime(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}

// TestWorkerRetryAfter: a send error that asks for a longer wait gets it, up
// to BackoffCap; a shorter ask leaves the backoff alone.
func TestWorkerRetryAfter(t *testing.T) {
	for _, c := range []struct {
		after, want time.Duration
	}{
		{5 * time.Second, 5 * time.Second}, // first backoff is 1s: the ask wins
		{500 * time.Millisecond, time.Second},
		{time.Hour, 5 * time.Minute}, // capped
	} {
		q := &memQueue{due: []Delivery[item]{due(1, "run.failed", "", testNow)}}
		w := NewWorker[item](q, func(context.Context, Delivery[item]) error {
			return RetryLater(errors.New("status 500"), c.after)
		}, discardLogger())
		w.Clock = clock.NewFake(testNow)
		if _, err := w.DrainOnce(context.Background()); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
		if got := q.rescheduled[1].Sub(testNow); got != c.want {
			t.Errorf("RetryLater(%v): rescheduled after %v, want %v", c.after, got, c.want)
		}
	}
	if RetryLater(nil, time.Second) != nil {
		t.Error("RetryLater(nil) != nil")
	}
	if _, ok := RetryAfter(errors.New("x")); ok {
		t.Error("a plain error asked for a delay")
	}
	// A permanent error that also asks for a delay is still permanent.
	if !IsPermanent(RetryLater(Permanent(errors.New("x")), time.Second)) {
		t.Error("RetryLater hid permanence")
	}
}

// TestWorkerRunDrainsOnNudgeAndStops: Run drains at start, again on a Nudge,
// and returns once its context is cancelled.
func TestWorkerRunDrainsOnNudgeAndStops(t *testing.T) {
	var mu sync.Mutex
	sent := make(chan int64, 4)
	q := &lockedQueue{q: &memQueue{}, mu: &mu}
	w := NewWorker[item](q, func(_ context.Context, d Delivery[item]) error { sent <- d.ID; return nil }, discardLogger())
	w.Interval = time.Hour // only the start and the nudge can drain

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	mu.Lock()
	q.q.due = append(q.q.due, due(9, "heartbeat.missed", "db-backup", time.Now()))
	mu.Unlock()
	// The first pass may already have run; a nudge makes the next one prompt.
	w.Nudge()
	w.Nudge() // never blocks
	select {
	case id := <-sent:
		if id != 9 {
			t.Fatalf("sent %d, want 9", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Nudge did not trigger a drain")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// lockedQueue serialises access to a memQueue shared with a test goroutine.
type lockedQueue struct {
	q  *memQueue
	mu *sync.Mutex
}

func (l *lockedQueue) Claim(ctx context.Context, now time.Time, lease, hold time.Duration, limit int) ([]Delivery[item], error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.q.Claim(ctx, now, lease, hold, limit)
}

func (l *lockedQueue) MarkSent(ctx context.Context, d Delivery[item]) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.q.MarkSent(ctx, d)
}

func (l *lockedQueue) Retry(ctx context.Context, d Delivery[item], at time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.q.Retry(ctx, d, at)
}

func (l *lockedQueue) GiveUp(ctx context.Context, d Delivery[item], cause error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.q.GiveUp(ctx, d, cause)
}
