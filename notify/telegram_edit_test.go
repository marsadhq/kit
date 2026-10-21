package notify

// Internal test: like telegram_test.go it points the unexported
// telegramAPIBase seam at an httptest server and must not run in parallel
// with the other Telegram tests.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// telegramFake answers every Bot API call with status and body, recording
// each request's method and JSON body.
type telegramFake struct {
	status  int
	answer  string
	methods []string
	bodies  []map[string]any
}

func installTelegramFake(t *testing.T, status int, answer string) *telegramFake {
	t.Helper()
	f := &telegramFake{status: status, answer: answer}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.methods = append(f.methods, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		f.bodies = append(f.bodies, body)
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.answer))
	}))
	old := telegramAPIBase
	telegramAPIBase = srv.URL
	t.Cleanup(func() { telegramAPIBase = old; srv.Close() })
	return f
}

// plainJSON encodes v as compact JSON without escaping <, > and &, so a
// test can spell the body it expects.
func plainJSON(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

// TestTelegramHTMLText: a message with TelegramHTML is sent as that text
// with parse_mode HTML, keeping its buttons and its quietness; Subject and
// Body are not sent.
func TestTelegramHTMLText(t *testing.T) {
	f := installTelegramFake(t, http.StatusOK, `{"ok":true}`)
	p := NewTelegram("1:x", "-100")
	m := Message{Subject: "plain subject", Body: "plain body", TelegramHTML: "<b>Disk full</b>\n<a href=\"https://ward.example/i/1\">Open incident</a>",
		Priority: PriorityLow, Actions: []Action{LinkAction("View", "https://ward.example/i/1")}}
	if err := p.Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := f.bodies[0]
	if got["text"] != m.TelegramHTML || got["parse_mode"] != "HTML" || got["disable_notification"] != true || got["reply_markup"] == nil {
		t.Fatalf("payload = %v", got)
	}
}

// TestTelegramSendMessageReturnsTheMessage: SendMessage reports the chat and
// message id Telegram gave the message, and an answer that does not say
// gives a zero TelegramMessage without failing the send.
func TestTelegramSendMessageReturnsTheMessage(t *testing.T) {
	f := installTelegramFake(t, http.StatusOK, `{"ok":true,"result":{"message_id":77,"chat":{"id":-1001234567890,"type":"supergroup"},"date":1}}`)
	p := NewTelegram("1:x", "@pager")
	got, err := p.SendMessage(context.Background(), Message{Subject: "s", Body: "b"})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got != (TelegramMessage{ChatID: "-1001234567890", MessageID: 77}) {
		t.Fatalf("message = %+v", got)
	}
	f.answer = `{"ok":true}`
	if got, err := p.SendMessage(context.Background(), Message{Subject: "s", Body: "b"}); err != nil || got != (TelegramMessage{}) {
		t.Fatalf("an answer without a result = %+v, %v; want a zero message and no error", got, err)
	}
	f.answer = `{"ok":true,"result":{"message_id":5}}`
	if got, _ := p.SendMessage(context.Background(), Message{Subject: "s", Body: "b"}); got != (TelegramMessage{ChatID: "@pager", MessageID: 5}) {
		t.Fatalf("without the chat in the answer = %+v, want the configured chat", got)
	}
}

// TestTelegramEdit: Edit calls editMessageText on the message with m's text
// and buttons; with no buttons it removes the keyboard explicitly.
func TestTelegramEdit(t *testing.T) {
	f := installTelegramFake(t, http.StatusOK, `{"ok":true,"result":{"message_id":77}}`)
	p := NewTelegram("1:x", "-100")
	msg := TelegramMessage{ChatID: "-1001234567890", MessageID: 77}
	m := Message{Subject: "s", Body: "b", TelegramHTML: "<b>s</b>", Priority: PriorityLow, Actions: []Action{
		CallbackAction("✓ Acknowledged by Alice", "ack:12"), HTTPAction("Ack", "POST", "https://ward.example/a/x"), LinkAction("View", "https://ward.example/i/12")}}
	if err := p.Edit(context.Background(), msg, m); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if f.methods[0] != "editMessageText" {
		t.Fatalf("method = %s", f.methods[0])
	}
	b := plainJSON(f.bodies[0])
	want := `{"chat_id":"-1001234567890","message_id":77,"parse_mode":"HTML","reply_markup":{"inline_keyboard":[[{"callback_data":"ack:12","text":"✓ Acknowledged by Alice"},{"text":"View","url":"https://ward.example/i/12"}]]},"text":"<b>s</b>"}`
	if b != want {
		t.Fatalf("edit body = %s\nwant        %s", b, want)
	}
	if err := p.Edit(context.Background(), msg, Message{Subject: "s", Body: "b"}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	b = plainJSON(f.bodies[1])
	if want := `{"chat_id":"-1001234567890","message_id":77,"reply_markup":{"inline_keyboard":[]},"text":"s\nb"}`; b != want {
		t.Fatalf("plain edit body = %s\nwant              %s", b, want)
	}
	// A button Telegram would refuse fails before anything is sent.
	if err := p.Edit(context.Background(), msg, Message{Subject: "s", Actions: []Action{CallbackAction("x", "")}}); !IsPermanent(err) || len(f.methods) != 2 {
		t.Fatalf("bad button: err %v, %d calls", err, len(f.methods))
	}
}

// TestTelegramEditOutcomes: how Edit reads Telegram's answers. A message
// that already reads so is a success; one Telegram no longer has, or will
// no longer edit, is ErrMessageGone (permanent); flood control asks for
// its retry_after; other refusals are permanent, server errors transient.
// No error carries the token.
func TestTelegramEditOutcomes(t *testing.T) {
	const token = "7777777:SUPERSECRETBOTTOKEN"
	cases := []struct {
		name      string
		status    int
		answer    string
		ok        bool
		gone      bool
		permanent bool
		after     time.Duration
	}{
		{"not modified", 400, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message"}`, true, false, false, 0},
		{"not found", 400, `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`, false, true, true, 0},
		{"too old", 400, `{"ok":false,"error_code":400,"description":"Bad Request: message can't be edited"}`, false, true, true, 0},
		{"flood control", 429, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 35","parameters":{"retry_after":35}}`, false, false, false, 35 * time.Second},
		{"other refusal", 400, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`, false, false, true, 0},
		{"blocked", 403, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`, false, false, true, 0},
		{"server error", 502, `bad gateway`, false, false, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installTelegramFake(t, c.status, c.answer)
			err := NewTelegram(token, "1").Edit(context.Background(), TelegramMessage{ChatID: "1", MessageID: 2}, Message{Subject: "s"})
			if c.ok {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("err = nil")
			}
			if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "SUPERSECRET") {
				t.Fatalf("error leaked the token: %v", err)
			}
			if errors.Is(err, ErrMessageGone) != c.gone || IsPermanent(err) != c.permanent {
				t.Fatalf("err = %v: gone %v permanent %v, want %v %v", err, errors.Is(err, ErrMessageGone), IsPermanent(err), c.gone, c.permanent)
			}
			if after, ok := RetryAfter(err); c.after != 0 && (!ok || after != c.after) {
				t.Fatalf("retry after = %v %v, want %v", after, ok, c.after)
			}
		})
	}
	// A transport error names the method, never the token.
	old := telegramAPIBase
	telegramAPIBase = "http://127.0.0.1:1"
	defer func() { telegramAPIBase = old }()
	err := NewTelegram(token, "1").Edit(context.Background(), TelegramMessage{ChatID: "1", MessageID: 2}, Message{Subject: "s"})
	if err == nil || strings.Contains(err.Error(), "SUPERSECRET") || !strings.HasPrefix(err.Error(), "telegram editMessageText: ") {
		t.Fatalf("transport error = %v", err)
	}
}

// TestTelegramSendHonoursFloodControl: a 429 on a send asks the worker to
// wait Telegram's retry_after, and stays retryable.
func TestTelegramSendHonoursFloodControl(t *testing.T) {
	installTelegramFake(t, http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7","parameters":{"retry_after":7}}`)
	err := NewTelegram("1:x", "1").Send(context.Background(), Message{Subject: "s"})
	if after, ok := RetryAfter(err); !ok || after != 7*time.Second || IsPermanent(err) {
		t.Fatalf("err = %v (retry after %v %v)", err, after, ok)
	}
	if err.Error() != "telegram sendMessage: status 429" {
		t.Fatalf("error text = %q", err.Error())
	}
}
