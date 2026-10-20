package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/marsadhq/kit/urlredact"
)

// telegramAPIBase is the Telegram Bot API root. It is a package var so tests can
// point Send at an httptest server (the same seam idea as smtpSend).
var telegramAPIBase = "https://api.telegram.org"

// TelegramProvider delivers notifications to a Telegram chat via the Bot API.
// The bot token authenticates the request and lives in the URL path, so it is
// treated as a credential and never allowed into an error message.
type TelegramProvider struct {
	token  string
	chatID string
}

// NewTelegram constructs a TelegramProvider for the given bot token and chat id.
func NewTelegram(token, chatID string) *TelegramProvider {
	return &TelegramProvider{token: token, chatID: chatID}
}

// Send POSTs {chat_id, text} to the Bot API sendMessage method, where text is
// the subject and body joined by a newline (same composition as Slack/Discord).
// net/http quotes the full request URL (whose path contains the token) in both
// request-construction and transport errors, so every error goes through
// urlredact.Error and names only the API host.
//
// When m has link or callback actions they become an inline keyboard under
// the message (reply_markup): a link is a URL button, a callback a
// callback_data button the tool's bot receives as a callback query. HTTP
// actions have no Telegram form and are left out. A button Telegram would
// refuse (no label, no URL, callback data empty or over 64 bytes) fails the
// send as a permanent error, before anything is sent.
func (p *TelegramProvider) Send(ctx context.Context, m Message) error {
	payload := map[string]any{
		"chat_id": p.chatID,
		"text":    m.Subject + "\n" + m.Body,
	}
	keyboard, err := telegramKeyboard(actionsOf(m, ActionLink, ActionCallback))
	if err != nil {
		return Permanent(err)
	}
	if keyboard != nil {
		payload["reply_markup"] = keyboard
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	rawURL := telegramAPIBase + "/bot" + p.token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram sendMessage: %w", urlredact.Error(rawURL, err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram sendMessage: %w", urlredact.Error(rawURL, err))
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpStatusError("telegram sendMessage", resp.StatusCode)
	}
	return nil
}

// telegramButtonsPerRow is the most buttons Telegram puts in one keyboard
// row; more wrap onto the next row.
const telegramButtonsPerRow = 8

// telegramMaxCallbackData is Telegram's limit on callback_data, in bytes.
const telegramMaxCallbackData = 64

// telegramButton is an InlineKeyboardButton: text plus exactly one of url and
// callback_data.
type telegramButton struct {
	Text         string `json:"text"`
	URL          string `json:"url,omitempty"`
	CallbackData string `json:"callback_data,omitempty"`
}

// telegramKeyboard builds the InlineKeyboardMarkup for actions, or nil when
// there are none. The error never quotes a URL or callback data.
func telegramKeyboard(actions []Action) (map[string][][]telegramButton, error) {
	if len(actions) == 0 {
		return nil, nil
	}
	var rows [][]telegramButton
	for i, a := range actions {
		if a.Label == "" {
			return nil, fmt.Errorf("telegram: button %d has no label", i+1)
		}
		b := telegramButton{Text: a.Label}
		switch a.Kind {
		case ActionLink:
			if a.URL == "" {
				return nil, fmt.Errorf("telegram: button %q has no URL", a.Label)
			}
			b.URL = a.URL
		case ActionCallback:
			if n := len(a.Data); n == 0 || n > telegramMaxCallbackData {
				return nil, fmt.Errorf("telegram: button %q callback data is %d bytes, want 1 to %d", a.Label, n, telegramMaxCallbackData)
			}
			b.CallbackData = a.Data
		}
		if len(rows) == 0 || len(rows[len(rows)-1]) == telegramButtonsPerRow {
			rows = append(rows, nil)
		}
		rows[len(rows)-1] = append(rows[len(rows)-1], b)
	}
	return map[string][][]telegramButton{"inline_keyboard": rows}, nil
}

// Compile-time assertion: TelegramProvider must satisfy the Provider interface.
var _ Provider = (*TelegramProvider)(nil)
