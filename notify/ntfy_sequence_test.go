package notify_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marsadhq/kit/notify"
)

// TestNtfySequenceID: a message with a sequence id is published under it,
// so a later one with the same id replaces it on the phone; without one the
// body is exactly what it always was.
func TestNtfySequenceID(t *testing.T) {
	srv, got := ntfyServer(t, 200, `{"id":"x","sequence_id":"ward-12-abc"}`)
	p := notify.NewNtfy(notify.NtfyConfig{URL: srv.URL + "/ward-alerts"})
	if err := p.Send(context.Background(), notify.Message{Subject: "Disk full", Body: "db-1", NtfySequenceID: "ward-12-abc"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if want := `{"topic":"ward-alerts","message":"db-1","title":"Disk full","sequence_id":"ward-12-abc"}`; (*got)[0].raw != want {
		t.Errorf("body = %s, want %s", (*got)[0].raw, want)
	}
	if err := p.Send(context.Background(), notify.Message{Subject: "Disk full", Body: "db-1"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if want := `{"topic":"ward-alerts","message":"db-1","title":"Disk full"}`; (*got)[1].raw != want {
		t.Errorf("body without a sequence id = %s, want %s", (*got)[1].raw, want)
	}
}

// TestNtfyRefusesABadSequenceID: ntfy takes 1 to 64 of [-_A-Za-z0-9]; any
// other id is permanent before anything is sent.
func TestNtfyRefusesABadSequenceID(t *testing.T) {
	srv, got := ntfyServer(t, 200, `{}`)
	p := notify.NewNtfy(notify.NtfyConfig{URL: srv.URL + "/t"})
	for _, id := range []string{"a/b", "a b", strings.Repeat("a", 65), "é"} {
		if err := p.Send(context.Background(), notify.Message{Subject: "s", NtfySequenceID: id}); err == nil || !notify.IsPermanent(err) {
			t.Errorf("sequence id %q: err = %v, want permanent", id, err)
		}
		if err := p.Clear(context.Background(), id); err == nil || !notify.IsPermanent(err) {
			t.Errorf("Clear(%q): err = %v, want permanent", id, err)
		}
	}
	if err := p.Clear(context.Background(), ""); err == nil || !notify.IsPermanent(err) {
		t.Errorf("Clear(\"\"): err = %v, want permanent", err)
	}
	if len(*got) != 0 {
		t.Fatalf("%d requests for bad sequence ids", len(*got))
	}
}

// TestNtfyClear: Clear is a PUT to <topic>/<sequence id>/clear on the
// topic's server (behind its path prefix, its query kept), with the token,
// which dismisses the notification on the phone.
func TestNtfyClear(t *testing.T) {
	srv, got := ntfyServer(t, 200, `{"id":"y","event":"message_clear"}`)
	p := notify.NewNtfy(notify.NtfyConfig{URL: srv.URL + "/ntfy/alerts?auth=QmFzaWM", Token: "tk_secret"})
	if err := p.Clear(context.Background(), "ward-12-abc"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	r := (*got)[0]
	if r.method != http.MethodPut || r.path != "/ntfy/alerts/ward-12-abc/clear" || r.query != "auth=QmFzaWM" || r.auth != "Bearer tk_secret" || r.raw != "" {
		t.Fatalf("clear = %s %s?%s auth %q body %q", r.method, r.path, r.query, r.auth, r.raw)
	}
}

// TestNtfyClearErrors: Clear's answers are classified as a publish's are
// (an old server without sequence ids answers 404: permanent; a 429 is
// not), StatusCode reads them, and no error names the topic or the token.
func TestNtfyClearErrors(t *testing.T) {
	const topic, token = "SECRETTOPIC", "tk_SECRETTOKEN"
	for _, c := range []struct {
		status    int
		resp      string
		permanent bool
	}{
		{404, `{"code":40401,"http":404,"error":"page not found"}`, true},
		{403, `{"code":40301,"http":403,"error":"forbidden for SECRETTOPIC"}`, true},
		{429, `{"code":42908,"http":429,"error":"limit reached: daily message quota reached"}`, false},
		{502, `bad gateway`, false},
	} {
		srv, _ := ntfyServer(t, c.status, c.resp)
		err := notify.NewNtfy(notify.NtfyConfig{URL: srv.URL + "/" + topic, Token: token}).Clear(context.Background(), "ward-1-a")
		if err == nil {
			t.Fatalf("status %d: err = nil", c.status)
		}
		if notify.IsPermanent(err) != c.permanent {
			t.Errorf("status %d: IsPermanent = %v, want %v (%v)", c.status, notify.IsPermanent(err), c.permanent, err)
		}
		if code, ok := notify.StatusCode(err); !ok || code != c.status {
			t.Errorf("status %d: StatusCode = %d, %v", c.status, code, ok)
		}
		if strings.Contains(err.Error(), topic) || strings.Contains(err.Error(), token) {
			t.Errorf("status %d: error leaks a secret: %v", c.status, err)
		}
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	closed := srv.URL
	srv.Close()
	err := notify.NewNtfy(notify.NtfyConfig{URL: closed + "/" + topic, Token: token}).Clear(context.Background(), "ward-1-a")
	if err == nil || notify.IsPermanent(err) || strings.Contains(err.Error(), topic) {
		t.Errorf("transport error = %v, want retryable without the topic", err)
	}
}
