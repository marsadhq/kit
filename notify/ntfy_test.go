package notify_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marsadhq/kit/notify"
)

// ntfyRequest is what a fake ntfy server saw.
type ntfyRequest struct {
	method, path, query, auth, contentType string
	body                                   map[string]any
	raw                                    string
}

// ntfyServer is a fake ntfy server answering status with respBody.
func ntfyServer(t *testing.T, status int, respBody string) (*httptest.Server, *[]ntfyRequest) {
	t.Helper()
	var got []ntfyRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		req := ntfyRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type"), raw: string(b)}
		_ = json.Unmarshal(b, &req.body)
		got = append(got, req)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// TestNtfyPublishesJSON: the message is published as JSON to the server's
// root naming the topic, with title, message, priority, tags and the token.
func TestNtfyPublishesJSON(t *testing.T) {
	srv, got := ntfyServer(t, 200, `{"id":"x"}`)
	p := notify.NewNtfy(notify.NtfyConfig{URL: srv.URL + "/ward-alerts", Token: "tk_secret", Priority: 4, Tags: []string{"rotating_light"}})
	if err := p.Send(context.Background(), notify.Message{Subject: "Disk full", Body: "db-1 at 98%"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	r := (*got)[0]
	if r.method != http.MethodPost || r.path != "/" || r.contentType != "application/json" {
		t.Errorf("request = %s %s (%s), want POST / as JSON", r.method, r.path, r.contentType)
	}
	if r.auth != "Bearer tk_secret" {
		t.Errorf("Authorization = %q, want the Bearer token", r.auth)
	}
	const want = `{"topic":"ward-alerts","message":"db-1 at 98%","title":"Disk full","priority":4,"tags":["rotating_light"]}`
	if r.raw != want {
		t.Errorf("body = %s, want %s", r.raw, want)
	}
}

// TestNtfyMinimal: no token, priority or tags sends none of them, and a
// message with no body is sent as the message text without a title.
func TestNtfyMinimal(t *testing.T) {
	srv, got := ntfyServer(t, 200, `{}`)
	if err := notify.NewNtfy(notify.NtfyConfig{URL: srv.URL + "/t"}).Send(context.Background(), notify.Message{Subject: "Backup done"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	r := (*got)[0]
	if r.auth != "" {
		t.Errorf("Authorization = %q without a token", r.auth)
	}
	if want := `{"topic":"t","message":"Backup done"}`; r.raw != want {
		t.Errorf("body = %s, want %s", r.raw, want)
	}
}

// TestNtfyPriority: the message's priority wins over the channel's, which
// wins over the server default.
func TestNtfyPriority(t *testing.T) {
	srv, got := ntfyServer(t, 200, `{}`)
	for _, c := range []struct {
		channel int
		message notify.Priority
		want    any
	}{
		{0, notify.PriorityUnset, nil},
		{2, notify.PriorityUnset, 2.0},
		{2, notify.PriorityUrgent, 5.0},
		{0, notify.PriorityMin, 1.0},
		{0, notify.PriorityHigh, 4.0},
	} {
		*got = nil
		p := notify.NewNtfy(notify.NtfyConfig{URL: srv.URL + "/t", Priority: c.channel})
		if err := p.Send(context.Background(), notify.Message{Subject: "s", Body: "b", Priority: c.message}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if pr := (*got)[0].body["priority"]; pr != c.want {
			t.Errorf("channel %d, message %d: priority = %v, want %v", c.channel, c.message, pr, c.want)
		}
	}
}

// TestNtfyActions: an Acknowledge HTTP action and a View link become ntfy's
// http and view buttons, in order; callbacks are left out and at most three
// buttons are sent.
func TestNtfyActions(t *testing.T) {
	srv, got := ntfyServer(t, 200, `{}`)
	ack := notify.HTTPAction("Acknowledge", "POST", "https://ward.example/api/incidents/12/ack")
	ack.Headers = map[string]string{"Authorization": "Bearer ward_x"}
	ack.Body = `{"via":"ntfy"}`
	ack.Clear = true
	m := notify.Message{Subject: "Disk full", Body: "db-1", Actions: []notify.Action{
		notify.CallbackAction("Bot only", "ack:12"),
		ack,
		notify.LinkAction("View", "https://ward.example/incidents/12"),
		notify.LinkAction("Runbook", "https://wiki.example/disk"),
		notify.LinkAction("Fourth", "https://example.com/4"),
	}}
	if err := notify.NewNtfy(notify.NtfyConfig{URL: srv.URL + "/t"}).Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body struct {
		Actions []map[string]any `json:"actions"`
	}
	if err := json.Unmarshal([]byte((*got)[0].raw), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(body.Actions) != 3 {
		t.Fatalf("actions = %v, want 3", body.Actions)
	}
	a := body.Actions[0]
	if a["action"] != "http" || a["label"] != "Acknowledge" || a["url"] != "https://ward.example/api/incidents/12/ack" ||
		a["method"] != "POST" || a["body"] != `{"via":"ntfy"}` || a["clear"] != true ||
		a["headers"].(map[string]any)["Authorization"] != "Bearer ward_x" {
		t.Errorf("http action = %v", a)
	}
	if v := body.Actions[1]; v["action"] != "view" || v["label"] != "View" || v["url"] != "https://ward.example/incidents/12" || v["clear"] != nil {
		t.Errorf("view action = %v", v)
	}
	if body.Actions[2]["label"] != "Runbook" {
		t.Errorf("third action = %v, want Runbook", body.Actions[2])
	}
}

// TestNtfyServerPrefixAndQuery: a topic on a server behind a path prefix is
// published to that prefix, and a query (ntfy's ?auth=) is kept.
func TestNtfyServerPrefixAndQuery(t *testing.T) {
	srv, got := ntfyServer(t, 200, `{}`)
	p := notify.NewNtfy(notify.NtfyConfig{URL: srv.URL + "/ntfy/alerts/?auth=QmFzaWM"})
	if err := p.Send(context.Background(), notify.Message{Subject: "s", Body: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	r := (*got)[0]
	if r.path != "/ntfy/" || r.query != "auth=QmFzaWM" || r.body["topic"] != "alerts" {
		t.Errorf("request = %s?%s topic %v, want /ntfy/?auth=QmFzaWM topic alerts", r.path, r.query, r.body["topic"])
	}
}

// TestNtfyErrors: a refusal is permanent unless it asks for a retry, ntfy's
// own explanation is kept, and no error names the topic or the token.
func TestNtfyErrors(t *testing.T) {
	const topic, token = "SECRETTOPIC", "tk_SECRETTOKEN"
	for _, c := range []struct {
		status    int
		resp      string
		permanent bool
		mention   string
	}{
		{403, `{"code":40301,"http":403,"error":"forbidden for SECRETTOPIC"}`, true, "forbidden for ***"},
		{400, `{"code":40007,"http":400,"error":"invalid request: actions invalid"}`, true, "actions invalid"},
		{429, `{"code":42901,"http":429,"error":"limit reached: too many requests"}`, false, "too many requests"},
		{503, `not json`, false, "status 503"},
	} {
		srv, _ := ntfyServer(t, c.status, c.resp)
		err := notify.NewNtfy(notify.NtfyConfig{URL: srv.URL + "/" + topic, Token: token}).Send(context.Background(), notify.Message{Subject: "s", Body: "b"})
		if err == nil {
			t.Fatalf("status %d: err = nil", c.status)
		}
		if notify.IsPermanent(err) != c.permanent {
			t.Errorf("status %d: IsPermanent = %v, want %v (%v)", c.status, notify.IsPermanent(err), c.permanent, err)
		}
		if !strings.Contains(err.Error(), c.mention) {
			t.Errorf("status %d: error %q does not mention %q", c.status, err, c.mention)
		}
		if strings.Contains(err.Error(), topic) || strings.Contains(err.Error(), token) {
			t.Errorf("status %d: error leaks a secret: %v", c.status, err)
		}
	}

	// A transport failure: the URL net/http would quote holds the topic.
	srv := httptest.NewServer(http.NotFoundHandler())
	closed := srv.URL
	srv.Close()
	err := notify.NewNtfy(notify.NtfyConfig{URL: closed + "/" + topic, Token: token}).Send(context.Background(), notify.Message{Subject: "s"})
	if err == nil || strings.Contains(err.Error(), topic) || notify.IsPermanent(err) {
		t.Errorf("transport error = %v, want a retryable error without the topic", err)
	}
}

// TestNtfyBadConfigNeverLeaks: a topic URL that cannot be published to fails
// permanently, and the error quotes no part of it.
func TestNtfyBadConfigNeverLeaks(t *testing.T) {
	for _, u := range []string{
		"https://ntfy.sh/SECRET\x7fTOPIC",
		"ftp://ntfy.sh/SECRETTOPIC",
		"https://ntfy.sh/",
		"https://ntfy.sh/SECRET.TOPIC",
		"ntfy.sh/SECRETTOPIC",
	} {
		err := notify.NewNtfy(notify.NtfyConfig{URL: u}).Send(context.Background(), notify.Message{Subject: "s"})
		if err == nil || !notify.IsPermanent(err) {
			t.Errorf("%q: err = %v, want permanent", u, err)
			continue
		}
		if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "TOPIC") {
			t.Errorf("%q: error leaks the topic: %v", u, err)
		}
	}
}

// TestNtfyRefusesBadActions: a button with no label or URL is permanent.
func TestNtfyRefusesBadActions(t *testing.T) {
	srv, got := ntfyServer(t, 200, `{}`)
	for _, a := range []notify.Action{notify.LinkAction("", "https://x.example"), notify.HTTPAction("Ack", "POST", "")} {
		err := notify.NewNtfy(notify.NtfyConfig{URL: srv.URL + "/t"}).Send(context.Background(), notify.Message{Subject: "s", Actions: []notify.Action{a}})
		if err == nil || !notify.IsPermanent(err) {
			t.Errorf("action %+v: err = %v, want permanent", a, err)
		}
	}
	if len(*got) != 0 {
		t.Errorf("%d requests sent for refused actions", len(*got))
	}
}
