package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/marsadhq/kit/urlredact"
)

// pushoverAPIURL is the Pushover messages endpoint. It is a package var so
// tests can point Send at an httptest server.
var pushoverAPIURL = "https://api.pushover.net/1/messages.json"

// Pushover's limits and the emergency-priority defaults.
const (
	pushoverMaxMessage  = 1024 // characters
	pushoverMaxTitle    = 250
	pushoverMaxURL      = 512
	pushoverMaxURLTitle = 100

	// PushoverEmergency is Pushover's emergency priority: the notification
	// repeats every Retry seconds until acknowledged or Expire seconds pass.
	PushoverEmergency = 2

	pushoverMinRetry      = 30    // seconds; Pushover's minimum
	pushoverMaxExpire     = 10800 // seconds; Pushover's maximum (3 hours)
	pushoverDefaultRetry  = 60
	pushoverDefaultExpire = 3600

	// pushoverRetryWait is the least Pushover asks clients to wait before
	// retrying a request that failed for a reason of its own.
	pushoverRetryWait = 5 * time.Second
)

// PushoverConfig is a Pushover channel's configuration. The token and user
// key are credentials and never appear in an error.
type PushoverConfig struct {
	// Token is the application's API token.
	Token string `json:"token"`
	// User is the user or group key to notify.
	User string `json:"user"`
	// Device optionally limits delivery to these devices (comma-separated).
	Device string `json:"device"`
	// Sound optionally names the notification sound.
	Sound string `json:"sound"`
	// Priority is the channel's default, on Pushover's scale: -2 (no alert)
	// to 2 (emergency); 0 is normal. A message's own Priority wins.
	Priority int `json:"priority"`
	// Retry is how often, in seconds, an emergency notification repeats: at
	// least 30. 0 means 60.
	Retry int `json:"retry"`
	// Expire is how long, in seconds, an emergency notification keeps
	// repeating: at most 10800. 0 means 3600.
	Expire int `json:"expire"`
}

// validate reports the first problem with c.
func (c PushoverConfig) validate() error {
	switch {
	case c.Token == "":
		return errors.New("missing token")
	case c.User == "":
		return errors.New("missing user")
	case c.Priority < -2 || c.Priority > PushoverEmergency:
		return fmt.Errorf("priority %d, want -2 to 2", c.Priority)
	case c.Retry != 0 && c.Retry < pushoverMinRetry:
		return fmt.Errorf("retry %ds, want at least %ds", c.Retry, pushoverMinRetry)
	case c.Expire < 0 || c.Expire > pushoverMaxExpire:
		return fmt.Errorf("expire %ds, want at most %ds", c.Expire, pushoverMaxExpire)
	}
	return nil
}

// PushoverProvider sends notifications through the Pushover messages API.
type PushoverProvider struct{ c PushoverConfig }

// NewPushover constructs a PushoverProvider. BuildProvider validates the
// configuration first.
func NewPushover(c PushoverConfig) *PushoverProvider { return &PushoverProvider{c} }

// pushoverPriority maps a message priority onto Pushover's scale.
var pushoverPriority = map[Priority]int{
	PriorityMin:    -2,
	PriorityLow:    -1,
	PriorityNormal: 0,
	PriorityHigh:   1,
	PriorityUrgent: PushoverEmergency,
}

// priority is the Pushover priority to send m with: the message's, else the
// channel's.
func (p *PushoverProvider) priority(m Message) int {
	if v, ok := pushoverPriority[m.Priority]; ok {
		return v
	}
	return p.c.Priority
}

// truncate shortens s to at most n characters, marking a cut with "…".
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// form builds the messages API request for m.
func (p *PushoverProvider) form(m Message) url.Values {
	v := url.Values{}
	v.Set("token", p.c.Token)
	v.Set("user", p.c.User)
	text, title := m.Body, m.Subject
	if text == "" {
		text, title = m.Subject, ""
	}
	v.Set("message", truncate(text, pushoverMaxMessage))
	if title != "" {
		v.Set("title", truncate(title, pushoverMaxTitle))
	}
	prio := p.priority(m)
	if prio != 0 {
		v.Set("priority", strconv.Itoa(prio))
	}
	if prio == PushoverEmergency {
		retry, expire := p.c.Retry, p.c.Expire
		if retry == 0 {
			retry = pushoverDefaultRetry
		}
		if expire == 0 {
			expire = pushoverDefaultExpire
		}
		v.Set("retry", strconv.Itoa(retry))
		v.Set("expire", strconv.Itoa(expire))
		// Pushover posts its acknowledgement to the callback URL; the first
		// HTTP action (an "Acknowledge" endpoint) is that URL.
		if ack := actionsOf(m, ActionHTTP); len(ack) > 0 && ack[0].URL != "" {
			v.Set("callback", ack[0].URL)
		}
	}
	// The first link becomes the supplementary URL. One Pushover would refuse
	// as too long is left out rather than cut, which would break it.
	if links := actionsOf(m, ActionLink); len(links) > 0 && links[0].URL != "" && len(links[0].URL) <= pushoverMaxURL {
		v.Set("url", links[0].URL)
		if links[0].Label != "" {
			v.Set("url_title", truncate(links[0].Label, pushoverMaxURLTitle))
		}
	}
	if p.c.Device != "" {
		v.Set("device", p.c.Device)
	}
	if p.c.Sound != "" {
		v.Set("sound", p.c.Sound)
	}
	return v
}

// Send posts m to the messages API: the subject as the title and the body as
// the message (the subject alone is the message when there is no body), both
// cut to Pushover's limits. Message.Priority maps onto Pushover's scale
// (PriorityUrgent is emergency, with the channel's retry and expire, and the
// first HTTP action's URL as the acknowledgement callback); the first link
// action becomes the supplementary URL and its label the URL title; callback
// actions have no Pushover form.
//
// Pushover asks clients never to repeat a request it answered 4xx (a bad
// token or user key, an exhausted quota) and to wait at least five seconds
// before retrying anything else. So every 4xx, 429 included, is permanent,
// and other failures ask the worker for that wait (RetryLater). Errors carry
// Pushover's own explanation but never the token or user key.
func (p *PushoverProvider) Send(ctx context.Context, m Message) error {
	_, err := p.SendReceipt(ctx, m)
	return err
}

// SendReceipt is Send, reporting the receipt Pushover gives an
// emergency-priority message: what CancelReceipt needs to stop its repeats.
// It is "" for any other message, and when the answer names none (or
// something that is no receipt): the message was sent all the same.
func (p *PushoverProvider) SendReceipt(ctx context.Context, m Message) (string, error) {
	answer, code, err := p.post(ctx, pushoverAPIURL, "pushover messages", p.form(m))
	if err != nil {
		return "", err
	}
	if code < 200 || code >= 300 || answer.Status != 1 {
		err := p.refusal("pushover messages", code, answer)
		if code >= 400 && code < 500 {
			return "", Permanent(err)
		}
		return "", RetryLater(err, pushoverRetryWait)
	}
	if !pushoverReceiptPattern.MatchString(answer.Receipt) {
		return "", nil
	}
	return answer.Receipt, nil
}

// pushoverReceiptPattern is what Ward accepts as a receipt: Pushover's are
// 30 letters and digits, and one goes into a URL path.
var pushoverReceiptPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

// pushoverLimitedWait is how long a rate-limited receipt call waits when
// Pushover does not say.
const pushoverLimitedWait = time.Minute

// CancelReceipt stops the repeats of the emergency-priority message receipt
// names (POST receipts/{receipt}/cancel.json with the application's token):
// the notification stays on the phone but no longer alerts. A receipt that
// is not one is refused before any request.
//
// A 4xx answer is permanent, as Pushover asks never to repeat one: it is how
// Pushover answers a receipt that expired, was never its own, or a bad token.
// A 429 asks to wait its Retry-After (a minute when it gives none) and is not
// permanent; other failures ask for Pushover's five seconds (RetryLater).
// StatusCode reads an answer's status. No error carries the token.
func (p *PushoverProvider) CancelReceipt(ctx context.Context, receipt string) error {
	if !pushoverReceiptPattern.MatchString(receipt) {
		return Permanent(errors.New("pushover: a receipt is 1 to 64 letters and digits"))
	}
	what := "pushover receipt cancel"
	rawURL := strings.TrimSuffix(pushoverAPIURL, "messages.json") + "receipts/" + receipt + "/cancel.json"
	v := url.Values{}
	v.Set("token", p.c.Token)
	answer, code, err := p.post(ctx, rawURL, what, v)
	if err != nil {
		return err
	}
	if code >= 200 && code < 300 && answer.Status == 1 {
		return nil
	}
	err = p.refusal(what, code, answer)
	switch {
	case code == http.StatusTooManyRequests:
		return RetryLater(err, retryAfterHeader(answer.header, pushoverLimitedWait))
	case code >= 400 && code < 500:
		return Permanent(err)
	}
	return RetryLater(err, pushoverRetryWait)
}

// pushoverAnswer is the part of a Pushover API answer kit reads.
type pushoverAnswer struct {
	Status  int      `json:"status"`
	Errors  []string `json:"errors"`
	Receipt string   `json:"receipt"`
	header  http.Header
}

// post sends form to one of Pushover's endpoints and reads its answer. Only a
// failure to get an answer is an error here (retryable after Pushover's
// wait); the caller classifies the answer.
func (p *PushoverProvider) post(ctx context.Context, rawURL, what string, form url.Values) (pushoverAnswer, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return pushoverAnswer{}, 0, fmt.Errorf("%s %w", what, urlredact.Error(rawURL, err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return pushoverAnswer{}, 0, RetryLater(fmt.Errorf("%s %w", what, urlredact.Error(rawURL, err)), pushoverRetryWait)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_, _ = io.Copy(io.Discard, resp.Body)
	var answer pushoverAnswer
	_ = json.Unmarshal(raw, &answer) // an unreadable answer leaves status 0: not accepted
	answer.header = resp.Header
	return answer, resp.StatusCode, nil
}

// refusal is the error for an answer Pushover did not accept: its status
// (StatusCode reads it), and Pushover's explanation without the token or
// user key.
func (p *PushoverProvider) refusal(what string, code int, answer pushoverAnswer) error {
	var err error = &statusError{what: what + " " + urlredact.Host(pushoverAPIURL), code: code}
	if code >= 200 && code < 300 {
		err = fmt.Errorf("%w without the accepted answer", err)
	}
	if len(answer.Errors) > 0 {
		err = fmt.Errorf("%w: %s", err, detail(strings.Join(answer.Errors, "; "), p.c.Token, p.c.User))
	}
	return err
}

// retryAfterHeader reads a Retry-After header given in seconds, else def.
func retryAfterHeader(h http.Header, def time.Duration) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	return def
}

// Compile-time assertion: PushoverProvider must satisfy the Provider interface.
var _ Provider = (*PushoverProvider)(nil)
