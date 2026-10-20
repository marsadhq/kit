package notify

// Internal test: it points the unexported pushoverAPIURL seam at an httptest
// server, so these tests must not run in parallel with each other.

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

// pushoverServer is a fake messages API answering status with body, recording
// each request's form.
func pushoverServer(t *testing.T, status int, body string) *[]url.Values {
	t.Helper()
	var forms []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("request = %s with %q, want a form POST", r.Method, r.Header.Get("Content-Type"))
		}
		b, _ := io.ReadAll(r.Body)
		v, err := url.ParseQuery(string(b))
		if err != nil {
			t.Errorf("body is not a form: %v", err)
		}
		forms = append(forms, v)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	old := pushoverAPIURL
	pushoverAPIURL = srv.URL + "/1/messages.json"
	t.Cleanup(func() { pushoverAPIURL = old; srv.Close() })
	return &forms
}

const pushoverOK = `{"status":1,"request":"647d2300-702c-4b38-8b2f-d56326ae460b"}`

func TestPushoverSendsTheMessage(t *testing.T) {
	forms := pushoverServer(t, 200, pushoverOK)
	p := NewPushover(PushoverConfig{Token: "azGDORePK8gMaC0QOYAMyEEuzJnyUi", User: "uQiRzpo4DXghDmr9QzzfQu27cmVRsG", Device: "phone", Sound: "siren"})
	if err := p.Send(context.Background(), Message{Subject: "Disk full", Body: "db-1 at 98%"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	want := url.Values{
		"token": {"azGDORePK8gMaC0QOYAMyEEuzJnyUi"}, "user": {"uQiRzpo4DXghDmr9QzzfQu27cmVRsG"},
		"title": {"Disk full"}, "message": {"db-1 at 98%"}, "device": {"phone"}, "sound": {"siren"},
	}
	if got := (*forms)[0]; got.Encode() != want.Encode() {
		t.Fatalf("form = %v, want %v", got, want)
	}
}

// TestPushoverSubjectOnly: with no body the subject is the message, and
// Pushover shows the app name as the title.
func TestPushoverSubjectOnly(t *testing.T) {
	forms := pushoverServer(t, 200, pushoverOK)
	if err := NewPushover(PushoverConfig{Token: "t", User: "u"}).Send(context.Background(), Message{Subject: "Backup done"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if f := (*forms)[0]; f.Get("message") != "Backup done" || f.Has("title") || f.Has("priority") {
		t.Fatalf("form = %v", f)
	}
}

// TestPushoverPriority: the message's priority maps onto Pushover's scale and
// wins over the channel's; normal priority is sent as no priority.
func TestPushoverPriority(t *testing.T) {
	forms := pushoverServer(t, 200, pushoverOK)
	for _, c := range []struct {
		channel int
		message Priority
		want    string
	}{
		{0, PriorityUnset, ""},
		{1, PriorityUnset, "1"},
		{-1, PriorityUnset, "-1"},
		{1, PriorityMin, "-2"},
		{1, PriorityLow, "-1"},
		{1, PriorityNormal, ""},
		{0, PriorityHigh, "1"},
	} {
		*forms = nil
		if err := NewPushover(PushoverConfig{Token: "t", User: "u", Priority: c.channel}).Send(context.Background(),
			Message{Subject: "s", Body: "b", Priority: c.message}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if got := (*forms)[0].Get("priority"); got != c.want {
			t.Errorf("channel %d, message %d: priority %q, want %q", c.channel, c.message, got, c.want)
		}
		if (*forms)[0].Has("retry") || (*forms)[0].Has("expire") {
			t.Errorf("channel %d, message %d: retry/expire below emergency", c.channel, c.message)
		}
	}
}

// TestPushoverEmergency: an urgent message is emergency priority with retry
// and expire (the defaults, or the channel's), and the Acknowledge endpoint
// as the callback Pushover posts to once acknowledged.
func TestPushoverEmergency(t *testing.T) {
	forms := pushoverServer(t, 200, `{"status":1,"request":"r","receipt":"rLqVuqTRh62UzxtmqiaLzQmVcPgiCy"}`)
	m := Message{Subject: "Disk full", Body: "db-1", Priority: PriorityUrgent, Actions: []Action{
		CallbackAction("Bot only", "ack:12"),
		HTTPAction("Acknowledge", "POST", "https://ward.example/hooks/pushover/ack?incident=12"),
		LinkAction("View", "https://ward.example/incidents/12"),
	}}
	if err := NewPushover(PushoverConfig{Token: "t", User: "u"}).Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	f := (*forms)[0]
	if f.Get("priority") != "2" || f.Get("retry") != "60" || f.Get("expire") != "3600" {
		t.Errorf("emergency = priority %q retry %q expire %q, want 2/60/3600", f.Get("priority"), f.Get("retry"), f.Get("expire"))
	}
	if f.Get("callback") != "https://ward.example/hooks/pushover/ack?incident=12" {
		t.Errorf("callback = %q", f.Get("callback"))
	}
	if f.Get("url") != "https://ward.example/incidents/12" || f.Get("url_title") != "View" {
		t.Errorf("url = %q (%q)", f.Get("url"), f.Get("url_title"))
	}

	*forms = nil
	if err := NewPushover(PushoverConfig{Token: "t", User: "u", Priority: 2, Retry: 30, Expire: 10800}).Send(context.Background(),
		Message{Subject: "s", Body: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if f := (*forms)[0]; f.Get("priority") != "2" || f.Get("retry") != "30" || f.Get("expire") != "10800" || f.Has("callback") {
		t.Errorf("configured emergency = %v", f)
	}

	// Below emergency there is no acknowledgement, so no callback.
	*forms = nil
	m.Priority = PriorityHigh
	if err := NewPushover(PushoverConfig{Token: "t", User: "u"}).Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if (*forms)[0].Has("callback") {
		t.Error("callback sent below emergency priority")
	}
}

// TestPushoverLimits: text is cut to Pushover's limits, and a supplementary
// URL too long to send is left out rather than broken.
func TestPushoverLimits(t *testing.T) {
	forms := pushoverServer(t, 200, pushoverOK)
	m := Message{
		Subject: strings.Repeat("t", 300),
		Body:    strings.Repeat("é", 1100),
		Actions: []Action{LinkAction(strings.Repeat("l", 120), "https://x.example/"+strings.Repeat("p", 600))},
	}
	if err := NewPushover(PushoverConfig{Token: "t", User: "u"}).Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	f := (*forms)[0]
	if n := len([]rune(f.Get("message"))); n != 1024 || !strings.HasSuffix(f.Get("message"), "…") {
		t.Errorf("message is %d characters, want 1024 ending in an ellipsis", n)
	}
	if n := len([]rune(f.Get("title"))); n != 250 {
		t.Errorf("title is %d characters, want 250", n)
	}
	if f.Has("url") || f.Has("url_title") {
		t.Errorf("an over-long URL was sent: %q", f.Get("url"))
	}

	*forms = nil
	m.Actions = []Action{LinkAction(strings.Repeat("l", 120), "https://x.example/ok")}
	if err := NewPushover(PushoverConfig{Token: "t", User: "u"}).Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if f := (*forms)[0]; f.Get("url") != "https://x.example/ok" || len([]rune(f.Get("url_title"))) != 100 {
		t.Errorf("url %q, url_title %d characters; want the URL and 100", f.Get("url"), len([]rune(f.Get("url_title"))))
	}
}

// TestPushoverErrors: every 4xx is permanent, as Pushover asks (an exhausted
// quota included); anything else asks the worker to wait five seconds. The
// error keeps Pushover's explanation but never the token or user key.
func TestPushoverErrors(t *testing.T) {
	const token, user = "azGDORePK8gMaC0QOYAMyEEuzJnyUi", "uQiRzpo4DXghDmr9QzzfQu27cmVRsG"
	for _, c := range []struct {
		status    int
		body      string
		permanent bool
		mention   string
	}{
		{400, `{"token":"invalid","errors":["application token is invalid"],"status":0,"request":"r"}`, true, "application token is invalid"},
		{400, `{"user":"invalid","errors":["user key ` + user + ` is invalid"],"status":0}`, true, "user key *** is invalid"},
		{429, `{"errors":["message limit reached"],"status":0}`, true, "message limit reached"},
		{500, `oops`, false, "status 500"},
		{200, `{"status":0,"errors":["odd"]}`, false, "without the accepted answer"},
		{200, `not json`, false, "without the accepted answer"},
	} {
		pushoverServer(t, c.status, c.body)
		err := NewPushover(PushoverConfig{Token: token, User: user}).Send(context.Background(), Message{Subject: "s", Body: "b"})
		if err == nil {
			t.Fatalf("status %d %s: err = nil", c.status, c.body)
		}
		if IsPermanent(err) != c.permanent {
			t.Errorf("status %d: IsPermanent = %v, want %v (%v)", c.status, IsPermanent(err), c.permanent, err)
		}
		if after, ok := RetryAfter(err); !c.permanent && (!ok || after != 5*time.Second) {
			t.Errorf("status %d: RetryAfter = %v, %v; want 5s", c.status, after, ok)
		}
		if !strings.Contains(err.Error(), c.mention) {
			t.Errorf("status %d: error %q does not mention %q", c.status, err, c.mention)
		}
		if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), user) {
			t.Errorf("status %d: error leaks a credential: %v", c.status, err)
		}
	}

	// A transport failure is retryable after the wait, without credentials.
	old := pushoverAPIURL
	defer func() { pushoverAPIURL = old }()
	pushoverAPIURL = "http://127.0.0.1:1/1/messages.json"
	err := NewPushover(PushoverConfig{Token: token, User: user}).Send(context.Background(), Message{Subject: "s"})
	if err == nil || IsPermanent(err) || strings.Contains(err.Error(), token) {
		t.Fatalf("transport error = %v, want retryable without the token", err)
	}
	if after, ok := RetryAfter(err); !ok || after != 5*time.Second {
		t.Errorf("transport error RetryAfter = %v, %v; want 5s", after, ok)
	}
}
