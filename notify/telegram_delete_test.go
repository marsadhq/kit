package notify

// Internal test: like telegram_test.go it points the unexported
// telegramAPIBase seam at an httptest server and must not run in parallel
// with the other Telegram tests.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestTelegramDelete: Delete calls deleteMessage on the message, in its own
// chat.
func TestTelegramDelete(t *testing.T) {
	f := installTelegramFake(t, http.StatusOK, `{"ok":true,"result":true}`)
	p := NewTelegram("1:x", "-100")
	if err := p.Delete(context.Background(), TelegramMessage{ChatID: "-1001234567890", MessageID: 77}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(f.methods) != 1 || f.methods[0] != "deleteMessage" {
		t.Fatalf("methods = %v", f.methods)
	}
	if b, want := plainJSON(f.bodies[0]), `{"chat_id":"-1001234567890","message_id":77}`; b != want {
		t.Fatalf("delete body = %s\nwant          %s", b, want)
	}
}

// TestTelegramDeleteOutcomes: how Delete reads Telegram's answers. A message
// Telegram no longer has was deleted already: a success. One it will not
// delete (too old, or not the bot's to delete) is ErrNotDeletable
// (permanent); flood control asks for its retry_after; other refusals are
// permanent, server errors transient. No error carries the token.
func TestTelegramDeleteOutcomes(t *testing.T) {
	const token = "7777777:SUPERSECRETBOTTOKEN"
	cases := []struct {
		name      string
		status    int
		answer    string
		ok        bool
		refused   bool
		permanent bool
		after     time.Duration
	}{
		{"deleted already", 400, `{"ok":false,"error_code":400,"description":"Bad Request: message to delete not found"}`, true, false, false, 0},
		{"too old", 400, `{"ok":false,"error_code":400,"description":"Bad Request: message can't be deleted"}`, false, true, true, 0},
		{"not for everyone", 400, `{"ok":false,"error_code":400,"description":"Bad Request: message can't be deleted for everyone"}`, false, true, true, 0},
		{"flood control", 429, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 12","parameters":{"retry_after":12}}`, false, false, false, 12 * time.Second},
		{"other refusal", 400, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`, false, false, true, 0},
		{"kicked", 403, `{"ok":false,"error_code":403,"description":"Forbidden: bot was kicked from the group chat"}`, false, false, true, 0},
		{"server error", 502, `bad gateway`, false, false, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installTelegramFake(t, c.status, c.answer)
			err := NewTelegram(token, "1").Delete(context.Background(), TelegramMessage{ChatID: "1", MessageID: 2})
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
			if errors.Is(err, ErrNotDeletable) != c.refused || IsPermanent(err) != c.permanent {
				t.Fatalf("err = %v: not deletable %v permanent %v, want %v %v", err, errors.Is(err, ErrNotDeletable), IsPermanent(err), c.refused, c.permanent)
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
	err := NewTelegram(token, "1").Delete(context.Background(), TelegramMessage{ChatID: "1", MessageID: 2})
	if err == nil || strings.Contains(err.Error(), "SUPERSECRET") || !strings.HasPrefix(err.Error(), "telegram deleteMessage: ") {
		t.Fatalf("transport error = %v", err)
	}
}
