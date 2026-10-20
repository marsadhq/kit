package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/kit/clock"
)

func TestRateLimiter_AllowsBurstThenBlocks(t *testing.T) {
	fk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	rl := NewRateLimiter(fk, 1, 5)

	for i := 0; i < 5; i++ {
		if !rl.Allow("k", fk.Now()) {
			t.Fatalf("burst request %d: expected allowed", i+1)
		}
	}
	if rl.Allow("k", fk.Now()) {
		t.Fatal("6th request within burst window: expected blocked")
	}

	fk.Advance(1 * time.Second)
	if !rl.Allow("k", fk.Now()) {
		t.Fatal("after 1s refill: expected one more allowed request")
	}
	if rl.Allow("k", fk.Now()) {
		t.Fatal("immediately after the refilled request: expected blocked again")
	}
}

func TestRateLimiter_PerKeyIsolation(t *testing.T) {
	fk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	rl := NewRateLimiter(fk, 1, 3)

	for i := 0; i < 3; i++ {
		if !rl.Allow("ip-a|ping", fk.Now()) {
			t.Fatalf("ip-a request %d: expected allowed", i+1)
		}
	}
	if rl.Allow("ip-a|ping", fk.Now()) {
		t.Fatal("ip-a: expected exhausted burst to block")
	}
	for i := 0; i < 3; i++ {
		if !rl.Allow("ip-b|ping", fk.Now()) {
			t.Fatalf("ip-b request %d: expected its own full burst, unaffected by ip-a", i+1)
		}
	}
}

func TestRateLimiter_EndpointIsolation(t *testing.T) {
	fk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	rl := NewRateLimiter(fk, 1, 2)

	for i := 0; i < 2; i++ {
		if !rl.Allow("1.2.3.4|login", fk.Now()) {
			t.Fatalf("login request %d: expected allowed", i+1)
		}
	}
	if rl.Allow("1.2.3.4|login", fk.Now()) {
		t.Fatal("login: expected exhausted burst to block")
	}
	// Same IP, different endpoint key: independent bucket.
	for i := 0; i < 2; i++ {
		if !rl.Allow("1.2.3.4|ping", fk.Now()) {
			t.Fatalf("ping request %d: expected its own full burst, unaffected by login", i+1)
		}
	}
}

func TestRateLimiter_GCEvictsIdleBuckets(t *testing.T) {
	fk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	rl := NewRateLimiter(fk, 1, 1)

	rl.Allow("stale-key", fk.Now())
	if len(rl.buckets) != 1 {
		t.Fatalf("buckets after seed: got %d, want 1", len(rl.buckets))
	}

	// Advance well past the idle TTL and touch a new key; gc runs opportunistically
	// inside Allow and should drop the stale bucket.
	fk.Advance(rateLimiterIdleTTL + time.Minute)
	rl.Allow("fresh-key", fk.Now())

	if _, ok := rl.buckets["stale-key"]; ok {
		t.Error("expected stale-key to be evicted after exceeding idle TTL")
	}
	if _, ok := rl.buckets["fresh-key"]; !ok {
		t.Error("expected fresh-key to be present")
	}
}

func TestClientIP_IgnoresXFFByDefault(t *testing.T) {

	r1 := httptest.NewRequest(http.MethodGet, "/", nil)
	r1.RemoteAddr = "10.0.0.1:5555"
	r1.Header.Set("X-Forwarded-For", "1.2.3.4")

	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.RemoteAddr = "10.0.0.1:6666"
	r2.Header.Set("X-Forwarded-For", "9.9.9.9")

	ip1 := ClientIP(r1, false)
	ip2 := ClientIP(r2, false)
	if ip1 != ip2 {
		t.Fatalf("forged XFF changed the key: %q vs %q, want equal (both keyed on RemoteAddr)", ip1, ip2)
	}
	if ip1 != "10.0.0.1" {
		t.Fatalf("ClientIP = %q, want RemoteAddr host 10.0.0.1", ip1)
	}
}

func TestClientIP_HonorsXFFWhenTrustProxySet(t *testing.T) {

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")

	if got := ClientIP(r, true); got != "203.0.113.7" {
		t.Fatalf("ClientIP = %q, want left-most XFF entry 203.0.113.7", got)
	}
}

// TestClientIP_XFFMustBeAnAddress pins what a forwarded entry may turn into a
// bucket key. Only an IP address counts, in one canonical spelling; any other
// header value (a name, junk, a huge string) falls back to the TCP peer, so the
// header cannot be used to mint arbitrary or arbitrarily large keys.
func TestClientIP_XFFMustBeAnAddress(t *testing.T) {
	const peer = "10.0.0.1"

	cases := []struct {
		name, xff, want string
	}{
		{"ipv4", "203.0.113.7", "203.0.113.7"},
		{"ipv4 with port", "203.0.113.7:4711", "203.0.113.7"},
		{"ipv4-mapped ipv6 is unmapped", "::ffff:203.0.113.7", "203.0.113.7"},
		{"ipv6 is canonicalised", "2001:DB8:0:0::1", "2001:db8::1"},
		{"ipv6 with port", "[2001:db8::1]:443", "2001:db8::1"},
		{"ipv6 zone is dropped", "fe80::1%eth0", "fe80::1"},
		{"surrounding space", "  203.0.113.7 , 10.0.0.1", "203.0.113.7"},
		{"empty header", "", peer},
		{"empty first entry", ", 203.0.113.7", peer},
		{"hostname", "client.example", peer},
		{"junk", "not an address", peer},
		{"huge value", strings.Repeat("a", 64<<10), peer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = peer + ":5555"
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := ClientIP(r, true); got != c.want {
				t.Errorf("ClientIP with X-Forwarded-For %.40q = %q, want %q", c.xff, got, c.want)
			}
		})
	}
}

// TestRateLimiter_RetryAfter: a rejected client is told how long an empty
// bucket takes to hold a token again, never less than a second.
func TestRateLimiter_RetryAfter(t *testing.T) {
	fk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	for _, c := range []struct {
		rate float64
		want string
	}{
		{10, "1"},   // a token every 100ms
		{1, "1"},    // the login and ping limiters: a token every second
		{0.1, "10"}, // a token every 10 seconds
		{0.3, "4"},  // rounded up
	} {
		if got := NewRateLimiter(fk, c.rate, 5).RetryAfter(); got != c.want {
			t.Errorf("retryAfter at %v tokens/s = %q, want %q", c.rate, got, c.want)
		}
	}
}

// TestRateLimiter_HasDoesNotConsumeOrAllocate: Has reports whether a token is
// there without taking it, and asking about a new key creates no bucket.
func TestRateLimiter_HasDoesNotConsumeOrAllocate(t *testing.T) {
	fk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	rl := NewRateLimiter(fk, 1, 2)

	for i := 0; i < 5; i++ {
		if !rl.Has("new", fk.Now()) {
			t.Fatal("Has on an unseen key: want true (a full bucket)")
		}
	}
	if len(rl.buckets) != 0 {
		t.Fatalf("Has created %d buckets, want 0", len(rl.buckets))
	}
	rl.Allow("k", fk.Now())
	rl.Allow("k", fk.Now())
	if rl.Has("k", fk.Now()) {
		t.Fatal("Has on an empty bucket: want false")
	}
	fk.Advance(time.Second)
	if !rl.Has("k", fk.Now()) || !rl.Has("k", fk.Now()) {
		t.Fatal("Has after a refill: want true, and still true on asking again")
	}
}

// TestRateLimiter_Limit drives a wrapped handler past its burst from one
// client: the over-budget request is answered 429 with Retry-After and never
// reaches the handler, other endpoints and other clients keep their own
// budgets, and the bucket refills on the limiter's clock.
func TestRateLimiter_Limit(t *testing.T) {
	fk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	rl := NewRateLimiter(fk, 1, 5)
	reached := 0
	next := func(w http.ResponseWriter, _ *http.Request) { reached++; w.WriteHeader(http.StatusUnauthorized) }
	peer := func(r *http.Request) string { return ClientIP(r, false) }
	login := rl.Limit("login", peer, next)
	other := rl.Limit("other", peer, next)

	call := func(h http.HandlerFunc, addr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}
	for i := 0; i < 5; i++ {
		if rec := call(login, "203.0.113.9:4444"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want the handler's 401", i+1, rec.Code)
		}
	}
	rec := call(login, "203.0.113.9:5555") // another source port, same client
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt: status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("429 Retry-After = %q, want %q", got, "1")
	}
	if reached != 5 {
		t.Fatalf("handler reached %d times, want 5", reached)
	}
	if rec := call(other, "203.0.113.9:4444"); rec.Code != http.StatusUnauthorized {
		t.Errorf("another endpoint: status = %d, want its own budget", rec.Code)
	}
	if rec := call(login, "198.51.100.1:4444"); rec.Code != http.StatusUnauthorized {
		t.Errorf("another client: status = %d, want its own budget", rec.Code)
	}
	fk.Advance(time.Second)
	if rec := call(login, "203.0.113.9:4444"); rec.Code != http.StatusUnauthorized {
		t.Errorf("after a 1s refill: status = %d, want one more allowed", rec.Code)
	}
}

// TestEnvFlag pins the affirmative spellings; everything else is off.
func TestEnvFlag(t *testing.T) {
	const name = "KIT_TEST_FLAG"
	for v, want := range map[string]bool{
		"1": true, "true": true, "TRUE": true, " yes ": true, "On": true,
		"": false, "0": false, "false": false, "no": false, "off": false, "enabled": false,
	} {
		t.Setenv(name, v)
		if got := EnvFlag(name); got != want {
			t.Errorf("EnvFlag with %q = %v, want %v", v, got, want)
		}
	}
}
