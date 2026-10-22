package notify

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/marsadhq/kit/urlredact"
)

// Twilio: phone calls, and text messages, through a team's own Twilio
// account, over its REST API alone. A call carries its TwiML (what it says)
// inline, so placing one needs no URL Twilio can reach; the caller polls its
// status (CallStatus) and ends it (EndCall) itself. Only a keypress
// (<Gather>) needs Twilio to reach the tool, whose request the tool checks
// with ValidTwilioSignature.

// twilioDefaultAPIBase is Twilio's REST API root.
const twilioDefaultAPIBase = "https://api.twilio.com"

// Twilio's limits.
const (
	// TwilioMaxTwiML is the longest inline TwiML a call may carry, in
	// characters (kit counts bytes, which is stricter).
	TwilioMaxTwiML = 4000
	// twilioMaxSMS is the longest text message body, in characters.
	twilioMaxSMS = 1600
	// twilioLimitedWait is how long a rate-limited request waits when
	// Twilio does not say.
	twilioLimitedWait = time.Minute
	// twilioMaxAnswer bounds how much of an answer is read.
	twilioMaxAnswer = 64 << 10
)

// The statuses of a call (Twilio's Call resource).
const (
	TwilioQueued     = "queued"      // waiting to be dialled
	TwilioRinging    = "ringing"     // ringing
	TwilioInProgress = "in-progress" // answered, going on
	TwilioCompleted  = "completed"   // answered, and over
	TwilioBusy       = "busy"        // the line was busy
	TwilioNoAnswer   = "no-answer"   // nobody answered, or the call was rejected
	TwilioFailed     = "failed"      // could not be placed (an invalid number, mostly)
	TwilioCanceled   = "canceled"    // cancelled through the API before it was answered
)

var (
	twilioAccountPattern = regexp.MustCompile(`^AC[0-9A-Za-z]{32}$`)
	twilioKeyPattern     = regexp.MustCompile(`^SK[0-9A-Za-z]{32}$`)
	twilioCallPattern    = regexp.MustCompile(`^CA[0-9a-fA-F]{32}$`)
	twilioMessagePattern = regexp.MustCompile(`^(SM|MM)[0-9a-fA-F]{32}$`)
	twilioVoicePattern   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	twilioLangPattern    = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8}){0,3}$`)
	e164Pattern          = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
)

// ValidE164 reports whether s is a phone number in E.164 form: a plus, then
// the country code and the number, 7 to 15 digits in all, no spaces or
// dashes.
func ValidE164(s string) bool { return e164Pattern.MatchString(s) }

// ValidTwilioVoice reports whether s can be a <Say> voice ("alice",
// "Polly.Joanna", "Google.en-US-Standard-C").
func ValidTwilioVoice(s string) bool { return twilioVoicePattern.MatchString(s) }

// ValidTwilioLanguage reports whether s can be a <Say> language ("en-US").
func ValidTwilioLanguage(s string) bool { return twilioLangPattern.MatchString(s) }

// ValidTwilioAccountSID reports whether s looks like an account SID: AC and
// 32 letters or digits (Twilio's are hexadecimal; the placeholder its
// documentation shows, ACXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX, passes too).
func ValidTwilioAccountSID(s string) bool { return twilioAccountPattern.MatchString(s) }

// TwilioConfig is a Twilio channel's configuration. The auth token and the
// API key's secret are credentials and never appear in an error; nor do the
// numbers and the account SID.
type TwilioConfig struct {
	// AccountSID names the account (AC...).
	AccountSID string `json:"account_sid"`
	// AuthToken is the account's auth token. It authenticates requests
	// unless an API key is set, and is what Twilio signs its own requests
	// with (TwilioSignature).
	AuthToken string `json:"auth_token"`
	// APIKeySID and APIKeySecret, when both are set, authenticate requests
	// instead of the auth token.
	APIKeySID    string `json:"api_key_sid"`
	APIKeySecret string `json:"api_key_secret"`
	// From is the Twilio number calls and texts come from, in E.164.
	From string `json:"from"`
	// To is the number Send calls, in E.164; PlaceCall and SendSMS take
	// theirs.
	To string `json:"to"`
	// Voice and Language are those of Send's <Say> (optional).
	Voice    string `json:"voice"`
	Language string `json:"language"`
	// APIBase replaces Twilio's REST API root (https://api.twilio.com): a
	// test's fake server, or an API compatible with Twilio's.
	APIBase string `json:"api_base"`
}

// validate reports the first problem with c, naming the field and never its
// value.
func (c TwilioConfig) validate() error {
	switch {
	case c.AccountSID == "":
		return errors.New("missing account_sid")
	case !twilioAccountPattern.MatchString(c.AccountSID):
		return errors.New("account_sid is not an account SID (AC and 32 letters or digits)")
	case c.APIKeySID != "" && !twilioKeyPattern.MatchString(c.APIKeySID):
		return errors.New("api_key_sid is not an API key SID (SK and 32 letters or digits)")
	case c.APIKeySID != "" && c.APIKeySecret == "":
		return errors.New("missing api_key_secret")
	case c.APIKeySID == "" && c.AuthToken == "":
		return errors.New("missing auth_token")
	case c.From == "":
		return errors.New("missing from")
	case !ValidE164(c.From):
		return errors.New("from is not a phone number in E.164 form (+ and the country code, digits only)")
	case c.To != "" && !ValidE164(c.To):
		return errors.New("to is not a phone number in E.164 form (+ and the country code, digits only)")
	case c.Voice != "" && !ValidTwilioVoice(c.Voice):
		return errors.New("voice is not a voice name (letters, digits, '.', '-' or '_')")
	case c.Language != "" && !ValidTwilioLanguage(c.Language):
		return errors.New("language is not a language tag (such as en-US)")
	}
	if c.APIBase != "" {
		u, err := url.Parse(c.APIBase)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
			!(u.Scheme == "https" || u.Scheme == "http" && loopbackHost(u.Hostname())) {
			return errors.New("api_base is not an https URL (or an http one on this machine, for tests)")
		}
	}
	return nil
}

// loopbackHost reports whether host is localhost or a loopback address.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// TwilioProvider places calls and sends texts through Twilio's REST API.
type TwilioProvider struct{ c TwilioConfig }

// NewTwilio constructs a TwilioProvider. BuildProvider validates the
// configuration first.
func NewTwilio(c TwilioConfig) *TwilioProvider { return &TwilioProvider{c} }

// TwilioCall is what Twilio says about one call.
type TwilioCall struct {
	SID    string
	Status string // one of the Twilio* statuses (another, should Twilio add one)
	// Duration is how long the answered call lasted, once it is over.
	Duration time.Duration
	// AnsweredBy is "human" or "machine..." only when answering machine
	// detection was asked for (kit never asks); "" otherwise.
	AnsweredBy string
	// EndedNow, in what EndCall returns, reports that this EndCall ended
	// the call (false: it was over already, by itself).
	EndedNow bool
}

// Ended reports whether the call is over: completed, busy, no-answer,
// failed or canceled.
func (c TwilioCall) Ended() bool {
	switch c.Status {
	case TwilioCompleted, TwilioBusy, TwilioNoAnswer, TwilioFailed, TwilioCanceled:
		return true
	}
	return false
}

// Send calls the configured To and says m: its subject, then its body, on
// one line each and without their links, once, after a one-second pause.
// Without a To it is permanent. See PlaceCall for the rest.
func (p *TwilioProvider) Send(ctx context.Context, m Message) error {
	if p.c.To == "" {
		return Permanent(errors.New("twilio: no number to call"))
	}
	text := SpeakableText(m.Subject, 300)
	if body := SpeakableText(m.WithOutputInBody().Body, 1000); body != "" {
		if r, _ := utf8.DecodeLastRuneInString(text); text != "" && !strings.ContainsRune(".!?…", r) {
			text += "."
		}
		text = strings.TrimSpace(text + " " + body)
	}
	// What the call says must fit Twilio's limit once escaped, however
	// much of it is markup: cut it to the room the rest leaves.
	room := TwilioMaxTwiML - len(NewTwiML().Pause(1).Say("x", p.c.Voice, p.c.Language).String()) + len("x")
	twiml := NewTwiML().Pause(1).Say(FitTwiMLText(text, room), p.c.Voice, p.c.Language).String()
	_, err := p.PlaceCall(ctx, p.c.To, twiml)
	return err
}

// FitTwiMLText returns text cut so that, escaped as TwiML text (Say does),
// it takes at most room bytes; a cut is marked "…".
func FitTwiMLText(text string, room int) string {
	if len(xmlText(text)) <= room {
		return text
	}
	const mark = "…"
	used := len(mark)
	var b strings.Builder
	for _, r := range text {
		w := len(xmlText(string(r)))
		if used+w > room {
			break
		}
		used += w
		b.WriteRune(r)
	}
	if b.Len() == 0 && used > room {
		return ""
	}
	return strings.TrimRight(b.String(), " ") + mark
}

// PlaceCall calls to (E.164) from the configured number, the call doing what
// twiml says (inline, at most TwilioMaxTwiML), and returns the call's SID:
// what CallStatus and EndCall take. A 2xx answer that names no call SID was
// a call placed all the same: "" and no error. A number that is not E.164,
// no TwiML or too much of it is refused before any request.
//
// A 4xx answer is permanent, except 429, which asks to wait its Retry-After
// (a minute when it gives none); a 5xx, an answer that never came or a
// transport failure is transient: the call may have been placed. StatusCode
// reads an answer's status. No error carries the auth token, the numbers or
// the account SID.
func (p *TwilioProvider) PlaceCall(ctx context.Context, to, twiml string) (string, error) {
	switch {
	case !ValidE164(to):
		return "", Permanent(errors.New("twilio: the number to call is not in E.164 form"))
	case strings.TrimSpace(twiml) == "":
		return "", Permanent(errors.New("twilio: a call needs TwiML"))
	case len(twiml) > TwilioMaxTwiML:
		return "", Permanent(fmt.Errorf("twilio: the call's TwiML is %d bytes, over Twilio's limit of %d characters (kit counts bytes)",
			len(twiml), TwilioMaxTwiML))
	}
	v := url.Values{}
	v.Set("To", to)
	v.Set("From", p.c.From)
	v.Set("Twiml", twiml)
	var answer struct {
		SID string `json:"sid"`
	}
	if err := p.do(ctx, http.MethodPost, "Calls.json", "twilio calls", v, &answer, to); err != nil {
		return "", err
	}
	if !twilioCallPattern.MatchString(answer.SID) {
		return "", nil
	}
	return answer.SID, nil
}

// errTwilioCallSID is the refusal of a call SID that is not one: it would go
// into the URL path.
var errTwilioCallSID = errors.New("twilio: a call SID is CA and 32 hexadecimal digits")

// CallStatus reads the call sid names (GET Calls/{sid}.json). Its errors are
// classified as PlaceCall's; a SID that is not one is refused before any
// request.
func (p *TwilioProvider) CallStatus(ctx context.Context, sid string) (TwilioCall, error) {
	if !twilioCallPattern.MatchString(sid) {
		return TwilioCall{}, Permanent(errTwilioCallSID)
	}
	return p.call(ctx, http.MethodGet, sid, nil)
}

// EndCall ends the call sid names, as it stands: a queued or ringing call is
// cancelled (Status=canceled, which leaves a call already answered alone),
// an answered one is hung up (Status=completed), and one that is over is
// left as it is. A call answered between the read and the cancel is then
// hung up; a refusal of the update for a call that, read again, is over is
// no failure. It returns the call as the last answer showed it, with
// EndedNow set when this ended it. Its errors are classified as
// PlaceCall's.
func (p *TwilioProvider) EndCall(ctx context.Context, sid string) (TwilioCall, error) {
	if !twilioCallPattern.MatchString(sid) {
		return TwilioCall{}, Permanent(errTwilioCallSID)
	}
	c, err := p.call(ctx, http.MethodGet, sid, nil)
	if err != nil || c.Ended() {
		return c, err
	}
	status := TwilioCompleted
	if c.Status == TwilioQueued || c.Status == TwilioRinging {
		status = TwilioCanceled
	}
	for range 2 {
		v := url.Values{}
		v.Set("Status", status)
		next, err := p.call(ctx, http.MethodPost, sid, v)
		if err != nil {
			if _, answered := StatusCode(err); answered && IsPermanent(err) {
				// Over between the read and the update?
				if again, rerr := p.call(ctx, http.MethodGet, sid, nil); rerr == nil && again.Ended() {
					return again, nil
				}
			}
			return c, err
		}
		c = next
		c.EndedNow = c.Ended()
		if c.Ended() || status == TwilioCompleted || c.Status != TwilioInProgress {
			return c, nil
		}
		// Answered between the read and the cancel: hang it up.
		status = TwilioCompleted
	}
	return c, nil
}

// call reads (GET) or updates (POST form) the call resource of sid.
func (p *TwilioProvider) call(ctx context.Context, method, sid string, form url.Values) (TwilioCall, error) {
	what := "twilio call status"
	if method == http.MethodPost {
		what = "twilio call update"
	}
	var answer struct {
		SID        string          `json:"sid"`
		Status     string          `json:"status"`
		Duration   json.RawMessage `json:"duration"`
		AnsweredBy *string         `json:"answered_by"`
	}
	if err := p.do(ctx, method, "Calls/"+sid+".json", what, form, &answer); err != nil {
		return TwilioCall{}, err
	}
	c := TwilioCall{SID: sid, Status: answer.Status}
	if answer.AnsweredBy != nil {
		c.AnsweredBy = *answer.AnsweredBy
	}
	// Twilio writes the duration as a string of seconds, null until the
	// call is over.
	raw := strings.Trim(string(answer.Duration), `"`)
	if s, err := strconv.Atoi(raw); err == nil && s > 0 {
		c.Duration = time.Duration(s) * time.Second
	}
	return c, nil
}

// SendSMS texts body (at most 1600 characters) to to (E.164) from the
// configured number and returns the message's SID ("" when the answer names
// none). Its errors are classified as PlaceCall's. (In the US, texting needs
// the sender registered for A2P 10DLC first.)
func (p *TwilioProvider) SendSMS(ctx context.Context, to, body string) (string, error) {
	switch {
	case !ValidE164(to):
		return "", Permanent(errors.New("twilio: the number to text is not in E.164 form"))
	case strings.TrimSpace(body) == "":
		return "", Permanent(errors.New("twilio: a text needs a body"))
	case utf8.RuneCountInString(body) > twilioMaxSMS:
		return "", Permanent(fmt.Errorf("twilio: a text is at most %d characters", twilioMaxSMS))
	}
	v := url.Values{}
	v.Set("To", to)
	v.Set("From", p.c.From)
	v.Set("Body", body)
	var answer struct {
		SID string `json:"sid"`
	}
	if err := p.do(ctx, http.MethodPost, "Messages.json", "twilio messages", v, &answer, to); err != nil {
		return "", err
	}
	if !twilioMessagePattern.MatchString(answer.SID) {
		return "", nil
	}
	return answer.SID, nil
}

// do sends one request to the account's resource path (under
// /2010-04-01/Accounts/{sid}/), a form for a POST, and decodes a 2xx answer
// into out. A failure is classified for the worker; also are the numbers in
// extra kept out of any error.
func (p *TwilioProvider) do(ctx context.Context, method, path, what string, form url.Values, out any, extra ...string) error {
	base := p.c.APIBase
	if base == "" {
		base = twilioDefaultAPIBase
	}
	rawURL := strings.TrimRight(base, "/") + "/2010-04-01/Accounts/" + p.c.AccountSID + "/" + path
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return fmt.Errorf("%s %w", what, urlredact.Error(rawURL, err))
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json")
	user, pass := p.c.AccountSID, p.c.AuthToken
	if p.c.APIKeySID != "" {
		user, pass = p.c.APIKeySID, p.c.APIKeySecret
	}
	req.SetBasicAuth(user, pass)
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %w", what, urlredact.Error(rawURL, err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, twilioMaxAnswer))
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_ = json.Unmarshal(raw, out) // an unreadable answer leaves out empty
		return nil
	}
	var refusal struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &refusal)
	var cause error = &statusError{what: what + " " + urlredact.Host(rawURL), code: resp.StatusCode}
	if refusal.Code != 0 || refusal.Message != "" {
		secrets := append([]string{p.c.AuthToken, p.c.APIKeySecret, p.c.AccountSID, p.c.APIKeySID, p.c.From, p.c.To}, extra...)
		cause = fmt.Errorf("%w: %d %s", cause, refusal.Code, detail(refusal.Message, secrets...))
	}
	switch code := resp.StatusCode; {
	case code == http.StatusTooManyRequests:
		return RetryLater(cause, retryAfterHeader(resp.Header, twilioLimitedWait))
	case code == http.StatusRequestTimeout:
		return cause
	case code >= 400 && code < 500:
		return Permanent(cause)
	}
	return cause
}

// TwilioGather is a <Gather> of keypresses: Twilio POSTs the digits pressed
// to Action (an absolute URL Twilio can reach), signed (TwilioSignature).
type TwilioGather struct {
	Action    string
	NumDigits int // how many digits end the gather (0: Twilio's default, '#')
	Timeout   int // seconds to wait for a digit after what it says (0: Twilio's 5)
}

// TwiML builds the TwiML of a call: what it says, its pauses and the
// keypresses it gathers, in order. Every text and attribute is escaped, so
// text written by whoever sent an alert adds no verb. String renders the
// document.
type TwiML struct{ b strings.Builder }

// NewTwiML starts an empty document.
func NewTwiML() *TwiML { return &TwiML{} }

// xmlEscape escapes s for XML text or an attribute value.
func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Say says text (left out when blank) in voice and language (each left to
// Twilio when "").
func (t *TwiML) Say(text, voice, language string) *TwiML {
	text = strings.TrimSpace(text)
	if text == "" {
		return t
	}
	t.b.WriteString("<Say")
	if voice != "" {
		t.b.WriteString(` voice="` + xmlEscape(voice) + `"`)
	}
	if language != "" {
		t.b.WriteString(` language="` + xmlEscape(language) + `"`)
	}
	t.b.WriteString(">" + xmlText(text) + "</Say>")
	return t
}

// xmlText escapes s as XML text: '&' and '<', and '>' where it would close
// "]]>"; characters XML cannot hold become U+FFFD, as encoding/xml does.
// Quotes need no escaping in text, which keeps escaped text short (an
// attribute is escaped in full, xmlEscape).
func xmlText(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>' && strings.HasSuffix(s[:i], "]]"):
			b.WriteString("&gt;")
		case !xmlChar(r): // an invalid byte decodes as U+FFFD, which stays
			b.WriteRune('\uFFFD')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// xmlChar reports whether r may appear in an XML 1.0 document.
func xmlChar(r rune) bool {
	return r == 0x09 || r == 0x0A || r == 0x0D || r >= 0x20 && r <= 0xD7FF || r >= 0xE000 && r <= 0xFFFD || r >= 0x10000 && r <= 0x10FFFF
}

// Pause waits seconds (left out when not positive).
func (t *TwiML) Pause(seconds int) *TwiML {
	if seconds > 0 {
		t.b.WriteString(`<Pause length="` + strconv.Itoa(seconds) + `"/>`)
	}
	return t
}

// Gather gathers keypresses (DTMF) while what inside says plays: inside may
// Say and Pause. A call nobody presses a key on goes on after it.
func (t *TwiML) Gather(g TwilioGather, inside func(*TwiML)) *TwiML {
	t.b.WriteString(`<Gather input="dtmf"`)
	if g.NumDigits > 0 {
		t.b.WriteString(` numDigits="` + strconv.Itoa(g.NumDigits) + `"`)
	}
	if g.Timeout > 0 {
		t.b.WriteString(` timeout="` + strconv.Itoa(g.Timeout) + `"`)
	}
	t.b.WriteString(` action="` + xmlEscape(g.Action) + `" method="POST">`)
	if inside != nil {
		in := NewTwiML()
		inside(in)
		t.b.WriteString(in.b.String())
	}
	t.b.WriteString("</Gather>")
	return t
}

// String renders the document.
func (t *TwiML) String() string {
	return `<?xml version="1.0" encoding="UTF-8"?><Response>` + t.b.String() + `</Response>`
}

// speechLinks are what a voice must not read out: URLs.
var speechLinks = regexp.MustCompile(`(?i)\b(?:[a-z][a-z0-9+.-]*://|www\.)\S+`)

// SpeakableText returns s fit to be read out: links replaced by "a link"
// (a voice spelling a URL is noise), control characters dropped, on one
// line, and cut at a word to at most max characters, the cut marked "…".
func SpeakableText(s string, max int) string {
	s = speechLinks.ReplaceAllString(s, "a link")
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r), r == utf8.RuneError:
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	if max <= 1 {
		return ""
	}
	cut := rs[:max-1]
	if rs[max-1] != ' ' {
		// Cut at the last word that fits whole, when there is one.
		for i := len(cut) - 1; i > 0; i-- {
			if cut[i] == ' ' {
				cut = cut[:i]
				break
			}
		}
	}
	return strings.TrimRight(string(cut), " ") + "…"
}

// Compile-time assertion: TwilioProvider must satisfy the Provider interface.
var _ Provider = (*TwilioProvider)(nil)
