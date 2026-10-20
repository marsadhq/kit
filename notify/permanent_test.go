package notify

import (
	"context"
	"errors"
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
