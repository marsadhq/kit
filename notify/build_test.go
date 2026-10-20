package notify_test

import (
	"testing"

	"github.com/marsadhq/kit/notify"
)

// TestBuildProvider asserts each channel kind maps to the right concrete
// provider type, and that malformed/unknown configs error.
func TestBuildProvider(t *testing.T) {
	cases := []struct {
		name string
		kind notify.ChannelType
		cfg  string
		want any // concrete provider type expected (by type switch below)
	}{
		{"webhook", notify.Webhook, `{"url":"https://hooks.example/wh"}`, (*notify.WebhookProvider)(nil)},
		{"slack", notify.Slack, `{"webhook_url":"https://hooks.slack.com/x"}`, (*notify.SlackProvider)(nil)},
		{"discord", notify.Discord, `{"webhook_url":"https://discord.com/api/webhooks/x"}`, (*notify.DiscordProvider)(nil)},
		{"smtp", notify.SMTP, `{"host":"smtp.example","port":587,"username":"u","password":"p","from":"a@x","to":["b@x"]}`, (*notify.SMTPProvider)(nil)},
		{"telegram", notify.Telegram, `{"bot_token":"123:abc","chat_id":"-100"}`, (*notify.TelegramProvider)(nil)},
		{"ntfy", notify.Ntfy, `{"url":"https://ntfy.sh/ward-alerts","token":"tk_x","priority":4,"tags":["warning"]}`, (*notify.NtfyProvider)(nil)},
		{"pushover", notify.Pushover, `{"token":"a","user":"u"}`, (*notify.PushoverProvider)(nil)},
		{"pushover emergency", notify.Pushover, `{"token":"a","user":"u","priority":2,"retry":30,"expire":10800,"device":"d","sound":"siren"}`, (*notify.PushoverProvider)(nil)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := notify.BuildProvider(c.kind, []byte(c.cfg))
			if err != nil {
				t.Fatalf("BuildProvider(%s): %v", c.kind, err)
			}
			if p == nil {
				t.Fatalf("BuildProvider(%s): nil provider", c.kind)
			}
			switch c.want.(type) {
			case *notify.WebhookProvider:
				if _, ok := p.(*notify.WebhookProvider); !ok {
					t.Fatalf("kind %s: got %T want *WebhookProvider", c.kind, p)
				}
			case *notify.SlackProvider:
				if _, ok := p.(*notify.SlackProvider); !ok {
					t.Fatalf("kind %s: got %T want *SlackProvider", c.kind, p)
				}
			case *notify.DiscordProvider:
				if _, ok := p.(*notify.DiscordProvider); !ok {
					t.Fatalf("kind %s: got %T want *DiscordProvider", c.kind, p)
				}
			case *notify.SMTPProvider:
				if _, ok := p.(*notify.SMTPProvider); !ok {
					t.Fatalf("kind %s: got %T want *SMTPProvider", c.kind, p)
				}
			case *notify.TelegramProvider:
				if _, ok := p.(*notify.TelegramProvider); !ok {
					t.Fatalf("kind %s: got %T want *TelegramProvider", c.kind, p)
				}
			case *notify.NtfyProvider:
				if _, ok := p.(*notify.NtfyProvider); !ok {
					t.Fatalf("kind %s: got %T want *NtfyProvider", c.kind, p)
				}
			case *notify.PushoverProvider:
				if _, ok := p.(*notify.PushoverProvider); !ok {
					t.Fatalf("kind %s: got %T want *PushoverProvider", c.kind, p)
				}
			}
		})
	}
}

// TestBuildProviderErrors covers unknown kinds, malformed JSON, and missing
// required fields.
func TestBuildProviderErrors(t *testing.T) {
	cases := []struct {
		name string
		kind notify.ChannelType
		cfg  string
	}{
		{"unknown kind", notify.ChannelType("carrier-pigeon"), `{}`},
		{"webhook malformed json", notify.Webhook, `{"url":`},
		{"webhook missing url", notify.Webhook, `{}`},
		{"slack missing webhook_url", notify.Slack, `{}`},
		{"discord missing webhook_url", notify.Discord, `{}`},
		{"smtp missing host", notify.SMTP, `{"port":587,"from":"a@x","to":["b@x"]}`},
		{"smtp missing from", notify.SMTP, `{"host":"smtp.example","port":587,"to":["b@x"]}`},
		{"smtp no recipients", notify.SMTP, `{"host":"smtp.example","port":587,"from":"a@x"}`},
		{"telegram missing bot_token", notify.Telegram, `{"chat_id":"-100"}`},
		{"telegram missing chat_id", notify.Telegram, `{"bot_token":"123:abc"}`},
		{"ntfy missing url", notify.Ntfy, `{"token":"tk_x"}`},
		{"ntfy no topic", notify.Ntfy, `{"url":"https://ntfy.sh/"}`},
		{"ntfy not http", notify.Ntfy, `{"url":"ntfy.sh/alerts"}`},
		{"ntfy priority too high", notify.Ntfy, `{"url":"https://ntfy.sh/alerts","priority":6}`},
		{"ntfy negative priority", notify.Ntfy, `{"url":"https://ntfy.sh/alerts","priority":-1}`},
		{"pushover malformed json", notify.Pushover, `{"token":`},
		{"pushover missing token", notify.Pushover, `{"user":"u"}`},
		{"pushover missing user", notify.Pushover, `{"token":"a"}`},
		{"pushover priority too high", notify.Pushover, `{"token":"a","user":"u","priority":3}`},
		{"pushover priority too low", notify.Pushover, `{"token":"a","user":"u","priority":-3}`},
		{"pushover retry too short", notify.Pushover, `{"token":"a","user":"u","priority":2,"retry":10}`},
		{"pushover expire too long", notify.Pushover, `{"token":"a","user":"u","priority":2,"expire":10801}`},
		{"pushover negative expire", notify.Pushover, `{"token":"a","user":"u","expire":-1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := notify.BuildProvider(c.kind, []byte(c.cfg)); err == nil {
				t.Fatalf("BuildProvider(%s, %q): expected error, got nil", c.kind, c.cfg)
			}
		})
	}
}
