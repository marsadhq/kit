package notify

// Internal test, like pushover_test.go: it points the pushoverAPIURL seam at
// an httptest server.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// pushoverCall is one request the fake Pushover API saw.
type pushoverCall struct {
	method, path string
	form         url.Values
}

// pushoverAPI is a fake Pushover API: every request is recorded, and
// answer gives the status, headers and body for it.
func pushoverAPI(t *testing.T, answer func(path string) (int, http.Header, string)) *[]pushoverCall {
	t.Helper()
	var calls []pushoverCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		v, _ := url.ParseQuery(string(b))
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("%s %s: Content-Type %q, want a form", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		calls = append(calls, pushoverCall{r.Method, r.URL.Path, v})
		code, h, body := answer(r.URL.Path)
		for k, vs := range h {
			w.Header()[k] = vs
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	old := pushoverAPIURL
	pushoverAPIURL = srv.URL + "/1/messages.json"
	t.Cleanup(func() { pushoverAPIURL = old; srv.Close() })
	return &calls
}

const testReceipt = "rLqVuqTRh62UzxtmqiaLzQmVcPgiCy"

// TestPushoverSendReceipt: an emergency message's answer carries the
// receipt, which SendReceipt reports; any other message has none. The
// request is the one Send makes.
func TestPushoverSendReceipt(t *testing.T) {
	calls := pushoverAPI(t, func(string) (int, http.Header, string) {
		return 200, nil, `{"status":1,"request":"r","receipt":"` + testReceipt + `"}`
	})
	p := NewPushover(PushoverConfig{Token: "app-token", User: "user-key"})
	m := Message{Subject: "Disk full", Body: "db-1", Priority: PriorityUrgent}
	receipt, err := p.SendReceipt(context.Background(), m)
	if err != nil || receipt != testReceipt {
		t.Fatalf("SendReceipt = %q, %v; want the receipt", receipt, err)
	}
	if err := p.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if a, b := (*calls)[0], (*calls)[1]; a.path != "/1/messages.json" || a.form.Encode() != b.form.Encode() {
		t.Fatalf("SendReceipt sent %s %v, Send %v", a.path, a.form, b.form)
	}

	// Pushover gives no receipt below emergency priority.
	calls2 := pushoverAPI(t, func(string) (int, http.Header, string) { return 200, nil, pushoverOK })
	receipt, err = p.SendReceipt(context.Background(), Message{Subject: "s", Body: "b", Priority: PriorityLow})
	if err != nil || receipt != "" || len(*calls2) != 1 {
		t.Fatalf("quiet SendReceipt = %q, %v (%d calls)", receipt, err, len(*calls2))
	}

	// A receipt that is not one (it would go into a URL path) is dropped.
	pushoverAPI(t, func(string) (int, http.Header, string) {
		return 200, nil, `{"status":1,"request":"r","receipt":"../../1/x"}`
	})
	if receipt, err := p.SendReceipt(context.Background(), m); err != nil || receipt != "" {
		t.Fatalf("odd receipt = %q, %v; want none and the send accepted", receipt, err)
	}
}

// TestPushoverCancelReceipt: the cancel is a form POST of the token alone to
// receipts/{receipt}/cancel.json.
func TestPushoverCancelReceipt(t *testing.T) {
	calls := pushoverAPI(t, func(string) (int, http.Header, string) { return 200, nil, `{"status":1,"request":"r"}` })
	p := NewPushover(PushoverConfig{Token: "app-token", User: "user-key", Device: "phone"})
	if err := p.CancelReceipt(context.Background(), testReceipt); err != nil {
		t.Fatalf("CancelReceipt: %v", err)
	}
	c := (*calls)[0]
	if c.method != http.MethodPost || c.path != "/1/receipts/"+testReceipt+"/cancel.json" {
		t.Fatalf("cancel = %s %s", c.method, c.path)
	}
	if want := (url.Values{"token": {"app-token"}}); c.form.Encode() != want.Encode() {
		t.Fatalf("cancel form = %v, want the token alone", c.form)
	}
}

// TestPushoverCancelOutcomes: a 4xx (an expired or unknown receipt, a bad
// token) is permanent, as Pushover asks never to repeat one; a 429 asks to
// wait (its Retry-After, else a minute) and is not permanent; anything else
// asks for Pushover's five seconds. No error carries the token.
func TestPushoverCancelOutcomes(t *testing.T) {
	const token = "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
	for _, c := range []struct {
		name      string
		status    int
		header    http.Header
		body      string
		permanent bool
		wait      time.Duration
		code      int
		mention   string
	}{
		{"expired", 400, nil, `{"receipt":"not found","errors":["receipt not found; may be invalid or expired"],"status":0}`, true, 0, 400, "receipt not found"},
		{"bad token", 400, nil, `{"token":"invalid","errors":["application token ` + token + ` is invalid"],"status":0}`, true, 0, 400, "application token *** is invalid"},
		{"rate limited", 429, http.Header{"Retry-After": {"120"}}, `{"status":0}`, false, 2 * time.Minute, 429, "status 429"},
		{"rate limited, no header", 429, nil, `{"status":0}`, false, time.Minute, 429, "status 429"},
		{"server error", 503, nil, `oops`, false, 5 * time.Second, 503, "status 503"},
		{"odd answer", 200, nil, `{"status":0}`, false, 5 * time.Second, 200, "without the accepted answer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pushoverAPI(t, func(string) (int, http.Header, string) { return c.status, c.header, c.body })
			err := NewPushover(PushoverConfig{Token: token, User: "uQiRzpo4DXghDmr9QzzfQu27cmVRsG"}).CancelReceipt(context.Background(), testReceipt)
			if err == nil {
				t.Fatal("err = nil")
			}
			if IsPermanent(err) != c.permanent {
				t.Errorf("IsPermanent = %v, want %v (%v)", IsPermanent(err), c.permanent, err)
			}
			if after, ok := RetryAfter(err); !c.permanent && (!ok || after != c.wait) {
				t.Errorf("RetryAfter = %v, %v; want %v", after, ok, c.wait)
			}
			if code, ok := StatusCode(err); !ok || code != c.code {
				t.Errorf("StatusCode = %d, %v; want %d", code, ok, c.code)
			}
			if !strings.Contains(err.Error(), c.mention) || strings.Contains(err.Error(), token) {
				t.Errorf("error %q: want %q and no token", err, c.mention)
			}
		})
	}

	// A transport failure is retryable after five seconds, without the token.
	old := pushoverAPIURL
	defer func() { pushoverAPIURL = old }()
	pushoverAPIURL = "http://127.0.0.1:1/1/messages.json"
	err := NewPushover(PushoverConfig{Token: token, User: "uQiRzpo4DXghDmr9QzzfQu27cmVRsG"}).CancelReceipt(context.Background(), testReceipt)
	if err == nil || IsPermanent(err) || strings.Contains(err.Error(), token) {
		t.Fatalf("transport error = %v", err)
	}
	if after, ok := RetryAfter(err); !ok || after != 5*time.Second {
		t.Errorf("transport RetryAfter = %v, %v", after, ok)
	}
}

// TestPushoverCancelRefusesABadReceipt: a receipt that is not one is refused
// before any request (it would go into the URL path).
func TestPushoverCancelRefusesABadReceipt(t *testing.T) {
	calls := pushoverAPI(t, func(string) (int, http.Header, string) { return 200, nil, `{"status":1}` })
	for _, r := range []string{"", "../messages", "abc/def", strings.Repeat("a", 65), "a b"} {
		if err := NewPushover(PushoverConfig{Token: "t", User: "u"}).CancelReceipt(context.Background(), r); err == nil || !IsPermanent(err) {
			t.Errorf("receipt %q: err = %v, want permanent", r, err)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("%d requests for bad receipts", len(*calls))
	}
}

// TestPushoverTags: an emergency message's tags are sent as Pushover's
// comma-separated tags; without tags the request is what it always was;
// below emergency priority Pushover takes no tags and none are sent.
func TestPushoverTags(t *testing.T) {
	calls := pushoverAPI(t, func(string) (int, http.Header, string) {
		return 200, nil, `{"status":1,"request":"r","receipt":"` + testReceipt + `"}`
	})
	p := NewPushover(PushoverConfig{Token: "app-token", User: "user-key"})
	m := Message{Subject: "Disk full", Body: "db-1", Priority: PriorityUrgent}
	if err := p.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	m.PushoverTags = []string{"ward-12-abc", "team=ops"}
	if err := p.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	plain, tagged := (*calls)[0].form, (*calls)[1].form
	if plain.Has("tags") || tagged.Get("tags") != "ward-12-abc,team=ops" {
		t.Fatalf("tags: without %q, with %q", plain.Get("tags"), tagged.Get("tags"))
	}
	tagged.Del("tags")
	if tagged.Encode() != plain.Encode() {
		t.Fatalf("tags changed the rest of the request: %v vs %v", tagged, plain)
	}
	m.Priority = PriorityHigh
	if err := p.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if (*calls)[2].form.Has("tags") {
		t.Fatal("tags sent below emergency priority")
	}
}

// TestPushoverRefusesBadTags: a tag Pushover would split or that cannot go
// into a URL path is refused before anything is sent.
func TestPushoverRefusesBadTags(t *testing.T) {
	calls := pushoverAPI(t, func(string) (int, http.Header, string) { return 200, nil, `{"status":1}` })
	p := NewPushover(PushoverConfig{Token: "t", User: "u"})
	for _, tag := range []string{"", "a,b", "a/b", "a b", "../x", strings.Repeat("a", 65)} {
		err := p.Send(context.Background(), Message{Subject: "s", Priority: PriorityUrgent, PushoverTags: []string{tag}})
		if err == nil || !IsPermanent(err) {
			t.Errorf("tag %q: Send = %v, want permanent", tag, err)
		}
		if err := p.CancelByTag(context.Background(), tag); err == nil || !IsPermanent(err) {
			t.Errorf("tag %q: CancelByTag = %v, want permanent", tag, err)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("%d requests for bad tags", len(*calls))
	}
}

// TestPushoverCancelByTag: a form POST of the token alone to
// receipts/cancel_by_tag/{tag}.json, which stops every emergency message
// of the application with that tag; its answers are classified as
// CancelReceipt's are.
func TestPushoverCancelByTag(t *testing.T) {
	calls := pushoverAPI(t, func(string) (int, http.Header, string) { return 200, nil, `{"status":1,"canceled":2,"request":"r"}` })
	p := NewPushover(PushoverConfig{Token: "app-token", User: "user-key"})
	if err := p.CancelByTag(context.Background(), "ward-12-abc"); err != nil {
		t.Fatalf("CancelByTag: %v", err)
	}
	c := (*calls)[0]
	if c.method != http.MethodPost || c.path != "/1/receipts/cancel_by_tag/ward-12-abc.json" || c.form.Encode() != (url.Values{"token": {"app-token"}}).Encode() {
		t.Fatalf("cancel by tag = %s %s %v", c.method, c.path, c.form)
	}
	for _, x := range []struct {
		status    int
		header    http.Header
		permanent bool
		wait      time.Duration
	}{
		{400, nil, true, 0},
		{429, http.Header{"Retry-After": {"30"}}, false, 30 * time.Second},
		{502, nil, false, 5 * time.Second},
	} {
		pushoverAPI(t, func(string) (int, http.Header, string) {
			return x.status, x.header, `{"status":0,"errors":["app-token odd"]}`
		})
		err := p.CancelByTag(context.Background(), "ward-12-abc")
		if err == nil || IsPermanent(err) != x.permanent || strings.Contains(err.Error(), "app-token") {
			t.Errorf("status %d: err = %v", x.status, err)
			continue
		}
		if after, ok := RetryAfter(err); !x.permanent && (!ok || after != x.wait) {
			t.Errorf("status %d: RetryAfter = %v, %v; want %v", x.status, after, ok, x.wait)
		}
		if code, ok := StatusCode(err); !ok || code != x.status {
			t.Errorf("status %d: StatusCode = %d, %v", x.status, code, ok)
		}
	}
}
