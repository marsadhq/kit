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
)

// Message is a notification ready to be delivered by a Provider: a rendered
// subject and body, plus optional parts a provider uses when it can.
type Message struct {
	// Subject is the one-line summary: an email subject, a push title, the
	// first line of a chat message.
	Subject string
	// Body is the detail text.
	Body string
	// Event is the Marsad event the message reports, if there is one. The
	// webhook provider posts it under "marsad" unless Webhook is set.
	Event *event.V1
	// Webhook, when non-nil, is the value the webhook provider posts, encoded
	// as JSON, in place of its default body. A tool with an established body
	// format sets it: Tend posts an event.TendWebhook. Other providers ignore
	// it.
	Webhook any
}

// Provider delivers a Message to a single destination. Implementations should
// return a non-nil error on any non-success so the delivery worker can retry.
type Provider interface {
	Send(ctx context.Context, m Message) error
}
