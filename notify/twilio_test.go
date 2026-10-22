package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// Fictional values: a 555-01xx number, made-up SIDs and a token.
const (
	testAccountSID = "ACtest0000000000000000000000000000"
	testAuthToken  = "test-auth-token-0000"
	testCallSID    = "CAfedcba9876543210fedcba9876543210"
	testFrom       = "+15550100"
	testTo         = "+15550142"
)

// testKeySID is a made-up API key SID, with letters no real one has.
const testKeySID = "SKtestkey0000000000000000000000000"

// twilioRequest is one request the fake Twilio API received.
type twilioRequest struct {
	method, path, user, pass, ctype string
	form                            url.Values
}

// twilioAnswer is what the fake answers one request with.
type twilioAnswer struct {
	status int
	header http.Header
	body   string
}

// fakeTwilio is Twilio's REST API: it records every request and answers
// each with what answer returns for it.
type fakeTwilio struct {
	mu   sync.Mutex
	got  []twilioRequest
	srv  *httptest.Server
	next func(r twilioRequest) twilioAnswer
}

func newFakeTwilio(t *testing.T, answer func(r twilioRequest) twilioAnswer) *fakeTwilio {
	t.Helper()
	f := &fakeTwilio{next: answer}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(b))
		user, pass, _ := r.BasicAuth()
		req := twilioRequest{method: r.Method, path: r.URL.Path, user: user, pass: pass, ctype: r.Header.Get("Content-Type"), form: form}
		f.mu.Lock()
		f.got = append(f.got, req)
		answer := f.next
		f.mu.Unlock()
		a := answer(req)
		for k, vs := range a.header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(a.body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTwilio) requests() []twilioRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]twilioRequest(nil), f.got...)
}

// provider is a Twilio provider that talks to the fake.
func (f *fakeTwilio) provider(tweak ...func(*TwilioConfig)) *TwilioProvider {
	c := TwilioConfig{AccountSID: testAccountSID, AuthToken: testAuthToken, From: testFrom, APIBase: f.srv.URL}
	for _, fn := range tweak {
		fn(&c)
	}
	return NewTwilio(c)
}

func callJSON(status string) string {
	return `{"sid":"` + testCallSID + `","account_sid":"` + testAccountSID + `","to":"` + testTo + `","from":"` + testFrom +
		`","status":"` + status + `","direction":"outbound-api","duration":null}`
}

const callsPath = "/2010-04-01/Accounts/" + testAccountSID + "/Calls.json"
const callPath = "/2010-04-01/Accounts/" + testAccountSID + "/Calls/" + testCallSID + ".json"

// TestTwilioPlaceCall: a call is a form POST of From, To and the inline
// TwiML to the account's Calls resource, authenticated with the account SID
// and auth token; the answer's SID is returned.
func TestTwilioPlaceCall(t *testing.T) {
	f := newFakeTwilio(t, func(twilioRequest) twilioAnswer { return twilioAnswer{status: 201, body: callJSON("queued")} })
	twiml := `<?xml version="1.0" encoding="UTF-8"?><Response><Say>Disk full</Say></Response>`
	sid, err := f.provider().PlaceCall(context.Background(), testTo, twiml)
	if err != nil {
		t.Fatalf("PlaceCall: %v", err)
	}
	if sid != testCallSID {
		t.Fatalf("sid = %q, want %q", sid, testCallSID)
	}
	got := f.requests()
	if len(got) != 1 {
		t.Fatalf("%d requests, want 1", len(got))
	}
	r := got[0]
	if r.method != http.MethodPost || r.path != callsPath || r.ctype != "application/x-www-form-urlencoded" {
		t.Fatalf("request = %s %s (%s)", r.method, r.path, r.ctype)
	}
	if r.user != testAccountSID || r.pass != testAuthToken {
		t.Fatalf("basic auth = %q:%q", r.user, r.pass)
	}
	want := url.Values{"From": {testFrom}, "To": {testTo}, "Twiml": {twiml}}
	if r.form.Encode() != want.Encode() {
		t.Fatalf("form = %v, want %v", r.form, want)
	}
}

// TestTwilioAPIKeyAuth: with an API key the key SID and its secret
// authenticate; the account SID still names the account in the path.
func TestTwilioAPIKeyAuth(t *testing.T) {
	f := newFakeTwilio(t, func(twilioRequest) twilioAnswer { return twilioAnswer{status: 201, body: callJSON("queued")} })
	p := f.provider(func(c *TwilioConfig) {
		c.AuthToken, c.APIKeySID, c.APIKeySecret = "", testKeySID, "key-secret"
	})
	if _, err := p.PlaceCall(context.Background(), testTo, "<Response/>"); err != nil {
		t.Fatal(err)
	}
	r := f.requests()[0]
	if r.user != testKeySID || r.pass != "key-secret" || r.path != callsPath {
		t.Fatalf("auth %q:%q on %s", r.user, r.pass, r.path)
	}
}

// TestTwilioPlaceCallRefusesBeforeAnyRequest: a number that is not E.164,
// no TwiML, and TwiML over Twilio's 4000 characters are permanent, and
// nothing is sent.
func TestTwilioPlaceCallRefusesBeforeAnyRequest(t *testing.T) {
	f := newFakeTwilio(t, func(twilioRequest) twilioAnswer { return twilioAnswer{status: 201, body: callJSON("queued")} })
	p := f.provider()
	long := "<Response><Say>" + strings.Repeat("a", TwilioMaxTwiML) + "</Say></Response>"
	for _, c := range []struct{ to, twiml string }{
		{"5550142", "<Response/>"},
		{"+1 555 0142", "<Response/>"},
		{"+0123456", "<Response/>"},
		{"", "<Response/>"},
		{testTo, ""},
		{testTo, long},
	} {
		if _, err := p.PlaceCall(context.Background(), c.to, c.twiml); err == nil || !IsPermanent(err) {
			t.Errorf("to %q, %d characters of TwiML: err = %v, want permanent", c.to, len(c.twiml), err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests for refused calls", n)
	}
	exact := "<Response><Say>" + strings.Repeat("a", TwilioMaxTwiML-len("<Response><Say></Say></Response>")) + "</Say></Response>"
	if _, err := p.PlaceCall(context.Background(), testTo, exact); err != nil {
		t.Fatalf("TwiML of exactly %d characters: %v", len(exact), err)
	}
}

// TestTwilioRefusals: a 4xx is permanent except 429, which asks to wait its
// Retry-After (a minute without one); a 5xx and a transport failure are
// transient. Twilio's message is kept, but never the token, the numbers or
// the account SID it may echo.
func TestTwilioRefusals(t *testing.T) {
	for _, c := range []struct {
		name      string
		status    int
		header    http.Header
		body      string
		permanent bool
		wait      time.Duration
		mention   string
	}{
		{"bad number", 400, nil, `{"code":21211,"message":"The 'To' number ` + testTo + ` is not a valid phone number.","status":400}`,
			true, 0, "21211"},
		{"bad credentials", 401, nil, `{"code":20003,"message":"Authenticate","status":401}`, true, 0, "status 401"},
		{"echoes", 400, nil, `{"code":21210,"message":"From ` + testFrom + ` of ` + testAccountSID + ` with ` + testAuthToken + `","status":400}`,
			true, 0, "21210"},
		{"rate limited", 429, http.Header{"Retry-After": {"30"}}, `{"code":20429,"message":"Too Many Requests","status":429}`,
			false, 30 * time.Second, "status 429"},
		{"rate limited, no header", 429, nil, `{"code":20429,"message":"Too Many Requests","status":429}`, false, time.Minute, "status 429"},
		{"server error", 503, nil, `oops`, false, 0, "status 503"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeTwilio(t, func(twilioRequest) twilioAnswer { return twilioAnswer{c.status, c.header, c.body} })
			_, err := f.provider().PlaceCall(context.Background(), testTo, "<Response/>")
			if err == nil {
				t.Fatal("err = nil")
			}
			if IsPermanent(err) != c.permanent {
				t.Errorf("IsPermanent = %v, want %v (%v)", IsPermanent(err), c.permanent, err)
			}
			if after, ok := RetryAfter(err); c.wait > 0 && (!ok || after != c.wait) {
				t.Errorf("RetryAfter = %v, %v; want %v", after, ok, c.wait)
			}
			if code, ok := StatusCode(err); !ok || code != c.status {
				t.Errorf("StatusCode = %d, %v; want %d", code, ok, c.status)
			}
			msg := err.Error()
			if !strings.Contains(msg, c.mention) {
				t.Errorf("error %q does not mention %q", msg, c.mention)
			}
			for _, secret := range []string{testAuthToken, testTo, testFrom, testAccountSID} {
				if strings.Contains(msg, secret) {
					t.Errorf("error %q quotes %q", msg, secret)
				}
			}
		})
	}

	// A transport failure is transient and quotes neither the token nor the
	// account.
	p := NewTwilio(TwilioConfig{AccountSID: testAccountSID, AuthToken: testAuthToken, From: testFrom, APIBase: "http://127.0.0.1:1"})
	_, err := p.PlaceCall(context.Background(), testTo, "<Response/>")
	if err == nil || IsPermanent(err) {
		t.Fatalf("transport error = %v, want a transient one", err)
	}
	if _, answered := StatusCode(err); answered {
		t.Fatalf("transport error %v has a status", err)
	}
	for _, secret := range []string{testAuthToken, testAccountSID, testTo} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("transport error %q quotes %q", err, secret)
		}
	}
}

// TestTwilioPlaceCallAnswerWithoutSID: a 2xx without a call SID was still
// placed: no error, and no SID.
func TestTwilioPlaceCallAnswerWithoutSID(t *testing.T) {
	f := newFakeTwilio(t, func(twilioRequest) twilioAnswer { return twilioAnswer{status: 201, body: `{"sid":"../x"}`} })
	sid, err := f.provider().PlaceCall(context.Background(), testTo, "<Response/>")
	if err != nil || sid != "" {
		t.Fatalf("PlaceCall = %q, %v; want no SID and no error", sid, err)
	}
}

// TestTwilioCallStatus: a GET of the call resource; its status and, once it
// ended, its duration (Twilio writes it as a string).
func TestTwilioCallStatus(t *testing.T) {
	answer := `{"sid":"` + testCallSID + `","status":"completed","duration":"34","answered_by":null}`
	f := newFakeTwilio(t, func(twilioRequest) twilioAnswer { return twilioAnswer{status: 200, body: answer} })
	c, err := f.provider().CallStatus(context.Background(), testCallSID)
	if err != nil {
		t.Fatal(err)
	}
	if c.SID != testCallSID || c.Status != TwilioCompleted || c.Duration != 34*time.Second || !c.Ended() {
		t.Fatalf("call = %+v", c)
	}
	r := f.requests()[0]
	if r.method != http.MethodGet || r.path != callPath || r.user != testAccountSID || r.pass != testAuthToken {
		t.Fatalf("request = %s %s as %q", r.method, r.path, r.user)
	}
	for _, s := range []string{TwilioQueued, TwilioRinging, TwilioInProgress, "initiated"} {
		if (TwilioCall{Status: s}).Ended() {
			t.Errorf("%s counts as ended", s)
		}
	}
	for _, s := range []string{TwilioCompleted, TwilioBusy, TwilioNoAnswer, TwilioFailed, TwilioCanceled} {
		if !(TwilioCall{Status: s}).Ended() {
			t.Errorf("%s does not count as ended", s)
		}
	}
	// A number as Twilio might also write it.
	answer = `{"sid":"` + testCallSID + `","status":"completed","duration":7}`
	if c, err := f.provider().CallStatus(context.Background(), testCallSID); err != nil || c.Duration != 7*time.Second {
		t.Fatalf("numeric duration: %+v, %v", c, err)
	}
	// A SID that is not one goes into no URL path.
	before := len(f.requests())
	for _, sid := range []string{"", "../Calls", "CA123", "SMfedcba9876543210fedcba9876543210"} {
		if _, err := f.provider().CallStatus(context.Background(), sid); err == nil || !IsPermanent(err) {
			t.Errorf("sid %q: err = %v, want permanent", sid, err)
		}
		if _, err := f.provider().EndCall(context.Background(), sid); err == nil || !IsPermanent(err) {
			t.Errorf("end sid %q: err = %v, want permanent", sid, err)
		}
	}
	if len(f.requests()) != before {
		t.Fatal("a bad SID was sent")
	}
}

// TestTwilioEndCall: a queued or ringing call is cancelled (Status=canceled,
// which leaves a call already answered alone), an answered one hung up
// (Status=completed), and one that already ended is left as it is.
func TestTwilioEndCall(t *testing.T) {
	for _, c := range []struct {
		name    string
		before  string
		updates []string // the Status of each update, in order
		after   string
	}{
		{"queued", TwilioQueued, []string{"canceled"}, TwilioCanceled},
		{"ringing", TwilioRinging, []string{"canceled"}, TwilioCanceled},
		{"in progress", TwilioInProgress, []string{"completed"}, TwilioCompleted},
		{"over", TwilioNoAnswer, nil, TwilioNoAnswer},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeTwilio(t, func(r twilioRequest) twilioAnswer {
				if r.method == http.MethodGet {
					return twilioAnswer{status: 200, body: callJSON(c.before)}
				}
				return twilioAnswer{status: 200, body: callJSON(c.after)}
			})
			got, err := f.provider().EndCall(context.Background(), testCallSID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != c.after || got.EndedNow != (len(c.updates) > 0) {
				t.Fatalf("status = %q, EndedNow = %v; want %q, %v", got.Status, got.EndedNow, c.after, len(c.updates) > 0)
			}
			var updates []string
			for _, r := range f.requests() {
				if r.path != callPath {
					t.Fatalf("request to %s", r.path)
				}
				if r.method == http.MethodPost {
					if len(r.form) != 1 {
						t.Fatalf("update form = %v, want Status alone", r.form)
					}
					updates = append(updates, r.form.Get("Status"))
				}
			}
			if strings.Join(updates, ",") != strings.Join(c.updates, ",") {
				t.Fatalf("updates = %v, want %v", updates, c.updates)
			}
		})
	}
}

// TestTwilioEndCallAnsweredMeanwhile: a call answered between the read and
// the cancel is not affected by it (Twilio answers in-progress), so it is
// hung up.
func TestTwilioEndCallAnsweredMeanwhile(t *testing.T) {
	f := newFakeTwilio(t, func(r twilioRequest) twilioAnswer {
		switch {
		case r.method == http.MethodGet:
			return twilioAnswer{status: 200, body: callJSON(TwilioRinging)}
		case r.form.Get("Status") == "canceled":
			return twilioAnswer{status: 200, body: callJSON(TwilioInProgress)}
		}
		return twilioAnswer{status: 200, body: callJSON(TwilioCompleted)}
	})
	got, err := f.provider().EndCall(context.Background(), testCallSID)
	if err != nil || got.Status != TwilioCompleted || !got.EndedNow {
		t.Fatalf("EndCall = %+v, %v", got, err)
	}
	var updates []string
	for _, r := range f.requests() {
		if r.method == http.MethodPost {
			updates = append(updates, r.form.Get("Status"))
		}
	}
	if strings.Join(updates, ",") != "canceled,completed" {
		t.Fatalf("updates = %v", updates)
	}
}

// TestTwilioEndCallEndedMeanwhile: a call that ended between the read and
// the update is refused by Twilio; read again, it is over: that is no
// failure. A refusal for a call still going is.
func TestTwilioEndCallEndedMeanwhile(t *testing.T) {
	reads := 0
	f := newFakeTwilio(t, func(r twilioRequest) twilioAnswer {
		if r.method == http.MethodGet {
			reads++
			if reads == 1 {
				return twilioAnswer{status: 200, body: callJSON(TwilioRinging)}
			}
			return twilioAnswer{status: 200, body: callJSON(TwilioNoAnswer)}
		}
		return twilioAnswer{status: 400, body: `{"code":21220,"message":"Call is not in-progress. Cannot redirect.","status":400}`}
	})
	got, err := f.provider().EndCall(context.Background(), testCallSID)
	if err != nil || got.Status != TwilioNoAnswer || got.EndedNow {
		t.Fatalf("EndCall = %+v, %v; want the call's own end", got, err)
	}

	f = newFakeTwilio(t, func(r twilioRequest) twilioAnswer {
		if r.method == http.MethodGet {
			return twilioAnswer{status: 200, body: callJSON(TwilioRinging)}
		}
		return twilioAnswer{status: 400, body: `{"code":21220,"message":"no","status":400}`}
	})
	if _, err := f.provider().EndCall(context.Background(), testCallSID); err == nil || !IsPermanent(err) {
		t.Fatalf("a refused end of a ringing call: err = %v, want permanent", err)
	}
}

// TestTwilioSend: Send calls the configured number and says the subject and
// the body, links left out; without a number to call it is permanent.
func TestTwilioSend(t *testing.T) {
	f := newFakeTwilio(t, func(twilioRequest) twilioAnswer { return twilioAnswer{status: 201, body: callJSON("queued")} })
	m := Message{Subject: "Ward test message", Body: "Nothing is wrong.\nhttps://ward.example.com/x"}
	if err := f.provider(func(c *TwilioConfig) { c.To, c.Voice, c.Language = testTo, "Polly.Joanna", "en-US" }).Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	r := f.requests()[0]
	want := `<?xml version="1.0" encoding="UTF-8"?><Response><Pause length="1"/>` +
		`<Say voice="Polly.Joanna" language="en-US">Ward test message. Nothing is wrong. a link</Say></Response>`
	if r.form.Get("To") != testTo || r.form.Get("Twiml") != want {
		t.Fatalf("to %q, twiml\n%s\nwant\n%s", r.form.Get("To"), r.form.Get("Twiml"), want)
	}
	if err := f.provider().Send(context.Background(), m); err == nil || !IsPermanent(err) {
		t.Fatalf("Send without a number: %v, want permanent", err)
	}
}

// TestTwiML: what a call says is escaped (a title is written by whoever
// sent the alert), and so are attributes; a Gather holds what it says.
func TestTwiML(t *testing.T) {
	got := NewTwiML().
		Pause(2).
		Gather(TwilioGather{Action: "https://ward.example.com/twilio/voice/a.b?x=1&y=2", NumDigits: 1, Timeout: 5}, func(g *TwiML) {
			g.Say(`Disk <full> & "hot" on db-1's host ]]>`, "alice", "en-GB").Pause(1)
		}).
		Say("Goodbye.", "", "").
		String()
	want := `<?xml version="1.0" encoding="UTF-8"?><Response><Pause length="2"/>` +
		`<Gather input="dtmf" numDigits="1" timeout="5" action="https://ward.example.com/twilio/voice/a.b?x=1&amp;y=2" method="POST">` +
		`<Say voice="alice" language="en-GB">Disk &lt;full&gt; &amp; "hot" on db-1's host ]]&gt;</Say><Pause length="1"/></Gather>` +
		`<Say>Goodbye.</Say></Response>`
	if got != want {
		t.Fatalf("TwiML\n%s\nwant\n%s", got, want)
	}
	// Empty speech says nothing; a non-positive pause is left out.
	if got := NewTwiML().Say("  ", "", "").Pause(0).String(); got != `<?xml version="1.0" encoding="UTF-8"?><Response></Response>` {
		t.Fatalf("empty TwiML = %s", got)
	}
}

// TestSpeakableText: one line, no links (a voice reading a URL out is
// noise), no control characters, cut at a word.
func TestSpeakableText(t *testing.T) {
	for _, c := range []struct {
		in   string
		max  int
		want string
	}{
		{"Disk full\non db-1", 100, "Disk full on db-1"},
		{"see https://grafana.example.com/d/abc?x=1 and www.example.com/x now", 100, "see a link and a link now"},
		{"bell\x07 and\ttab", 100, "bell and tab"},
		{"one two three four", 12, "one two…"},
		{"averyveryverylongword", 8, "averyve…"},
		{"", 10, ""},
	} {
		if got := SpeakableText(c.in, c.max); got != c.want {
			t.Errorf("SpeakableText(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}

// TestValidE164: a plus, a country code that does not start with 0, and 7
// to 15 digits in all; nothing else.
func TestValidE164(t *testing.T) {
	for _, ok := range []string{"+15550142", "+12025550142", "+15550199", "+1234567"} {
		if !ValidE164(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "15550142", "+0155501", "+1 555 0142", "+1-555-0142", "+123456", "+1234567890123456", "+1555014a", " +15550142"} {
		if ValidE164(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestTwilioSendSMS: a text is a form POST of From, To and Body to the
// Messages resource; a body over Twilio's 1600 characters is permanent.
func TestTwilioSendSMS(t *testing.T) {
	f := newFakeTwilio(t, func(twilioRequest) twilioAnswer {
		return twilioAnswer{status: 201, body: `{"sid":"SM00112233445566778899aabbccddeeff","status":"queued"}`}
	})
	sid, err := f.provider().SendSMS(context.Background(), testTo, "Disk full on db-1")
	if err != nil || sid != "SM00112233445566778899aabbccddeeff" {
		t.Fatalf("SendSMS = %q, %v", sid, err)
	}
	r := f.requests()[0]
	want := url.Values{"Body": {"Disk full on db-1"}, "From": {testFrom}, "To": {testTo}}
	if r.method != http.MethodPost || r.path != "/2010-04-01/Accounts/"+testAccountSID+"/Messages.json" || r.form.Encode() != want.Encode() {
		t.Fatalf("request = %s %s %v", r.method, r.path, r.form)
	}
	if _, err := f.provider().SendSMS(context.Background(), testTo, strings.Repeat("x", 1601)); err == nil || !IsPermanent(err) {
		t.Fatalf("a long text: %v, want permanent", err)
	}
}

// TestBuildProviderTwilio: the configuration needs the account SID, a way
// to authenticate and the number to call from; its errors never quote a
// credential.
func TestBuildProviderTwilio(t *testing.T) {
	p, err := BuildProvider(Twilio, []byte(`{"account_sid":"`+testAccountSID+`","auth_token":"`+testAuthToken+`","from":"`+testFrom+`","to":"`+testTo+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*TwilioProvider); !ok {
		t.Fatalf("provider is %T", p)
	}
	for _, c := range []struct{ cfg, mention string }{
		{`{"auth_token":"` + testAuthToken + `","from":"` + testFrom + `"}`, "account_sid"},
		{`{"account_sid":"AC123","auth_token":"` + testAuthToken + `","from":"` + testFrom + `"}`, "account_sid"},
		{`{"account_sid":"` + testAccountSID + `","from":"` + testFrom + `"}`, "auth_token"},
		{`{"account_sid":"` + testAccountSID + `","api_key_sid":"` + testKeySID + `","from":"` + testFrom + `"}`, "api_key_secret"},
		{`{"account_sid":"` + testAccountSID + `","auth_token":"` + testAuthToken + `"}`, "from"},
		{`{"account_sid":"` + testAccountSID + `","auth_token":"` + testAuthToken + `","from":"555-0100"}`, "from"},
		{`{"account_sid":"` + testAccountSID + `","auth_token":"` + testAuthToken + `","from":"` + testFrom + `","to":"x"}`, "to"},
		{`{"account_sid":"` + testAccountSID + `","auth_token":"` + testAuthToken + `","from":"` + testFrom + `","voice":"a b"}`, "voice"},
		{`{"account_sid":"` + testAccountSID + `","auth_token":"` + testAuthToken + `","from":"` + testFrom + `","language":"english!"}`, "language"},
		{`{"account_sid":"` + testAccountSID + `","auth_token":"` + testAuthToken + `","from":"` + testFrom + `","api_base":"ftp://x"}`, "api_base"},
		{`{"account_sid":"` + testAccountSID + `","auth_token":"` + testAuthToken + `","from":"` + testFrom + `","api_base":"http://api.example.com"}`, "api_base"},
		{`{"account_sid":"` + testAccountSID + `","auth_token":"` + testAuthToken + `","from":"` + testFrom + `","api_base":"https://api.example.com/#x"}`, "api_base"},
	} {
		_, err := BuildProvider(Twilio, []byte(c.cfg))
		if err == nil || !strings.Contains(err.Error(), c.mention) {
			t.Errorf("%s: err = %v, want one naming %s", c.cfg, err, c.mention)
			continue
		}
		if strings.Contains(err.Error(), testAuthToken) {
			t.Errorf("error %q quotes the token", err)
		}
	}
}

// TestTwilioAPIBase: an API root is https, or http on this machine only (a
// test's fake): never plain http elsewhere, with credentials in a request.
func TestTwilioAPIBase(t *testing.T) {
	for _, ok := range []string{"https://api.example.com", "http://127.0.0.1:8080", "http://localhost:1234", "http://[::1]:9"} {
		if _, err := BuildProvider(Twilio, []byte(`{"account_sid":"`+testAccountSID+`","auth_token":"t","from":"`+testFrom+`","api_base":"`+ok+`"}`)); err != nil {
			t.Errorf("api_base %s refused: %v", ok, err)
		}
	}
}

// TestTwilioSendFitsTwilio: whatever a message holds, Send's TwiML fits
// Twilio's 4000 characters (the text cut to fit once escaped), so a long
// or markup-heavy message is still a call; a cut text ends in "…" alone.
func TestTwilioSendFitsTwilio(t *testing.T) {
	f := newFakeTwilio(t, func(twilioRequest) twilioAnswer { return twilioAnswer{status: 201, body: callJSON("queued")} })
	p := f.provider(func(c *TwilioConfig) { c.To = testTo })
	for _, m := range []Message{
		{Subject: strings.Repeat("&", 300), Body: strings.Repeat("&", 1000)},
		{Subject: "quotes", Body: strings.Repeat(`"`, 1000)},
		{Subject: "emoji", Body: strings.Repeat("\U0001F4DE", 1000)},
		{Subject: strings.Repeat("a ", 200), Body: strings.Repeat("<b>", 500)},
	} {
		if err := p.Send(context.Background(), m); err != nil {
			t.Errorf("Send(%.20q…): %v", m.Body, err)
		}
	}
	for _, r := range f.requests() {
		if tw := r.form.Get("Twiml"); len(tw) > TwilioMaxTwiML || strings.Contains(tw, "….") {
			t.Errorf("TwiML of %d bytes:\n%.200s", len(tw), tw)
		}
	}
	if err := p.Send(context.Background(), Message{Subject: strings.Repeat("word ", 100)}); err != nil {
		t.Fatal(err)
	}
	last := f.requests()[len(f.requests())-1].form.Get("Twiml")
	if strings.Contains(last, "….") || !strings.Contains(last, "…</Say>") {
		t.Fatalf("a cut subject:\n%s", last)
	}
}

// TestFitTwiMLTextCountsAsSent: what FitTwiMLText keeps takes, escaped as
// Say escapes it, no more than its room, whatever the text holds ("]]>"
// included); and Send's call with such text fits Twilio's limit.
func TestFitTwiMLTextCountsAsSent(t *testing.T) {
	for _, c := range []struct {
		text string
		room int
	}{
		{strings.Repeat("&&]]>", 300), 3900},
		{strings.Repeat(">", 2000), 1000},
		{strings.Repeat("]]>", 1000), 500},
		{"a]]>b", 3},
	} {
		if got := FitTwiMLText(c.text, c.room); len(xmlText(got)) > c.room {
			t.Errorf("FitTwiMLText(%.15q…, %d) escapes to %d bytes", c.text, c.room, len(xmlText(got)))
		}
	}
	f := newFakeTwilio(t, func(twilioRequest) twilioAnswer { return twilioAnswer{status: 201, body: callJSON("queued")} })
	p := f.provider(func(c *TwilioConfig) { c.To = testTo })
	if err := p.Send(context.Background(), Message{Subject: strings.Repeat("&&]]>", 60), Body: strings.Repeat("&&]]>", 200)}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if tw := f.requests()[0].form.Get("Twiml"); len(tw) > TwilioMaxTwiML {
		t.Fatalf("TwiML of %d bytes", len(tw))
	}
}

// FuzzFitTwiMLText: for any text and room, the kept text escapes to at
// most room bytes (for a room that holds the cut mark), is valid UTF-8,
// and is a prefix of the text up to the mark.
func FuzzFitTwiMLText(f *testing.F) {
	for _, seed := range []string{"]]>", "&&]]>", "a]]>b]]>c", "]]]>>", "<>&\"'", "\U0001F4DE]]>", "plain text", ""} {
		f.Add(strings.Repeat(seed, 50), 120)
		f.Add(seed, 4)
	}
	f.Fuzz(func(t *testing.T, text string, room int) {
		if room < len("…") || room > 1<<16 || !utf8.ValidString(text) {
			return
		}
		got := FitTwiMLText(text, room)
		if n := len(xmlText(got)); n > room {
			t.Fatalf("FitTwiMLText(%q, %d) = %q, which escapes to %d bytes", text, room, got, n)
		}
		if !utf8.ValidString(got) || !strings.HasPrefix(text, strings.TrimSuffix(got, "…")) && got != text {
			t.Fatalf("FitTwiMLText(%q, %d) = %q, not a cut of the text", text, room, got)
		}
	})
}

// TestTwilioSIDShapes: an account or API key SID is AC or SK and 32 letters
// or digits: Twilio's own (hexadecimal) and the placeholders its
// documentation shows both pass; anything else is refused.
func TestTwilioSIDShapes(t *testing.T) {
	hex := strings.Repeat("0f", 16) // a real-shaped one, built: no literal
	for _, ok := range []string{"AC" + hex, "AC" + strings.ToUpper(hex), "ACXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX", testAccountSID} {
		if !ValidTwilioAccountSID(ok) {
			t.Errorf("account SID %q refused", ok)
		}
	}
	for _, bad := range []string{"", "AC", "AC123", "AC" + hex[:31], "AC" + hex + "0", "SK" + hex, "XX" + hex, "ac" + hex,
		"AC" + hex[:31] + "-", "AC" + hex[:31] + "_", "AC" + hex[:31] + " ", " AC" + hex} {
		if ValidTwilioAccountSID(bad) {
			t.Errorf("account SID %q accepted", bad)
		}
	}
	cfg := func(keySID string) []byte {
		return []byte(`{"account_sid":"` + testAccountSID + `","api_key_sid":"` + keySID + `","api_key_secret":"s","from":"` + testFrom + `"}`)
	}
	for _, ok := range []string{"SK" + hex, "SKXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX", testKeySID} {
		if _, err := BuildProvider(Twilio, cfg(ok)); err != nil {
			t.Errorf("API key SID %q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"SK123", "AC" + hex, "SK" + hex[:31] + "-", "SK" + hex + "0"} {
		if _, err := BuildProvider(Twilio, cfg(bad)); err == nil || !strings.Contains(err.Error(), "api_key_sid") {
			t.Errorf("API key SID %q: err = %v, want it refused", bad, err)
		}
	}
}
