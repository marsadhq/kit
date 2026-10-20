package httpx_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/marsadhq/kit/httpx"
)

// TestServerDropsStalledRequestBody proves the read timeout covers the
// request body, not only the headers. The client sends complete headers that
// promise a body and then goes silent; the handler blocked on that body must
// be released once the timeout passes instead of waiting forever.
func TestServerDropsStalledRequestBody(t *testing.T) {
	released := make(chan error, 1)
	srv := httpx.NewServer("", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		released <- err
	}), 200*time.Millisecond)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln) //nolint:errcheck
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "POST /login HTTP/1.1\r\nHost: kit\r\n"+
		"Content-Type: application/x-www-form-urlencoded\r\nContent-Length: 64\r\n\r\n"); err != nil {
		t.Fatalf("write headers: %v", err)
	}

	select {
	case err := <-released:
		if err == nil {
			t.Fatal("handler read a complete body, but none was sent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler still blocked on the request body 5s after a 200ms read timeout")
	}
}

// TestNewServerBoundsEveryPhase pins the production values: every phase of a
// connection, the body read included, is bounded by the timeout given, and
// the address is the one given.
func TestNewServerBoundsEveryPhase(t *testing.T) {
	srv := httpx.NewServer("127.0.0.1:8080", http.NotFoundHandler(), httpx.DefaultTimeout)
	if srv.Addr != "127.0.0.1:8080" {
		t.Errorf("Addr = %q", srv.Addr)
	}
	if srv.ReadHeaderTimeout != httpx.DefaultTimeout || srv.ReadTimeout != httpx.DefaultTimeout ||
		srv.WriteTimeout != httpx.DefaultTimeout || srv.IdleTimeout != httpx.DefaultTimeout {
		t.Fatalf("timeouts = header %v, read %v, write %v, idle %v; want all %v",
			srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout, httpx.DefaultTimeout)
	}
	if httpx.DefaultTimeout != 10*time.Second {
		t.Fatalf("DefaultTimeout = %v, want the 10s both tools serve with", httpx.DefaultTimeout)
	}
}
