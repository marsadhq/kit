package httpx

import (
	"net/http"
	"time"
)

// DefaultTimeout is the per-phase connection limit Tend and Ward serve with.
const DefaultTimeout = 10 * time.Second

// NewServer returns an HTTP server for h listening on addr (which may be
// empty when the caller hands it a listener), with every phase of a
// connection bounded by timeout: reading the request headers, reading the
// whole request including its body, writing the response, and sitting idle
// between requests. The tools' handlers are all quick (pings, ingest, login,
// dashboard pages, API calls) and nothing long-polls or streams, so one short
// limit fits them all and a stalled or abandoned client can hold a connection
// for at most about that long instead of forever.
//
// ReadTimeout is what bounds the body. Without it a client could send complete
// headers, promise a body, and never send it: the connection and its goroutine
// then stayed open indefinitely, including on the unauthenticated routes.
func NewServer(addr string, h http.Handler, timeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: timeout,
		ReadTimeout:       timeout,
		WriteTimeout:      timeout,
		IdleTimeout:       timeout,
	}
}
