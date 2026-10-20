package auth

import (
	"strings"
	"testing"
	"time"
)

// TestDummyPasswordHashMatchesRealHashes: the dummy hash has the same shape
// and cost parameters as a hash HashPassword writes, and the same salt and key
// lengths, so checking against it costs what checking a real hash costs.
func TestDummyPasswordHashMatchesRealHashes(t *testing.T) {
	stored, err := HashPassword("whatever")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	dummy := DummyPasswordHash()
	rp, dp := strings.Split(stored, "$"), strings.Split(dummy, "$")
	if len(dp) != 6 {
		t.Fatalf("dummy hash %q is not a PHC string", dummy)
	}
	for i := 0; i < 4; i++ {
		if rp[i] != dp[i] {
			t.Fatalf("dummy hash part %d = %q, real hashes have %q", i, dp[i], rp[i])
		}
	}
	if len(rp[4]) != len(dp[4]) || len(rp[5]) != len(dp[5]) {
		t.Fatalf("dummy salt/key lengths %d/%d differ from real %d/%d", len(dp[4]), len(dp[5]), len(rp[4]), len(rp[5]))
	}
	// It must take the full argon2id path, not a parse failure's early
	// return, which is what would make it cheap.
	if VerifyPassword(dummy, "") || VerifyPassword(dummy, "whatever") {
		t.Fatal("something verified against the dummy hash")
	}
}

// TestVerifyPasswordOrDummy: an empty hash never verifies, whatever the
// password, and a real one behaves exactly like VerifyPassword.
func TestVerifyPasswordOrDummy(t *testing.T) {
	h, err := HashPassword("right")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPasswordOrDummy(h, "right") {
		t.Fatal("the right password was refused")
	}
	if VerifyPasswordOrDummy(h, "wrong") {
		t.Fatal("a wrong password was accepted")
	}
	for _, pw := range []string{"", "right", "anything"} {
		if VerifyPasswordOrDummy("", pw) {
			t.Fatalf("an empty hash accepted %q", pw)
		}
	}
}

// TestVerifyPasswordOrDummyCostsAFullCheck: with no hash it still spends about
// what a real check spends. The bound is loose (a quarter) so a busy machine
// does not make it flaky; what it catches is an early return, which costs
// microseconds against tens of milliseconds.
func TestVerifyPasswordOrDummyCostsAFullCheck(t *testing.T) {
	h, err := HashPassword("right")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	timeOf := func(f func()) time.Duration {
		best := time.Duration(1<<63 - 1)
		for i := 0; i < 3; i++ {
			start := time.Now()
			f()
			best = min(best, time.Since(start))
		}
		return best
	}
	genuine := timeOf(func() { VerifyPasswordOrDummy(h, "wrong") })
	dummy := timeOf(func() { VerifyPasswordOrDummy("", "wrong") })
	if dummy < genuine/4 {
		t.Fatalf("the no-hash path took %v against %v for a real check; it is skipping the argon2id work", dummy, genuine)
	}
}
