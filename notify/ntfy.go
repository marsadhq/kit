package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/marsadhq/kit/urlredact"
)

// NtfyConfig is an ntfy channel's configuration.
//
// On ntfy.sh a topic is public to anyone who knows its name, so the topic is
// treated as a credential, like the access token: neither ever appears in an
// error.
type NtfyConfig struct {
	// URL is the topic's URL: https://ntfy.sh/<topic>, or the topic on a
	// self-hosted server (https://ntfy.example.com/<topic>).
	URL string `json:"url"`
	// Token is an optional access token (tk_...), sent as a Bearer token.
	Token string `json:"token"`
	// Priority is the channel's default priority, 1 (min) to 5 (max); 0
	// leaves the server's default (3). A message's own Priority wins.
	Priority int `json:"priority"`
	// Tags are optional ntfy tags; ones that name an emoji show as one.
	Tags []string `json:"tags"`
}

// NtfyProvider publishes notifications to an ntfy topic.
type NtfyProvider struct{ c NtfyConfig }

// NewNtfy constructs an NtfyProvider. BuildProvider validates the
// configuration first; a bad URL given here fails at Send.
func NewNtfy(c NtfyConfig) *NtfyProvider { return &NtfyProvider{c} }

// ntfyMaxActions is the most action buttons ntfy accepts on a message.
const ntfyMaxActions = 3

// ntfyTopicPattern is what ntfy accepts as a topic name.
var ntfyTopicPattern = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)

// ntfyMessage is ntfy's JSON publishing body.
type ntfyMessage struct {
	Topic    string       `json:"topic"`
	Message  string       `json:"message"`
	Title    string       `json:"title,omitempty"`
	Priority int          `json:"priority,omitempty"`
	Tags     []string     `json:"tags,omitempty"`
	Actions  []ntfyAction `json:"actions,omitempty"`
}

// ntfyAction is one action button: "view" opens url, "http" makes the app
// send the request.
type ntfyAction struct {
	Action  string            `json:"action"`
	Label   string            `json:"label"`
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Clear   bool              `json:"clear,omitempty"`
}

// ntfyEndpoint splits a topic URL into the URL to publish JSON to (the
// server's root, with any path prefix and query kept) and the topic. Errors
// never quote the URL.
func ntfyEndpoint(topicURL string) (publishURL, topic string, err error) {
	u, err := url.Parse(topicURL)
	if err != nil {
		return "", "", fmt.Errorf("ntfy: topic URL %w", urlredact.Error(topicURL, err))
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", errors.New("ntfy: topic URL must be http(s)://<server>/<topic>")
	}
	path := strings.TrimSuffix(u.Path, "/")
	i := strings.LastIndex(path, "/")
	topic = path[i+1:]
	if !ntfyTopicPattern.MatchString(topic) {
		return "", "", errors.New("ntfy: topic URL must end in a topic of 1 to 64 letters, digits, '-' or '_'")
	}
	u.Path, u.RawPath, u.Fragment = path[:i+1], "", ""
	return u.String(), topic, nil
}

// priority is the priority to publish m with: the message's, else the
// channel's, else none (the server default).
func (p *NtfyProvider) priority(m Message) int {
	if m.Priority >= PriorityMin && m.Priority <= PriorityUrgent {
		return int(m.Priority)
	}
	return p.c.Priority
}

// Send publishes m as JSON to the topic's server: the subject as the title
// and the body as the message (the subject alone is the message when there is
// no body). Link actions become view buttons and HTTP actions http buttons,
// at most three in all, in order; callback actions have no ntfy form and are
// left out. A refused button (no label or URL) or a 4xx other than 408 and
// 429 is a permanent error. No error names the topic, its path or the token.
func (p *NtfyProvider) Send(ctx context.Context, m Message) error {
	publishURL, topic, err := ntfyEndpoint(p.c.URL)
	if err != nil {
		return Permanent(err)
	}
	msg := ntfyMessage{Topic: topic, Message: m.Body, Title: m.Subject, Priority: p.priority(m), Tags: p.c.Tags}
	if m.Body == "" {
		msg.Message, msg.Title = m.Subject, ""
	}
	for _, a := range actionsOf(m, ActionLink, ActionHTTP) {
		if len(msg.Actions) == ntfyMaxActions {
			break
		}
		if a.Label == "" || a.URL == "" {
			return Permanent(fmt.Errorf("ntfy: action %d needs a label and a URL", len(msg.Actions)+1))
		}
		na := ntfyAction{Action: "view", Label: a.Label, URL: a.URL, Clear: a.Clear}
		if a.Kind == ActionHTTP {
			na.Action, na.Method, na.Headers, na.Body = "http", a.Method, a.Headers, a.Body
		}
		msg.Actions = append(msg.Actions, na)
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	host := urlredact.Host(publishURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, publishURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ntfy publish %w", urlredact.Error(publishURL, err))
	}
	req.Header.Set("Content-Type", "application/json")
	if p.c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.c.Token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy publish %w", urlredact.Error(publishURL, err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := httpStatusError("ntfy publish "+host, resp.StatusCode)
		// ntfy explains a refusal as {"code":40301,"http":403,"error":"..."}.
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			err = fmt.Errorf("%w: %s", err, detail(e.Error, p.c.Token, topic))
		}
		return err
	}
	return nil
}

// Compile-time assertion: NtfyProvider must satisfy the Provider interface.
var _ Provider = (*NtfyProvider)(nil)
