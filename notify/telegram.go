package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

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

// TelegramMessage identifies a message the Telegram provider sent: what
// Edit needs to change it later.
type TelegramMessage struct {
	// ChatID is the chat's id as Telegram reported it (the configured chat
	// id when its answer did not say).
	ChatID    string
	MessageID int64
}

// ErrMessageGone is the error Edit returns for a message Telegram no longer
// has (it was deleted) or will no longer edit. It is permanent.
var ErrMessageGone = errors.New("the message is gone or can no longer be edited")

// Send POSTs {chat_id, text} to the Bot API sendMessage method, where text is
// the subject and body joined by a newline (same composition as Slack/Discord),
// with disable_notification set for a low or minimum priority message. A
// message with TelegramHTML is sent as that text instead, with parse_mode
// HTML.
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
//
// A 4xx answer is permanent, except 408 and 429; a 429 (Telegram's flood
// control) asks the worker to wait the retry_after it names (RetryAfter).
func (p *TelegramProvider) Send(ctx context.Context, m Message) error {
	_, err := p.SendMessage(ctx, m)
	return err
}

// SendMessage is Send, reporting the message Telegram created so it can be
// edited later. An answer that does not name the message gives a zero
// TelegramMessage, not an error: the message was sent.
func (p *TelegramProvider) SendMessage(ctx context.Context, m Message) (TelegramMessage, error) {
	payload := telegramText(m)
	payload["chat_id"] = p.chatID
	// A low or minimum priority message arrives without sound; the field is
	// left out otherwise so other messages are sent exactly as before.
	if m.Priority == PriorityMin || m.Priority == PriorityLow {
		payload["disable_notification"] = true
	}
	keyboard, err := telegramKeyboard(actionsOf(m, ActionLink, ActionCallback))
	if err != nil {
		return TelegramMessage{}, Permanent(err)
	}
	if keyboard != nil {
		payload["reply_markup"] = keyboard
	}
	res, _, err := p.call(ctx, "sendMessage", payload)
	if err != nil {
		return TelegramMessage{}, err
	}
	var sent struct {
		MessageID int64 `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	}
	if json.Unmarshal(res, &sent) != nil || sent.MessageID == 0 {
		return TelegramMessage{}, nil
	}
	chat := p.chatID
	if sent.Chat.ID != 0 {
		chat = strconv.FormatInt(sent.Chat.ID, 10)
	}
	return TelegramMessage{ChatID: chat, MessageID: sent.MessageID}, nil
}

// Edit replaces the text and the buttons of a message the bot sent with
// m's (editMessageText), composed as Send composes them; a message without
// link or callback actions loses its keyboard. Priority means nothing to
// an edit, which never rings.
//
// Telegram answering that the message already reads so is a success. A
// message it no longer has, or will no longer edit, is ErrMessageGone. A
// 429 asks for its retry_after (RetryAfter); other 4xx answers are
// permanent. No error carries the token.
func (p *TelegramProvider) Edit(ctx context.Context, msg TelegramMessage, m Message) error {
	payload := telegramText(m)
	payload["chat_id"] = msg.ChatID
	payload["message_id"] = msg.MessageID
	keyboard, err := telegramKeyboard(actionsOf(m, ActionLink, ActionCallback))
	if err != nil {
		return Permanent(err)
	}
	if keyboard == nil {
		keyboard = map[string][][]telegramButton{"inline_keyboard": {}}
	}
	payload["reply_markup"] = keyboard
	_, desc, err := p.call(ctx, "editMessageText", payload)
	if err == nil {
		return nil
	}
	d := strings.ToLower(desc)
	switch {
	case strings.Contains(d, "message is not modified"):
		return nil
	case strings.Contains(d, "message to edit not found"), strings.Contains(d, "message can't be edited"):
		return Permanent(fmt.Errorf("telegram editMessageText: %w", ErrMessageGone))
	}
	return err
}

// telegramText is the text part of a sendMessage or editMessageText
// payload: TelegramHTML with parse_mode HTML, else the subject and body.
func telegramText(m Message) map[string]any {
	if m.TelegramHTML != "" {
		return map[string]any{"text": m.TelegramHTML, "parse_mode": "HTML"}
	}
	return map[string]any{"text": m.Subject + "\n" + m.Body}
}

// telegramMaxAnswer bounds how much of a Bot API answer is read.
const telegramMaxAnswer = 64 << 10

// call POSTs payload to a Bot API method. It returns the answer's result on
// success; otherwise the error (httpStatusError's classification, with a
// 429's retry_after kept) and Telegram's description of it, for the caller
// to classify. Neither the error nor anything else quotes the token or the
// description.
func (p *TelegramProvider) call(ctx context.Context, method string, payload map[string]any) (json.RawMessage, string, error) {
	what := "telegram " + method
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	rawURL := telegramAPIBase + "/bot" + p.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", what, urlredact.Error(rawURL, err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", what, urlredact.Error(rawURL, err))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, telegramMaxAnswer))
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	var answer struct {
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	_ = json.Unmarshal(data, &answer)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return answer.Result, "", nil
	}
	err = httpStatusError(what, resp.StatusCode)
	if resp.StatusCode == http.StatusTooManyRequests && answer.Parameters.RetryAfter > 0 {
		err = RetryLater(err, time.Duration(answer.Parameters.RetryAfter)*time.Second)
	}
	return nil, answer.Description, err
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
