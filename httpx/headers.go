// Package httpx holds the HTTP building blocks every Marsad tool's server
// shares: the security headers middleware, a server whose every connection
// phase is bounded, a keyed token-bucket rate limiter, and the login limiter.
// It depends only on the standard library and kit/clock.
package httpx

import "net/http"

// ContentSecurityPolicy is the policy SecurityHeaders sets. It is deliberately
// strict: a tool's pages load only same-origin assets and use no inline
// scripts, so 'self' suffices with no 'unsafe-inline' for scripts. htmx
// attributes are HTML attributes, not inline <script>, so they are unaffected
// by script-src. style-src is 'self' as well: htmx would otherwise inject an
// inline <style> for its request indicator at load, so a page using htmx must
// turn that off (htmx-config includeIndicatorStyles=false).
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'"

// SecurityHeaders sets defensive response headers on every route: no MIME
// sniffing, no framing, no referrer, and ContentSecurityPolicy.
//
// HSTS is intentionally omitted: the tools speak plain HTTP by default (TLS is
// terminated at a reverse proxy), and emitting HSTS over plain HTTP is either
// ignored or, once cached, can lock a browser to HTTPS against an HTTP-only
// deploy.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", ContentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}
