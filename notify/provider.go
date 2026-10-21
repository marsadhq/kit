// Package notify delivers notifications: the Provider contract, the Message
// a provider sends, the providers themselves (webhook, Slack, Discord, SMTP,
// Telegram, ntfy, Pushover), BuildProvider to make one from a channel's JSON
// configuration, and the durable delivery Worker that drives a tool's own
// delivery queue with leases, retries and a give-up hook.
//
// Credentials live in URLs (webhook, Slack and Discord paths, ntfy topics),
// in the Telegram bot path, and in request bodies; no error a provider
// returns ever contains one, so the worker may log errors as they are.
//
// The package knows nothing about a tool's events, channels or tables: the
// tool renders its events into Messages and implements Queue over its own
// deliveries.
package notify

import (
	"context"

	"github.com/marsadhq/kit/event"
)

// ChannelType identifies the kind of destination a notification channel
// delivers to. Tend stores it in its notification_channels.kind column.
type ChannelType string

const (
	// Webhook posts JSON to an arbitrary HTTP endpoint.
	Webhook ChannelType = "webhook"
	// Slack posts to a Slack incoming webhook.
	Slack ChannelType = "slack"
	// Discord posts to a Discord webhook.
	Discord ChannelType = "discord"
	// SMTP sends email.
	SMTP ChannelType = "smtp"
	// Telegram posts to a chat via the Telegram Bot API.
	Telegram ChannelType = "telegram"
	// Ntfy publishes to an ntfy topic, on ntfy.sh or a self-hosted server.
	Ntfy ChannelType = "ntfy"
	// Pushover sends a push through the Pushover messages API.
	Pushover ChannelType = "pushover"
)

// Message is a notification ready to be delivered by a Provider: a rendered
// subject and body, plus optional parts a provider uses when it can.
type Message struct {
	// Subject is the one-line summary: an email subject, a push title, the
	// first line of a chat message.
	Subject string
	// Body is the detail text.
	Body string
	// Output, when non-empty, is text the tool passes on without having
	// written it (a job's output, the body a ping logged), which must reach
	// the reader as text, never as markup, a mention or a link with a label
	// of its own. Slack and Discord show it after the body in a code block,
	// its backticks kept from closing the block. The other providers do not
	// read it: a tool sending to them puts it in the body (WithOutputInBody).
	Output string
	// Event is the Marsad event the message reports, if there is one. The
	// webhook provider posts it under "marsad" unless Webhook is set.
	Event *event.V1
	// Webhook, when non-nil, is the value the webhook provider posts, encoded
	// as JSON, in place of its default body. A tool with an established body
	// format sets it: Tend posts an event.TendWebhook. Other providers ignore
	// it.
	Webhook any
	// Priority is how urgently the message should reach its reader; the zero
	// value leaves each channel's configured priority.
	Priority Priority
	// Actions are buttons the reader can press. Each provider renders the
	// kinds it supports and leaves out the rest (see ActionKind); a provider
	// with no buttons ignores them all.
	Actions []Action
	// TelegramHTML, when non-empty, is the text the Telegram provider sends
	// in place of Subject and Body, with parse_mode HTML: Telegram's HTML
	// subset (<b>, <a href>, ...), written by the tool, which must escape
	// every piece of text it did not write itself (html.EscapeString does).
	// Telegram counts its 4096-character limit after the markup is parsed.
	// Other providers ignore it.
	TelegramHTML string
	// NtfySequenceID, when non-empty, is the sequence id the ntfy provider
	// publishes the message under (1 to 64 letters, digits, '-' or '_'): a
	// later message with the same id replaces the notification on the phone,
	// and NtfyProvider.Clear dismisses it (ntfy 2.16 and later; an older
	// server ignores it and shows each message on its own). Other providers
	// ignore it.
	NtfySequenceID string
	// PushoverTags, on an emergency-priority message (PriorityUrgent), are
	// sent as Pushover's tags (each 1 to 64 letters, digits, '-', '_' or
	// '='), so PushoverProvider.CancelByTag can stop the message's repeats
	// without its receipt: every copy, should it be sent twice. Pushover
	// takes no tags below emergency priority, where they are neither sent
	// nor checked, and other providers ignore them.
	PushoverTags []string
}

// WithOutputInBody returns m with its Output after its Body, on the next
// line, and no Output: the message for a provider that shows plain text and
// has no form of its own for Output.
func (m Message) WithOutputInBody() Message {
	switch {
	case m.Output == "":
	case m.Body == "":
		m.Body = m.Output
	default:
		m.Body += "\n" + m.Output
	}
	m.Output = ""
	return m
}

// Priority is how urgently a message should reach its reader, on ntfy's
// five-step scale. Providers with priorities map it onto theirs (ntfy 1 to 5,
// Pushover -2 to 2); the others ignore it.
type Priority int

const (
	PriorityUnset  Priority = iota // the channel's configured priority
	PriorityMin                    // no alert at all where the app allows it
	PriorityLow                    // no sound or vibration
	PriorityNormal                 // the app's default
	PriorityHigh                   // stands out, sound even in quiet hours where configured
	PriorityUrgent                 // as loud as the app gets; Pushover repeats it until acknowledged
)

// ActionKind is what pressing an Action does.
type ActionKind string

const (
	// ActionLink opens URL in the reader's browser. Telegram shows a URL
	// button, ntfy a view action; Pushover uses the first link as the
	// message's supplementary URL.
	ActionLink ActionKind = "link"
	// ActionHTTP makes the receiving app send an HTTP request to URL (Method,
	// Headers, Body) without opening anything: an "Acknowledge" button that
	// calls the tool's API. ntfy shows an http action. At emergency priority
	// Pushover posts its own acknowledgement to the first HTTP action's URL
	// (its callback) once the reader acknowledges, ignoring Method, Headers
	// and Body.
	ActionHTTP ActionKind = "http"
	// ActionCallback hands Data back to the tool's own chat bot when pressed:
	// a Telegram callback_data button, which the bot receives as a callback
	// query.
	ActionCallback ActionKind = "callback"
)

// Action is one button on a Message.
type Action struct {
	Kind  ActionKind
	Label string // the button text; required
	// URL is the page to open (ActionLink) or the endpoint to call
	// (ActionHTTP).
	URL string
	// Method, Headers and Body shape an ActionHTTP request. An empty Method
	// means POST.
	Method  string
	Headers map[string]string
	Body    string
	// Data is what an ActionCallback hands the bot: 1 to 64 bytes, Telegram's
	// limit.
	Data string
	// Clear dismisses the notification once the action has run, where the
	// app supports it (ntfy).
	Clear bool
}

// LinkAction is an ActionLink button opening url.
func LinkAction(label, url string) Action { return Action{Kind: ActionLink, Label: label, URL: url} }

// HTTPAction is an ActionHTTP button sending a method request to url.
func HTTPAction(label, method, url string) Action {
	return Action{Kind: ActionHTTP, Label: label, Method: method, URL: url}
}

// CallbackAction is an ActionCallback button handing data to the tool's bot.
func CallbackAction(label, data string) Action {
	return Action{Kind: ActionCallback, Label: label, Data: data}
}

// actionsOf returns m's actions of the given kinds, in order.
func actionsOf(m Message, kinds ...ActionKind) []Action {
	var out []Action
	for _, a := range m.Actions {
		for _, k := range kinds {
			if a.Kind == k {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// Provider delivers a Message to a single destination. Implementations should
// return a non-nil error on any non-success so the delivery worker can retry.
type Provider interface {
	Send(ctx context.Context, m Message) error
}
