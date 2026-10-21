package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHTTPStatusErrorClassification pins which HTTP answers end a delivery at
// once: every 4xx except 408 and 429; never a 5xx.
func TestHTTPStatusErrorClassification(t *testing.T) {
	for code, permanent := range map[int]bool{
		400: true, 401: true, 403: true, 404: true, 410: true, 413: true, 422: true,
		408: false, 429: false, 500: false, 502: false, 503: false, 504: false, 302: false,
	} {
		if got := IsPermanent(httpStatusError("x", code)); got != permanent {
			t.Errorf("status %d: IsPermanent = %v, want %v", code, got, permanent)
		}
	}
	if IsPermanent(errors.New("plain")) || IsPermanent(nil) || Permanent(nil) != nil {
		t.Error("only errors marked by Permanent are permanent")
	}
}

// TestProvidersReportRejectionsAsPermanent drives every HTTP provider against
// a receiver that answers 400 (Ward's answer to a body it cannot read) and one
// that answers 503: the first is permanent, the second retryable.
func TestProvidersReportRejectionsAsPermanent(t *testing.T) {
	providers := func(url string) map[string]Provider {
		return map[string]Provider{
			"webhook":  NewWebhook(url),
			"slack":    NewSlack(url),
			"discord":  NewDiscord(url),
			"telegram": NewTelegram("tok", "1"),
		}
	}
	old := telegramAPIBase
	defer func() { telegramAPIBase = old }()
	for code, permanent := range map[int]bool{http.StatusBadRequest: true, http.StatusServiceUnavailable: false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		telegramAPIBase = srv.URL
		for name, p := range providers(srv.URL) {
			err := p.Send(context.Background(), Message{Subject: "s", Body: "b"})
			if err == nil {
				t.Errorf("%s on %d: err = nil", name, code)
				continue
			}
			if got := IsPermanent(err); got != permanent {
				t.Errorf("%s on %d: IsPermanent = %v, want %v (%v)", name, code, got, permanent, err)
			}
		}
		srv.Close()
	}
}

// TestStatusCode: the HTTP status of a refused request is readable through
// any wrapping, the error text stays what it was, and an error that is no
// HTTP answer (a transport failure) has none.
func TestStatusCode(t *testing.T) {
	for _, code := range []int{400, 404, 408, 429, 500, 503} {
		err := httpStatusError("ntfy publish ntfy.test", code)
		wrapped := RetryLater(errors.Join(errors.New("context"), err), 0)
		for _, e := range []error{err, wrapped} {
			if got, ok := StatusCode(e); !ok || got != code {
				t.Errorf("StatusCode(%v) = %d, %v; want %d", e, got, ok, code)
			}
		}
		if want := "ntfy publish ntfy.test: status " + itoa(code); err.Error() != want {
			t.Errorf("error text = %q, want %q", err, want)
		}
	}
	if code, ok := StatusCode(errors.New("dial tcp: connection refused")); ok || code != 0 {
		t.Errorf("StatusCode of a transport error = %d, %v", code, ok)
	}
	if _, ok := StatusCode(nil); ok {
		t.Error("StatusCode(nil) reported a status")
	}
}

func itoa(n int) string { return fmt.Sprint(n) }
