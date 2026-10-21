package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/marsadhq/kit/event"
	"github.com/marsadhq/kit/urlredact"
)

// httpClient is the shared HTTP client for all HTTP-based providers. The
// 10-second timeout prevents hung goroutines when an upstream is slow.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// postJSON marshals payload as JSON, POSTs it to rawURL with Content-Type:
// application/json, and returns a non-nil error if the response status is
// outside the 2xx range: a permanent one (IsPermanent) for a 4xx other than
// 408 and 429, a retryable one otherwise. Every error that could quote the
// URL (request construction, transport, non-2xx status) reports only the host:
// webhook/Slack/Discord URLs embed a secret token in the path, and this error
// is logged by the delivery worker.
func postJSON(ctx context.Context, rawURL string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		// A URL that net/url rejects comes back as a *url.Error quoting the
		// whole URL, just like the transport failure below.
		return fmt.Errorf("webhook POST %w", urlredact.Error(rawURL, err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		// A transport error here is a *url.Error whose message embeds the full
		// URL, secret token included; urlredact.Error keeps only the host and
		// the underlying cause.
		return fmt.Errorf("webhook POST %w", urlredact.Error(rawURL, err))
	}
	defer resp.Body.Close()
	// Drain the body so the underlying connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A 4xx (other than 408/429) is the receiver refusing this
		// notification for good; see httpStatusError.
		return httpStatusError("webhook POST "+urlredact.Host(rawURL), resp.StatusCode)
	}
	return nil
}

// SlackProvider delivers notifications to a Slack incoming webhook URL. The
// message is encoded as a Slack-compatible {"text": "…"} payload.
type SlackProvider struct{ url string }

// NewSlack constructs a SlackProvider that posts to the given incoming webhook URL.
func NewSlack(url string) *SlackProvider { return &SlackProvider{url} }

// Send posts the message subject and body joined by a newline, and its Output
// in a code block after them, to the Slack webhook. The text is escaped
// (slackEscape): Slack reads <...> as a mention (<!channel>, <@U123>) or as a
// link with a label of the sender's choosing, and a subject or body can carry
// text from anyone (a job's name, an alert's title). It returns an error when
// the HTTP response is non-2xx.
func (p *SlackProvider) Send(ctx context.Context, m Message) error {
	text := slackEscape(m.Subject + "\n" + m.Body)
	if m.Output != "" {
		text += "\n```\n" + slackEscape(unfence(m.Output)) + "\n```"
	}
	return postJSON(ctx, p.url, map[string]string{"text": text})
}

// slackEscape escapes the three characters Slack's text format gives a
// meaning to, as Slack asks: & as &amp;, < as &lt;, > as &gt;. Slack shows
// them as typed.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// unfence keeps s from closing a ``` code block it is shown in: every
// backtick is followed by a zero-width space, so no two are next to each
// other.
func unfence(s string) string { return strings.ReplaceAll(s, "`", "`\u200b") }

// DiscordProvider delivers notifications to a Discord webhook URL. The
// message is encoded as a Discord-compatible {"content": "…"} payload.
type DiscordProvider struct{ url string }

// NewDiscord constructs a DiscordProvider that posts to the given Discord webhook URL.
func NewDiscord(url string) *DiscordProvider { return &DiscordProvider{url} }

// discordMaxContent is the most characters Discord takes in a message's
// content; it refuses more.
const discordMaxContent = 2000

// discordPayload is what the Discord provider posts: the content, and
// allowed_mentions with nothing to parse, so that no @everyone, @here, user
// or role mention in it pings anyone.
type discordPayload struct {
	Content         string                 `json:"content"`
	AllowedMentions discordAllowedMentions `json:"allowed_mentions"`
}

type discordAllowedMentions struct {
	Parse []string `json:"parse"`
}

// Send posts the message subject and body joined by a newline, and its Output
// in a code block after them, to the Discord webhook, with no mention allowed
// to ping. Content over Discord's 2000 characters is cut: Output from its
// start, so that its end stays, and otherwise the text at its end. It
// returns an error when the HTTP response is non-2xx.
func (p *DiscordProvider) Send(ctx context.Context, m Message) error {
	return postJSON(ctx, p.url, discordPayload{
		Content:         discordContent(m),
		AllowedMentions: discordAllowedMentions{Parse: []string{}},
	})
}

// discordContent is m's content as the Discord provider posts it, at most
// discordMaxContent characters.
func discordContent(m Message) string {
	text := m.Subject + "\n" + m.Body
	if m.Output != "" {
		head, tail := text+"\n```\n", "\n```"
		out := unfence(m.Output)
		if room := discordMaxContent - utf8.RuneCountInString(head+tail); utf8.RuneCountInString(out) > room && room > 1 {
			r := []rune(out)
			out = "…" + string(r[len(r)-(room-1):])
		}
		text = head + out + tail
	}
	if utf8.RuneCountInString(text) > discordMaxContent {
		r := []rune(text)
		text = string(r[:discordMaxContent-1]) + "…"
	}
	return text
}

// WebhookProvider delivers notifications to a generic HTTP endpoint as JSON.
// The body is Message.Webhook when the tool set it (Tend's is an
// event.TendWebhook: subject, body and its event), and otherwise
// {"body": …, "marsad": <Message.Event>, "subject": …}, with "marsad" left out
// when the message carries no event.
type WebhookProvider struct{ url string }

// NewWebhook constructs a WebhookProvider that posts to the given URL.
func NewWebhook(url string) *WebhookProvider { return &WebhookProvider{url} }

// webhookBody is the default webhook body. Its fields are in key order, the
// order Tend's body has always had, with the envelope where TendWebhook puts
// it.
type webhookBody struct {
	Body    string    `json:"body"`
	Marsad  *event.V1 `json:"marsad,omitempty"`
	Subject string    `json:"subject"`
}

// Send posts the message's JSON body (see WebhookProvider) to the webhook URL.
// It returns an error when the HTTP response is non-2xx.
func (p *WebhookProvider) Send(ctx context.Context, m Message) error {
	if m.Webhook != nil {
		return postJSON(ctx, p.url, m.Webhook)
	}
	return postJSON(ctx, p.url, webhookBody{Body: m.Body, Marsad: m.Event, Subject: m.Subject})
}

// Compile-time assertions: all three providers must satisfy the Provider interface.
var _ Provider = (*SlackProvider)(nil)
var _ Provider = (*DiscordProvider)(nil)
var _ Provider = (*WebhookProvider)(nil)
