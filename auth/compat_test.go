package auth_test

import (
	"testing"
	"time"

	"github.com/marsadhq/kit/auth"
)

// The vectors below were produced by the copies of this code that Tend and
// Ward ran before kit existed. They pin that what those tools already stored
// or handed out (password hashes, API token hashes, session cookies, CSRF
// tokens) stays valid after the move.

// TestCompat_StoredPasswordHashStillVerifies: a PHC string written by the
// original HashPassword verifies, and only for its own password.
func TestCompat_StoredPasswordHashStillVerifies(t *testing.T) {
	const stored = "$argon2id$v=19$m=19456,t=2,p=1$bttsEw45Hm1Q1caBHwmVig$OoTRrhbdnyW/gMQxqPj1mZLaz4ztUEHv680NLaxOCR4"
	if !auth.VerifyPassword(stored, "correct horse battery staple") {
		t.Fatal("a hash stored by the original code no longer verifies")
	}
	if auth.VerifyPassword(stored, "correct horse battery stapler") {
		t.Fatal("a stored hash verified the wrong password")
	}
}

// TestCompat_TokenHashesUnchanged: HashToken hashes the whole token, prefix
// included, exactly as before, so token rows stored by either tool still
// match the bearer tokens their users hold.
func TestCompat_TokenHashesUnchanged(t *testing.T) {
	for token, want := range map[string]string{
		"tend_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA": "33d918f7b7f8dd575afb8c07b232c42ad12dfa912abffd9a22c62125145ee73c",
		"ward_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA": "8da4809ed5d808551bf335574452f4b6a32248ee48c394c8ca0e0149992c6b9e",
	} {
		if got := auth.HashToken(token); got != want {
			t.Errorf("HashToken(%q) = %s, want %s", token, got, want)
		}
	}
}

// TestCompat_SessionAndCSRFEncodingUnchanged: a session cookie and its CSRF
// token are byte-for-byte what the original codec produced for the same key
// and session, and a cookie issued by it still decodes.
func TestCompat_SessionAndCSRFEncodingUnchanged(t *testing.T) {
	const (
		cookie = "AAAAAAAAACoAAAAAAAAABwAAAAD0hlcA.ErCX851rfJyv7GEpp5FXz-FjLQser78UZLjDg8Qfta8"
		csrf   = "sezrrLvA8QJhSjeUQtjO-B8Pp-ckjQYakJ_Tb3SSODY"
	)
	c := auth.NewSessionCodec(testKey())
	s := auth.Session{UserID: 42, OrgID: 7, Expiry: time.Unix(4102444800, 0)} // 2100-01-01

	if got, err := c.Encode(s); err != nil || got != cookie {
		t.Fatalf("Encode = %q, %v; want %q", got, err, cookie)
	}
	got, err := c.Decode(cookie)
	if err != nil {
		t.Fatalf("Decode of a cookie issued by the original codec: %v", err)
	}
	if got.UserID != 42 || got.OrgID != 7 || !got.Expiry.Equal(s.Expiry) {
		t.Fatalf("Decode = %+v, want %+v", got, s)
	}
	if got := c.IssueCSRF(s); got != csrf {
		t.Fatalf("IssueCSRF = %q, want %q", got, csrf)
	}
	if !c.CheckCSRF(s, csrf) {
		t.Fatal("CheckCSRF rejected a token issued by the original codec")
	}
}
