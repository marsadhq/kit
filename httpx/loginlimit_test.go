package httpx

// Unit tests for the login rate limiter, lifted from Ward.

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/marsadhq/kit/clock"
)

func TestBackoffDuration(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{4, 0},                // under threshold: no lockout
		{5, 5 * time.Second},  // threshold: base
		{6, 10 * time.Second}, // doubles
		{7, 20 * time.Second},
		{8, 40 * time.Second},
		{20, 30 * time.Minute},   // deep past cap: still capped
		{1000, 30 * time.Minute}, // pathological input: no overflow, still capped
	}
	for _, c := range cases {
		if got := backoffDuration(c.failures, loginFailureThreshold); got != c.want {
			t.Errorf("backoffDuration(%d, %d) = %v; want %v", c.failures, loginFailureThreshold, got, c.want)
		}
	}
}

func TestLoginLimiter_FourFailuresNotBlocked_FifthBlocked(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := NewLoginLimiter(clk)
	ip, email := "10.0.0.1", "victim@example.com"

	for i := 0; i < loginFailureThreshold-1; i++ {
		l.Fail(ip, email)
		if _, blocked := l.Blocked(ip, email); blocked {
			t.Fatalf("blocked after %d failures; want not blocked (free budget)", i+1)
		}
	}

	l.Fail(ip, email)
	d, blocked := l.Blocked(ip, email)
	if !blocked {
		t.Fatal("not blocked after the 5th consecutive failure; want blocked")
	}
	if d != loginBackoffBase {
		t.Errorf("retry-after = %v; want %v", d, loginBackoffBase)
	}
}

func TestLoginLimiter_LockoutExpiresAfterAdvancingClock(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := NewLoginLimiter(clk)
	ip, email := "10.0.0.1", "victim@example.com"

	for i := 0; i < loginFailureThreshold; i++ {
		l.Fail(ip, email)
	}
	if _, blocked := l.Blocked(ip, email); !blocked {
		t.Fatal("want blocked immediately after threshold failures")
	}

	clk.Advance(loginBackoffBase + time.Second)
	if _, blocked := l.Blocked(ip, email); blocked {
		t.Fatal("want unblocked once the lockout duration has elapsed")
	}
}

func TestLoginLimiter_ResetClearsAccountEntryOnly(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := NewLoginLimiter(clk)
	ip, email := "10.0.0.1", "victim@example.com"

	for i := 0; i < loginFailureThreshold; i++ {
		l.Fail(ip, email)
	}
	if _, blocked := l.Blocked(ip, email); !blocked {
		t.Fatal("want blocked before Reset")
	}

	l.Reset(ip, email)
	if _, blocked := l.Blocked(ip, email); blocked {
		t.Fatal("want not blocked after Reset")
	}
	entry, ok := l.byIP[ip]
	if !ok || entry.failures == 0 {
		t.Fatal("want the per-ip aggregate entry to survive Reset with its failure count intact")
	}
}

func TestLoginLimiter_IPAggregateTripsAcrossDistinctEmails(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := NewLoginLimiter(clk)
	ip := "10.0.0.1"

	// Each (ip,email) pair sees exactly one failure -- far under
	// loginFailureThreshold -- but the ip aggregate accumulates
	// loginIPThreshold failures across the distinct emails.
	for i := 0; i < loginIPThreshold-1; i++ {
		email := fmt.Sprintf("user-%d@example.com", i)
		l.Fail(ip, email)
		if _, blocked := l.Blocked(ip, email); blocked {
			t.Fatalf("iteration %d: per-account entry blocked after 1 failure; want not blocked", i)
		}
	}
	l.Fail(ip, fmt.Sprintf("user-%d@example.com", loginIPThreshold-1))

	if _, blocked := l.Blocked(ip, "brand-new@example.com"); !blocked {
		t.Fatal("want the ip aggregate to block a never-seen email after loginIPThreshold distinct failures")
	}
}

func TestLoginLimiter_GCDropsIdleUnlockedEntries(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := NewLoginLimiter(clk)
	ip, email := "10.0.0.1", "idle@example.com"

	l.Fail(ip, email) // one failure: unlocked, just sits idle afterwards
	if _, ok := l.byAccount[accountKey(ip, email)]; !ok {
		t.Fatal("want an entry recorded after Fail")
	}

	clk.Advance(loginIdleTTL + time.Minute)
	l.GC()

	if _, ok := l.byAccount[accountKey(ip, email)]; ok {
		t.Error("want the idle unlocked entry purged by GC")
	}
	if _, ok := l.byIP[ip]; ok {
		t.Error("want the idle unlocked ip aggregate entry purged by GC")
	}
}

func TestLoginLimiter_GCDoesNotDropLockedEntries(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := NewLoginLimiter(clk)
	ip, email := "10.0.0.1", "locked@example.com"

	for i := 0; i < loginFailureThreshold; i++ {
		l.Fail(ip, email)
	}
	// Advance past idleTTL but the lock (30m max) hasn't necessarily expired;
	// use a short advance that clears idleTTL relative to lastSeen only if
	// the entry were unlocked -- here it is still within its lockUntil.
	clk.Advance(loginBackoffBase) // exactly the 5s lock: now unblocked but not idle
	l.GC()
	if _, ok := l.byAccount[accountKey(ip, email)]; !ok {
		t.Fatal("want a just-expired-lock entry to survive GC (not idle yet)")
	}
}

func TestLoginLimiter_MaxEntriesCapBoundsMapSize(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := NewLoginLimiter(clk)

	for i := 0; i < loginMaxEntries+100; i++ {
		l.Fail(fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256), "attacker@example.com")
		clk.Advance(time.Millisecond)
	}
	if len(l.byAccount) > loginMaxEntries {
		t.Errorf("byAccount grew to %d entries; want <= %d", len(l.byAccount), loginMaxEntries)
	}
	if len(l.byIP) > loginMaxEntries {
		t.Errorf("byIP grew to %d entries; want <= %d", len(l.byIP), loginMaxEntries)
	}
}

func TestLoginLimiter_ConcurrentAccess(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := NewLoginLimiter(clk)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := fmt.Sprintf("10.0.0.%d", i%5)
			email := fmt.Sprintf("user-%d@example.com", i%7)
			l.Fail(ip, email)
			l.Blocked(ip, email)
			l.Reset(ip, email)
			l.GC()
		}(i)
	}
	wg.Wait()
}

// Reserve counts checks in flight against the remaining budget: with no
// failures yet, five may run at once and the sixth is refused; settling one
// (Cancel) frees its place, a failure uses up budget, and after the lockout
// expires exactly one check at a time is allowed.
func TestLoginLimiter_ReserveCountsInflightAttempts(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := NewLoginLimiter(clk)
	ip, email := "10.0.0.1", "victim@example.com"

	var held []*LoginAttempt
	for i := 0; i < loginFailureThreshold; i++ {
		a, _, ok := l.Reserve(ip, email)
		if !ok {
			t.Fatalf("reservation %d refused; want %d allowed in flight", i+1, loginFailureThreshold)
		}
		held = append(held, a)
	}
	if _, d, ok := l.Reserve(ip, email); ok || d != loginInflightRetry {
		t.Fatalf("6th concurrent reservation: ok=%v retry=%v; want refused with %v", ok, d, loginInflightRetry)
	}

	held[0].Cancel()
	held[0].Cancel() // settling twice is a no-op
	again, _, ok := l.Reserve(ip, email)
	if !ok {
		t.Fatal("reservation after a Cancel refused; want the freed place reusable")
	}
	held[0] = again

	for _, a := range held {
		a.Fail()
	}
	if _, d, ok := l.Reserve(ip, email); ok || d != loginBackoffBase {
		t.Fatalf("after %d failures: ok=%v retry=%v; want locked for %v", loginFailureThreshold, ok, d, loginBackoffBase)
	}

	clk.Advance(loginBackoffBase)
	one, _, ok := l.Reserve(ip, email)
	if !ok {
		t.Fatal("after the lockout expired: refused; want one attempt allowed")
	}
	if _, _, ok := l.Reserve(ip, email); ok {
		t.Fatal("a second concurrent attempt after the lockout; want refused")
	}
	one.Succeed()
	if _, _, ok := l.Reserve(ip, email); !ok {
		t.Fatal("after a success: refused; want the account budget restored")
	}
}

// A success while other checks for the same account are still in flight
// keeps their reservations counted, and GC never drops an entry with a check
// in flight.
func TestLoginLimiter_InflightSurvivesResetAndGC(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := NewLoginLimiter(clk)
	ip, email := "10.0.0.1", "victim@example.com"

	winner, _, _ := l.Reserve(ip, email)
	others := make([]*LoginAttempt, 0, loginFailureThreshold-1)
	for i := 0; i < loginFailureThreshold-1; i++ {
		a, _, ok := l.Reserve(ip, email)
		if !ok {
			t.Fatalf("reservation %d refused", i+2)
		}
		others = append(others, a)
	}
	winner.Succeed()
	if e := l.byAccount[accountKey(ip, email)]; e == nil || e.inflight != loginFailureThreshold-1 {
		t.Fatalf("account entry after a concurrent success = %+v; want %d still in flight", e, loginFailureThreshold-1)
	}

	clk.Advance(loginIdleTTL + time.Minute)
	l.GC()
	if _, ok := l.byAccount[accountKey(ip, email)]; !ok {
		t.Fatal("GC dropped an account entry with checks in flight")
	}
	if _, ok := l.byIP[ip]; !ok {
		t.Fatal("GC dropped an ip entry with checks in flight")
	}
	for _, a := range others {
		a.Fail()
	}
	if e := l.byAccount[accountKey(ip, email)]; e == nil || e.inflight != 0 || e.failures != loginFailureThreshold-1 {
		t.Fatalf("account entry after the others failed = %+v; want 0 in flight, %d failures", e, loginFailureThreshold-1)
	}
}

// Concurrent reservations from many goroutines never exceed the budget and
// leave nothing in flight once all are settled (run under -race).
func TestLoginLimiter_ConcurrentReservations(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := NewLoginLimiter(clk)
	ip, email := "10.0.0.1", "victim@example.com"

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
		start   = make(chan struct{})
		release = make(chan struct{})
	)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			a, _, ok := l.Reserve(ip, email)
			if !ok {
				return
			}
			mu.Lock()
			granted++
			mu.Unlock()
			<-release
			a.Fail()
		}()
	}
	close(start)
	// Wait until every goroutine has either been granted or refused.
	deadline := time.Now().Add(5 * time.Second)
	for {
		l.mu.Lock()
		e := l.byAccount[accountKey(ip, email)]
		inflight := 0
		if e != nil {
			inflight = e.inflight
		}
		l.mu.Unlock()
		if inflight == loginFailureThreshold || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let any straggler try and be refused
	close(release)
	wg.Wait()
	if granted != loginFailureThreshold {
		t.Fatalf("granted %d concurrent reservations; want exactly %d", granted, loginFailureThreshold)
	}
	if e := l.byAccount[accountKey(ip, email)]; e.inflight != 0 || e.failures != loginFailureThreshold {
		t.Fatalf("entry after settling = %+v; want 0 in flight and %d failures", e, loginFailureThreshold)
	}
}
