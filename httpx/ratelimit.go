package httpx

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marsadhq/kit/clock"
)

// rateLimiterIdleTTL bounds how long an idle bucket is kept before gc drops
// it, so the bucket map does not keep growing as distinct IPs churn through
// the limiter. Between two sweeps the map still holds one small entry per
// address seen.
const rateLimiterIdleTTL = 10 * time.Minute

// RateLimiter is a fixed-rate token bucket per key (an IP and endpoint, a
// token, anything the caller names). Refill is lazy (computed from elapsed
// time on each Allow), so there is no background goroutine. Idle buckets are
// evicted opportunistically, at most once per ten minutes. Safe for
// concurrent use.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens added per second
	burst   float64 // bucket capacity
	clk     clock.Clock
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter returns a RateLimiter allowing burst requests immediately and
// refilling at rate tokens/second thereafter, using clk as its time source.
func NewRateLimiter(clk clock.Clock, rate, burst float64) *RateLimiter {
	return &RateLimiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   burst,
		clk:     clk,
		lastGC:  clk.Now(),
	}
}

// Has reports whether key's bucket holds at least one token, without taking
// it. A key with no bucket yet has a full one, and none is created for it, so
// asking about an arbitrary key costs no memory.
func (rl *RateLimiter) Has(key string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b := rl.buckets[key]
	if b == nil {
		return rl.burst >= 1
	}
	return min(rl.burst, b.tokens+now.Sub(b.last).Seconds()*rl.rate) >= 1
}

// Allow reports whether one token is available for key, consuming it if so.
func (rl *RateLimiter) Allow(key string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b := rl.buckets[key]
	if b == nil {
		b = &bucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	}
	// lazy refill
	b.tokens = min(rl.burst, b.tokens+now.Sub(b.last).Seconds()*rl.rate)
	b.last = now
	allowed := false
	if b.tokens >= 1 {
		b.tokens -= 1
		allowed = true
	}
	rl.gc(now) // opportunistic eviction of stale buckets
	return allowed
}

// gc drops buckets untouched for longer than rateLimiterIdleTTL. It runs at
// most once per rateLimiterIdleTTL interval (tracked via lastGC) so it does
// not add per-request O(n) cost. Caller must hold rl.mu.
func (rl *RateLimiter) gc(now time.Time) {
	if now.Sub(rl.lastGC) < rateLimiterIdleTTL {
		return
	}
	rl.lastGC = now
	for k, b := range rl.buckets {
		if now.Sub(b.last) >= rateLimiterIdleTTL {
			delete(rl.buckets, k)
		}
	}
}

// RetryAfter is the Retry-After value, in whole seconds, for a request the
// limiter rejected: how long until an empty bucket holds a token again, and at
// least one second.
func (rl *RateLimiter) RetryAfter() string {
	secs := 1
	if rl.rate > 0 && rl.rate < 1 {
		secs = int(math.Ceil(1 / rl.rate))
	}
	return strconv.Itoa(secs)
}

// Limit wraps next so requests exceeding rl's per-(client, endpoint) budget
// get a 429 instead of reaching the handler. The bucket key is
// clientIP(r) + "|" + endpoint; pass ClientIP bound to the tool's proxy
// setting as clientIP.
func (rl *RateLimiter) Limit(endpoint string, clientIP func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := clientIP(r) + "|" + endpoint
		if !rl.Allow(key, rl.clk.Now()) {
			TooManyRequests(w, rl)
			return
		}
		next(w, r)
	}
}

// TooManyRequests answers 429 with the Retry-After of rl.
func TooManyRequests(w http.ResponseWriter, rl *RateLimiter) {
	w.Header().Set("Retry-After", rl.RetryAfter())
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
}

// ClientIP returns the request's source IP for rate-limit keying. It uses the
// TCP peer (RemoteAddr) by default; only when trustProxy is true does it honor
// the left-most X-Forwarded-For entry (set by a trusted proxy). Trusting XFF
// unconditionally would let a client forge the key and evade the limit.
//
// The entry is used only when it is an IP address (optionally with a port),
// and in its canonical form. A header is text of any length: keying on it
// verbatim would let whatever reaches the header mint a new, arbitrarily large
// bucket key per request. Anything else falls back to the TCP peer.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
		if addr, ok := forwardedAddr(first); ok {
			return addr.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// forwardedAddr parses one X-Forwarded-For entry, "ip" or "ip:port", into a
// canonical address: an IPv4-mapped IPv6 address is unmapped and a zone is
// dropped, so one client cannot spell itself several ways.
func forwardedAddr(entry string) (netip.Addr, bool) {
	entry = strings.TrimSpace(entry)
	addr, err := netip.ParseAddr(entry)
	if err != nil {
		ap, perr := netip.ParseAddrPort(entry)
		if perr != nil {
			return netip.Addr{}, false
		}
		addr = ap.Addr()
	}
	return addr.Unmap().WithZone(""), true
}

// EnvFlag reports whether the environment variable name is set to an
// affirmative value: "1", "true", "yes" or "on", in any case and with
// surrounding space ignored. Anything else, unset included, is false. It is
// how a tool reads its opt-in switches, such as TEND_TRUST_PROXY for
// ClientIP; reading it per request keeps a change to the environment
// effective without a restart, as Tend's did.
func EnvFlag(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
