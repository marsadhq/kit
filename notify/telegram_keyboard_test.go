package notify

// Internal test: like telegram_test.go it points the unexported
// telegramAPIBase seam at an httptest server and must not run in parallel
// with the other Telegram tests.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// telegramCapture points the Bot API at a server recording each request body.
func telegramCapture(t *testing.T) *[]string {
	t.Helper()
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	old := telegramAPIBase
	telegramAPIBase = srv.URL
	t.Cleanup(func() { telegramAPIBase = old; srv.Close() })
	return &bodies
}

// TestTelegramBodyWithoutActionsIsUnchanged: with no buttons the request body
// is byte for byte what the provider sent before keyboards existed.
func TestTelegramBodyWithoutActionsIsUnchanged(t *testing.T) {
	bodies := telegramCapture(t)
	p := NewTelegram("123:abc", "-100")
	m := Message{Subject: "Job failed: backup", Body: "status=failed exit_code=3"}
	// An HTTP action has no Telegram form, so it changes nothing either.
	for _, m := range []Message{m, {Subject: m.Subject, Body: m.Body, Actions: []Action{HTTPAction("Ack", "POST", "https://ward.example/ack")}}} {
		if err := p.Send(context.Background(), m); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	want, _ := json.Marshal(map[string]string{"chat_id": "-100", "text": "Job failed: backup\nstatus=failed exit_code=3"})
	for i, got := range *bodies {
		if got != string(want) {
			t.Errorf("request %d body = %s, want %s", i+1, got, want)
		}
	}
}

// TestTelegramInlineKeyboard: callback and link actions become one row of
// buttons, in order, under the message; HTTP actions are left out.
func TestTelegramInlineKeyboard(t *testing.T) {
	bodies := telegramCapture(t)
	m := Message{Subject: "Disk full", Body: "db-1", Actions: []Action{
		CallbackAction("Acknowledge", "ack:12"),
		HTTPAction("Resolve", "POST", "https://ward.example/api/incidents/12/resolve"),
		LinkAction("View", "https://ward.example/incidents/12"),
	}}
	if err := NewTelegram("123:abc", "-100").Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	const want = `{"chat_id":"-100","reply_markup":{"inline_keyboard":[[` +
		`{"text":"Acknowledge","callback_data":"ack:12"},` +
		`{"text":"View","url":"https://ward.example/incidents/12"}]]},"text":"Disk full\ndb-1"}`
	if got := (*bodies)[0]; got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
}

// TestTelegramKeyboardWrapsRows: Telegram takes at most eight buttons a row.
func TestTelegramKeyboardWrapsRows(t *testing.T) {
	var actions []Action
	for i := range 10 {
		actions = append(actions, CallbackAction("b", "d"+string(rune('0'+i))))
	}
	kb, err := telegramKeyboard(actions)
	if err != nil {
		t.Fatalf("telegramKeyboard: %v", err)
	}
	rows := kb["inline_keyboard"]
	if len(rows) != 2 || len(rows[0]) != 8 || len(rows[1]) != 2 || rows[1][1].CallbackData != "d9" {
		t.Fatalf("rows = %+v, want 8 then 2 in order", rows)
	}
}

// TestTelegramRefusesBadButtons: a button Telegram would refuse fails the
// send as permanent (no retry can fix it) and nothing is sent. The error
// never quotes the data.
func TestTelegramRefusesBadButtons(t *testing.T) {
	bodies := telegramCapture(t)
	long := strings.Repeat("x", 65)
	for name, a := range map[string]Action{
		"no label":           CallbackAction("", "ack:1"),
		"empty callback":     CallbackAction("Acknowledge", ""),
		"65-byte callback":   CallbackAction("Acknowledge", long),
		"link without a URL": LinkAction("View", ""),
	} {
		t.Run(name, func(t *testing.T) {
			err := NewTelegram("123:abc", "-100").Send(context.Background(), Message{Subject: "s", Actions: []Action{a}})
			if err == nil {
				t.Fatal("Send accepted a button Telegram refuses")
			}
			if !IsPermanent(err) {
				t.Errorf("err = %v, want permanent", err)
			}
			if strings.Contains(err.Error(), long) {
				t.Errorf("error quotes the callback data: %v", err)
			}
		})
	}
	if len(*bodies) != 0 {
		t.Fatalf("%d requests sent for refused buttons, want none", len(*bodies))
	}
	// 64 bytes is still fine.
	if err := NewTelegram("123:abc", "-100").Send(context.Background(), Message{Subject: "s",
		Actions: []Action{CallbackAction("Acknowledge", long[:64])}}); err != nil {
		t.Fatalf("64-byte callback data: %v", err)
	}
}
