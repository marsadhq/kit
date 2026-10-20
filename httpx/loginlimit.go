package httpx

// LoginLimiter is the login rate limiter Ward runs: a consecutive-failure
// counter with exponential backoff, keyed on (ip, email), plus a coarser
// per-ip aggregate that catches an attacker rotating emails to stay under the
// per-account threshold.

import (
	"sync"
	"time"

	"github.com/marsadhq/kit/clock"
)

const (
	// loginFailureThreshold is the free budget of consecutive (ip,email)
	// failures before a lockout is armed.
	loginFailureThreshold = 5
	// loginIPThreshold is the coarser per-ip failure budget; deliberately
	// loose so a shared tailnet exit or a fumbling operator isn't caught,
	// while a scripted email-rotation sweep still trips it.
	loginIPThreshold = 50
	// loginBackoffBase and loginMaxBackoff bound the exponential lockout:
	// base * 2^(failures-threshold), capped at maxBackoff.
	loginBackoffBase = 5 * time.Second
	loginMaxBackoff  = 30 * time.Minute
	// loginIdleTTL is how long an unlocked entry survives without activity
	// before it is eligible for GC.
	loginIdleTTL = time.Hour
	// loginMaxEntries hard-bounds each map: email and ip are
	// attacker-controlled, so an unbounded map would be a memory-DoS
	// surface. Realistic cardinality on a single-operator tailnet is tiny;
	// this is a backstop.
	loginMaxEntries = 4096
	// LoginGCInterval is how often a server's background sweep should call
	// LoginLimiter.GC to purge idle, unlocked entries. Small relative to
	// loginIdleTTL (1h) so stale entries never linger more than one interval
	// past their TTL.
	LoginGCInterval = 10 * time.Minute
)

// loginInflightRetry is the Retry-After for an attempt refused because as
// many checks for its key as its remaining budget allows are already running:
// they settle within a password check's time.
const loginInflightRetry = time.Second

// limiterEntry tracks one key's consecutive-failure state.
type limiterEntry struct {
	failures  int
	inflight  int       // credential checks reserved (Reserve) and not yet settled
	lockUntil time.Time // zero == not locked
	lastSeen  time.Time
}

// allowance is how many credential checks may run at once for an unlocked
// entry: its remaining failure budget, but at least one, so a key whose
// lockout has expired still gets its single next try.
func (e *limiterEntry) allowance(threshold int) int {
	if n := threshold - e.failures; n > 1 {
		return n
	}
	return 1
}

// idle reports whether e can be reclaimed: unlocked, nothing in flight, and
// untouched for loginIdleTTL.
func (e *limiterEntry) idle(now time.Time) bool {
	return e.inflight == 0 && !now.Before(e.lockUntil) && now.Sub(e.lastSeen) > loginIdleTTL
}

// LoginLimiter bounds login attempt rate per (ip, email) and per ip. email is
// the login name exactly as the tool identifies the account (Ward passes the
// normalized email, so case or whitespace variation cannot evade it). Safe for
// concurrent use; one mutex guards both maps since login is not a throughput
// path.
type LoginLimiter struct {
	clk clock.Clock

	mu        sync.Mutex
	byAccount map[string]*limiterEntry // key: ip+"\x00"+email
	byIP      map[string]*limiterEntry // key: ip
}

// NewLoginLimiter constructs a LoginLimiter driven by clk.
func NewLoginLimiter(clk clock.Clock) *LoginLimiter {
	return &LoginLimiter{
		clk:       clk,
		byAccount: make(map[string]*limiterEntry),
		byIP:      make(map[string]*limiterEntry),
	}
}

func accountKey(ip, email string) string { return ip + "\x00" + email }

// Blocked reports whether a new attempt for (ip,email) would be refused --
// locked out by either the per-account or the per-ip counter, or as many
// checks already in flight as the remaining budget allows -- and for how
// long (a Retry-After). It also lazily reclaims idle entries it touches.
// Reserve makes the same decision and claims the attempt atomically; this is
// the read-only form.
func (l *LoginLimiter) Blocked(ip, email string) (retryAfter time.Duration, blocked bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.blockedLocked(ip, email, l.clk.Now())
}

// blockedLocked is Blocked with l.mu held.
func (l *LoginLimiter) blockedLocked(ip, email string, now time.Time) (retryAfter time.Duration, blocked bool) {
	check := func(m map[string]*limiterEntry, key string, threshold int) {
		e, ok := m[key]
		if !ok {
			return
		}
		switch {
		case now.Before(e.lockUntil):
			blocked = true
			if d := e.lockUntil.Sub(now); d > retryAfter {
				retryAfter = d
			}
		case e.inflight >= e.allowance(threshold):
			blocked = true
			if loginInflightRetry > retryAfter {
				retryAfter = loginInflightRetry
			}
		case e.idle(now):
			delete(m, key)
		}
	}
	check(l.byAccount, accountKey(ip, email), loginFailureThreshold)
	check(l.byIP, ip, loginIPThreshold)
	return retryAfter, blocked
}

// LoginAttempt is one reserved credential check. Exactly one of Fail,
// Succeed or Cancel settles it; later calls are no-ops.
type LoginAttempt struct {
	l       *LoginLimiter
	ip      string
	email   string
	account *limiterEntry
	byIP    *limiterEntry
	settled bool
}

// Reserve decides whether one more credential check may run for (ip,email)
// and, when it may, counts it as in flight -- in ONE critical section, so
// concurrent attempts cannot all pass a check that none of them has failed
// yet. With the budget at n failures, at most n checks run at once, and a
// key whose lockout has expired gets one. When refused, retryAfter is how
// long to wait. The caller must settle the returned attempt.
func (l *LoginLimiter) Reserve(ip, email string) (attempt *LoginAttempt, retryAfter time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clk.Now()
	if d, blocked := l.blockedLocked(ip, email, now); blocked {
		return nil, d, false
	}
	a := &LoginAttempt{l: l, ip: ip, email: email,
		account: l.entry(l.byAccount, accountKey(ip, email), now),
		byIP:    l.entry(l.byIP, ip, now)}
	a.account.inflight++
	a.byIP.inflight++
	return a, 0, true
}

// entry returns key's entry in m, creating it if absent, and marks it seen.
func (l *LoginLimiter) entry(m map[string]*limiterEntry, key string, now time.Time) *limiterEntry {
	e, ok := m[key]
	if !ok {
		e = &limiterEntry{}
		l.insert(m, key, e)
	}
	e.lastSeen = now
	return e
}

// release ends the attempt's in-flight reservation, reporting false when it
// was already settled. l.mu must be held.
func (a *LoginAttempt) release() bool {
	if a.settled {
		return false
	}
	a.settled = true
	a.account.inflight--
	a.byIP.inflight--
	return true
}

// Fail settles the attempt as a failed credential check (see Fail).
func (a *LoginAttempt) Fail() {
	a.l.mu.Lock()
	defer a.l.mu.Unlock()
	if a.release() {
		a.l.failLocked(a.ip, a.email)
	}
}

// Succeed settles the attempt as a successful login (see Reset).
func (a *LoginAttempt) Succeed() {
	a.l.mu.Lock()
	defer a.l.mu.Unlock()
	if a.release() {
		a.l.resetLocked(a.ip, a.email)
	}
}

// Cancel settles the attempt without a verdict (no password was checked,
// e.g. the server was too busy): nothing is counted against the key.
func (a *LoginAttempt) Cancel() {
	a.l.mu.Lock()
	defer a.l.mu.Unlock()
	a.release()
}

// Fail records a failed attempt against both the per-account and per-ip
// counters, arming a lockout on either once its threshold is crossed.
func (l *LoginLimiter) Fail(ip, email string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failLocked(ip, email)
}

// failLocked is Fail with l.mu held.
func (l *LoginLimiter) failLocked(ip, email string) {
	now := l.clk.Now()
	l.fail(l.byAccount, accountKey(ip, email), loginFailureThreshold, now)
	l.fail(l.byIP, ip, loginIPThreshold, now)
}

// fail increments key's failure count in m (creating the entry if absent, or
// if the existing one is idle and unlocked), arming lockUntil once threshold
// is crossed.
func (l *LoginLimiter) fail(m map[string]*limiterEntry, key string, threshold int, now time.Time) {
	e, ok := m[key]
	if ok && e.idle(now) {
		delete(m, key)
		ok = false
	}
	if !ok {
		e = &limiterEntry{}
		l.insert(m, key, e)
	}
	e.failures++
	e.lastSeen = now
	if d := backoffDuration(e.failures, threshold); d > 0 {
		e.lockUntil = now.Add(d)
	}
}

// insert adds a new entry, evicting the soonest-to-expire one first if m is
// already at loginMaxEntries.
func (l *LoginLimiter) insert(m map[string]*limiterEntry, key string, e *limiterEntry) {
	if len(m) >= loginMaxEntries {
		l.evictSoonest(m)
	}
	m[key] = e
}

// evictSoonest removes the entry with the earliest lockUntil (or, if
// unlocked, lastSeen) from m. This fails safe: an actively-escalating
// attacker's entry (a future lockUntil) is never evicted ahead of a
// longer-idle or already-expired one, and an entry with a check in flight is
// never evicted at all (its in-flight count would be lost).
func (l *LoginLimiter) evictSoonest(m map[string]*limiterEntry) {
	var victim string
	var soonest time.Time
	first := true
	for k, e := range m {
		if e.inflight > 0 {
			continue
		}
		mark := e.lockUntil
		if mark.IsZero() {
			mark = e.lastSeen
		}
		if first || mark.Before(soonest) {
			soonest, victim, first = mark, k, false
		}
	}
	if !first {
		delete(m, victim)
	}
}

// Reset clears the (ip,email) entry after a successful login. The per-ip
// aggregate is deliberately left alone: one correct password must not
// launder a host's email-rotation sweep.
func (l *LoginLimiter) Reset(ip, email string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resetLocked(ip, email)
}

// resetLocked is Reset with l.mu held. An entry with other checks still in
// flight is cleared in place rather than deleted, so their reservations stay
// counted.
func (l *LoginLimiter) resetLocked(ip, email string) {
	key := accountKey(ip, email)
	e, ok := l.byAccount[key]
	switch {
	case !ok:
	case e.inflight > 0:
		e.failures = 0
		e.lockUntil = time.Time{}
	default:
		delete(l.byAccount, key)
	}
}

// GC purges idle, unlocked entries from both maps. Blocked/Fail already
// reclaim the keys they touch; GC is for entries nobody has touched since. A
// long-lived server calls it every LoginGCInterval; lazy reclamation and the
// hard entry cap bound memory without it.
func (l *LoginLimiter) GC() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clk.Now()
	gcMap(l.byAccount, now)
	gcMap(l.byIP, now)
}

func gcMap(m map[string]*limiterEntry, now time.Time) {
	for k, e := range m {
		if e.idle(now) {
			delete(m, k)
		}
	}
}

// backoffDuration returns the lockout duration for the nth consecutive
// failure against threshold: base * 2^(n-threshold), capped at
// loginMaxBackoff. Returns 0 (no lockout) below threshold.
func backoffDuration(failures, threshold int) time.Duration {
	if failures < threshold {
		return 0
	}
	offset := failures - threshold
	if offset > 32 { // 2^32 * base already dwarfs the cap; avoid a shift overflow
		offset = 32
	}
	d := loginBackoffBase * time.Duration(uint64(1)<<uint(offset))
	if d <= 0 || d > loginMaxBackoff {
		d = loginMaxBackoff
	}
	return d
}
