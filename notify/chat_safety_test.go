package notify_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/marsadhq/kit/notify"
)

// slackText sends m through a Slack provider and returns the text it posted.
func slackText(t *testing.T, m notify.Message) string {
	t.Helper()
	srv, _, _, body := captureServer(200)
	defer srv.Close()
	if err := notify.NewSlack(srv.URL).Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var got struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(*body), &got); err != nil {
		t.Fatalf("payload %s: %v", *body, err)
	}
	return got.Text
}

// discordPayload is what a Discord provider posted.
type discordPayload struct {
	Content         string `json:"content"`
	AllowedMentions *struct {
		Parse []string `json:"parse"`
	} `json:"allowed_mentions"`
}

// discordSend sends m through a Discord provider and returns the raw body and
// what it decodes to.
func discordSend(t *testing.T, m notify.Message) (string, discordPayload) {
	t.Helper()
	srv, _, _, body := captureServer(200)
	defer srv.Close()
	if err := notify.NewDiscord(srv.URL).Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var got discordPayload
	if err := json.Unmarshal([]byte(*body), &got); err != nil {
		t.Fatalf("payload %s: %v", *body, err)
	}
	return *body, got
}

// Slack reads <...> in a message's text as a mention (<!channel>, <@U123>) or
// a link with a label of the sender's choosing (<https://evil|https://good>),
// and & as the start of an entity. Escaped, they are shown as typed.
func TestSlackEscapesMentionsAndLinks(t *testing.T) {
	got := slackText(t, notify.Message{
		Subject: "Heartbeat missed: <!channel>",
		Body:    "a & b <https://evil.example|https://tend.example/runs/1> <@U123>",
	})
	want := "Heartbeat missed: &lt;!channel&gt;\na &amp; b &lt;https://evil.example|https://tend.example/runs/1&gt; &lt;@U123&gt;"
	if got != want {
		t.Errorf("text =\n%q\nwant\n%q", got, want)
	}
}

// Output (text the tool did not write) is shown after the body in a code
// block, escaped, with its backticks kept from closing the block.
func TestSlackShowsOutputInACodeBlock(t *testing.T) {
	got := slackText(t, notify.Message{
		Subject: "Job failed: backup (exit 3)",
		Body:    "Last output:",
		Output:  "```\n<!channel> <https://evil.example|re-login>\n`done`",
	})
	want := "Job failed: backup (exit 3)\nLast output:\n```\n" +
		"`​`​`​\n&lt;!channel&gt; &lt;https://evil.example|re-login&gt;\n`​done`​\n```"
	if got != want {
		t.Errorf("text =\n%q\nwant\n%q", got, want)
	}
	if strings.Count(got, "```") != 2 {
		t.Errorf("text has %d fences, want the block's own two: %q", strings.Count(got, "```"), got)
	}
}

// Discord is told to ping no one (allowed_mentions with nothing to parse), so
// @everyone, @here and user or role mentions in the text stay plain text.
// The content itself is what it was.
func TestDiscordAllowsNoMentions(t *testing.T) {
	raw, got := discordSend(t, notify.Message{Subject: "Job failed: @everyone", Body: "<@123> @here"})
	if got.Content != "Job failed: @everyone\n<@123> @here" {
		t.Errorf("content = %q", got.Content)
	}
	if got.AllowedMentions == nil || got.AllowedMentions.Parse == nil || len(got.AllowedMentions.Parse) != 0 {
		t.Errorf("allowed_mentions = %+v, want parse: []", got.AllowedMentions)
	}
	if want := `{"content":"Job failed: @everyone\n\u003c@123\u003e @here","allowed_mentions":{"parse":[]}}`; raw != want {
		t.Errorf("body =\n%s\nwant\n%s", raw, want)
	}
}

// Discord renders [label](url) as a link with the sender's label; in a code
// block it is plain text.
func TestDiscordShowsOutputInACodeBlock(t *testing.T) {
	_, got := discordSend(t, notify.Message{Subject: "Job failed: backup", Body: "Last output:",
		Output: "[re-login](https://evil.example)\n```"})
	want := "Job failed: backup\nLast output:\n```\n[re-login](https://evil.example)\n`​`​`​\n```"
	if got.Content != want {
		t.Errorf("content =\n%q\nwant\n%q", got.Content, want)
	}
}

// Discord refuses content over 2000 characters. Output too long to fit is
// cut from its start, so its end, where the error usually is, stays.
func TestDiscordFitsTheOutputInto2000Characters(t *testing.T) {
	out := strings.Repeat("é", 3000) + "\nERROR: the end"
	_, got := discordSend(t, notify.Message{Subject: "Job failed: backup", Body: "Last output:", Output: out})
	if n := utf8.RuneCountInString(got.Content); n > 2000 {
		t.Errorf("content has %d characters, want at most 2000", n)
	}
	if !strings.HasPrefix(got.Content, "Job failed: backup\nLast output:\n```\n…") || !strings.HasSuffix(got.Content, "ERROR: the end\n```") {
		t.Errorf("content = %q...%q", got.Content[:60], got.Content[len(got.Content)-40:])
	}
	// Without Output, an over-long body is cut at its end.
	_, plain := discordSend(t, notify.Message{Subject: "s", Body: strings.Repeat("x", 2500)})
	if n := utf8.RuneCountInString(plain.Content); n != 2000 || !strings.HasSuffix(plain.Content, "…") {
		t.Errorf("long body: %d characters, ends %q; want 2000 ending with …", n, plain.Content[len(plain.Content)-5:])
	}
}

// A provider that shows plain text has no form of its own for Output: the
// tool puts it in the body, after a newline.
func TestWithOutputInBody(t *testing.T) {
	m := notify.Message{Subject: "s", Body: "Last output:", Output: "line 1\nline 2"}.WithOutputInBody()
	if m.Body != "Last output:\nline 1\nline 2" || m.Output != "" || m.Subject != "s" {
		t.Errorf("message = %+v", m)
	}
	if m := (notify.Message{Output: "only"}).WithOutputInBody(); m.Body != "only" {
		t.Errorf("no body: %q", m.Body)
	}
	if m := (notify.Message{Body: "b"}).WithOutputInBody(); m.Body != "b" {
		t.Errorf("no output: %q", m.Body)
	}
}
