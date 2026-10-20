package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marsadhq/kit/httpx"
)

// wantSecurityHeaders asserts the defensive headers are present with their
// exact expected values on the given response.
func wantSecurityHeaders(t *testing.T, h http.Header) {
	t.Helper()
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options: got %q want %q", got, "nosniff")
	}
	if got := h.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options: got %q want %q", got, "DENY")
	}
	if got := h.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy: got %q want %q", got, "no-referrer")
	}
	csp := h.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy: missing")
	}
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "style-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing directive %q: %s", want, csp)
		}
	}
	if h.Get("Strict-Transport-Security") != "" {
		t.Error("did not expect an HSTS header (serve is plain-HTTP by default)")
	}
}

// TestSecurityHeaders: every response carries the headers, whatever the
// handler does, including a handler that writes an error.
func TestSecurityHeaders(t *testing.T) {
	for name, inner := range map[string]http.HandlerFunc{
		"ok":        func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) },
		"not found": http.NotFound,
		"error":     func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) },
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			httpx.SecurityHeaders(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			wantSecurityHeaders(t, rec.Header())
		})
	}
}

// TestContentSecurityPolicyIsTendsPolicy pins the exact policy text Tend
// served before it moved here.
func TestContentSecurityPolicyIsTendsPolicy(t *testing.T) {
	const want = "default-src 'self'; script-src 'self'; style-src 'self'; " +
		"img-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'"
	if httpx.ContentSecurityPolicy != want {
		t.Fatalf("CSP = %q, want %q", httpx.ContentSecurityPolicy, want)
	}
}
