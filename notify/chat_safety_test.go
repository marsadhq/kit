package notify_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"
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
	if got.Content != "Job failed: @everyone\n\\<@123\\> @here" {
		t.Errorf("content = %q", got.Content)
	}
	if got.AllowedMentions == nil || got.AllowedMentions.Parse == nil || len(got.AllowedMentions.Parse) != 0 {
		t.Errorf("allowed_mentions = %+v, want parse: []", got.AllowedMentions)
	}
	if want := `{"content":"Job failed: @everyone\n\\\u003c@123\\\u003e @here","allowed_mentions":{"parse":[]}}`; raw != want {
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

// Discord renders [label](url) as a link showing a label of the sender's
// choosing, and <@123>, <#123> and <@&123> as mentions. Subject and body text
// can come from anyone (an alert's title, a job's name), so the characters
// those are built from (\ [ ] < >) are backslash-escaped there: Discord shows
// them as typed, and a plain URL stays a link to itself.
func TestDiscordEscapesLinksAndMentions(t *testing.T) {
	_, got := discordSend(t, notify.Message{
		Subject: "Heartbeat missed: [re-login](https://evil.example)",
		Body:    `<#42> \[x\] see https://tend.example/runs/1`,
	})
	want := `Heartbeat missed: \[re-login\](https://evil.example)` + "\n" + `\<#42\> \\\[x\\\] see https://tend.example/runs/1`
	if got.Content != want {
		t.Errorf("content =\n%q\nwant\n%q", got.Content, want)
	}
}

// utf16Len is how Discord counts a message's length.
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// Discord counts its 2000-character limit in UTF-16 code units, so a
// character outside the Basic Multilingual Plane (most emoji) counts twice.
func TestDiscordCountsUTF16(t *testing.T) {
	_, got := discordSend(t, notify.Message{Subject: "s", Body: "b", Output: strings.Repeat("😀", 1500) + "end"})
	if n := utf16Len(got.Content); n > 2000 || !strings.HasSuffix(got.Content, "end\n```") {
		t.Errorf("content: %d UTF-16 units, ends %q; want at most 2000 and the output's end", n, got.Content[len(got.Content)-12:])
	}
	_, plain := discordSend(t, notify.Message{Subject: "s", Body: strings.Repeat("😀", 1500)})
	if n := utf16Len(plain.Content); n > 2000 {
		t.Errorf("long body: %d UTF-16 units, want at most 2000", n)
	}
}

// DiscordOutputRoom and DiscordOutputLength let a tool fit its output itself
// (and choose what to keep): output whose length is the room is sent whole;
// one more and the provider cuts it from its start.
func TestDiscordOutputRoom(t *testing.T) {
	if n := notify.DiscordOutputLength("`é😀"); n != 5 {
		t.Errorf("DiscordOutputLength = %d, want 5 (a backtick is sent with a zero-width space, 😀 is two units)", n)
	}
	m := notify.Message{Subject: "Job failed: [x]", Body: strings.Repeat("b", 1500)}
	room := notify.DiscordOutputRoom(m)
	out := strings.Repeat("a`", room/3) + strings.Repeat("c", room%3)
	if notify.DiscordOutputLength(out) != room {
		t.Fatalf("test output length %d, want %d", notify.DiscordOutputLength(out), room)
	}
	m.Output = out
	if _, got := discordSend(t, m); !strings.Contains(got.Content, "\n```\n"+strings.ReplaceAll(out, "`", "`\u200b")+"\n```") || utf16Len(got.Content) != 2000 {
		t.Errorf("output that fits the room was not sent whole (%d units)", utf16Len(got.Content))
	}
	m.Output = out + "d"
	if _, got := discordSend(t, m); !strings.Contains(got.Content, "\n```\n…") || utf16Len(got.Content) > 2000 {
		t.Errorf("output one unit over the room was not cut from its start")
	}
}

// A subject or body with backticks in it (a job named "nightly ```") must not
// open or close a code block: the output's block would then end early and
// what follows render as markup. Backticks are escaped there too.
func TestDiscordEscapesBackticks(t *testing.T) {
	_, got := discordSend(t, notify.Message{Subject: "Job failed: nightly ```", Body: "Last output:",
		Output: "[re-login](https://evil.example)"})
	want := "Job failed: nightly \\`\\`\\`\nLast output:\n```\n[re-login](https://evil.example)\n```"
	if got.Content != want {
		t.Errorf("content =\n%q\nwant\n%q", got.Content, want)
	}
}

// A URL is left as it is, so it stays a working link: escaping the brackets
// of an IPv6 host or a query would break it. Discord reads a URL as a link to
// itself, so no masked link can form inside one.
func TestDiscordLeavesURLsAsTheyAre(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"https://[2001:db8::1]:8080/runs/17", "https://[2001:db8::1]:8080/runs/17"},
		{"http://tend.example/runs?x[a]=1 and [x]", `http://tend.example/runs?x[a]=1 and \[x\]`},
		{"[re-login](https://evil.example) see http://[2001:db8::1]/heartbeats/4",
			`\[re-login\](https://evil.example) see http://[2001:db8::1]/heartbeats/4`},
		{"<https://tend.example/runs/1>", `\<https://tend.example/runs/1\>`},
		{"https://tend.example/```", "https://tend.example/\\`\\`\\`"},
	} {
		_, got := discordSend(t, notify.Message{Subject: "s", Body: tc.body})
		if got.Content != "s\n"+tc.want {
			t.Errorf("body %q: content %q, want %q", tc.body, got.Content, "s\n"+tc.want)
		}
	}
}
