package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"
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

// discordMaxContent is the most a message's content may be, in UTF-16 code
// units, which is how Discord counts it; it refuses more.
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
// to ping. Subject and body are escaped (discordEscape). Content over
// Discord's limit is cut: Output from its start, so that its end stays, and
// otherwise the text at its end. It returns an error when the HTTP response
// is non-2xx.
func (p *DiscordProvider) Send(ctx context.Context, m Message) error {
	return postJSON(ctx, p.url, discordPayload{
		Content:         discordContent(m),
		AllowedMentions: discordAllowedMentions{Parse: []string{}},
	})
}

// discordEscape backslash-escapes, in text the tool did not mark up, the
// characters Discord builds a masked link ([label](url), which shows a label
// of the sender's choosing), mention-like constructs (<@123>, <#123>,
// <@&123>) and code from: \ [ ] < > and the backtick, which would otherwise
// open or close a code block, the one the Output is shown in included.
// Discord shows them as typed. URLs (discordURL) are left as they are: they
// stay links to themselves, with the brackets of an IPv6 host or a query
// intact, and Discord forms no masked link inside one.
func discordEscape(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range discordURL.FindAllStringIndex(s, -1) {
		b.WriteString(discordEscaper.Replace(s[last:m[0]]))
		b.WriteString(s[m[0]:m[1]])
		last = m[1]
	}
	b.WriteString(discordEscaper.Replace(s[last:]))
	return b.String()
}

var discordEscaper = strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`, "<", `\<`, ">", `\>`, "`", "\\`")

// discordURL matches a URL as discordEscape leaves it: http or https, up to
// white space, an angle bracket or a backtick, which are escaped wherever
// they are.
var discordURL = regexp.MustCompile("https?://[^\\s<>`]+")

// DiscordOutputLength is how much of Discord's limit Output s takes once the
// Discord provider sends it: its UTF-16 code units, a backtick counting
// twice (it is sent followed by a zero-width space, unfence).
func DiscordOutputLength(s string) int { return utf16Len(s) + strings.Count(s, "`") }

// DiscordOutputRoom is how much of Discord's limit the Discord provider
// leaves for m's Output, measured like DiscordOutputLength: what is left
// after m's subject and body, escaped, and the code block's fences. A tool
// that fits its output itself (to keep something other than its end) cuts
// it to this; the provider cuts longer output from its start.
func DiscordOutputRoom(m Message) int {
	return discordMaxContent - utf16Len(discordEscape(m.Subject+"\n"+m.Body)+"\n```\n\n```")
}

// discordContent is m's content as the Discord provider posts it, at most
// discordMaxContent UTF-16 code units.
func discordContent(m Message) string {
	text := discordEscape(m.Subject + "\n" + m.Body)
	if m.Output != "" {
		out := unfence(m.Output)
		if room := DiscordOutputRoom(m); utf16Len(out) > room && room > 1 {
			out = "…" + lastUnits(out, room-1)
		}
		text += "\n```\n" + out + "\n```"
	}
	if utf16Len(text) > discordMaxContent {
		text = firstUnits(text, discordMaxContent-1) + "…"
	}
	return text
}

// utf16Len is the length of s in UTF-16 code units.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// firstUnits is the longest start of s, whole characters, of at most n
// UTF-16 code units.
func firstUnits(s string, n int) string {
	used := 0
	for i, r := range s {
		if used += utf16.RuneLen(r); used > n {
			return s[:i]
		}
	}
	return s
}

// lastUnits is the longest end of s, whole characters, of at most n UTF-16
// code units.
func lastUnits(s string, n int) string {
	used, start := 0, len(s)
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:start])
		if used += utf16.RuneLen(r); used > n {
			break
		}
		start -= size
	}
	return s[start:]
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
