package notify

import (
	"net/url"
	"testing"
)

// TestTwilioSignature: the signature of a request is HMAC-SHA1, under the
// auth token, of the full URL followed by every parameter's name and value
// in name order, in base64 (the vectors were computed apart from this code,
// as Twilio's own request validators do).
func TestTwilioSignature(t *testing.T) {
	params := url.Values{
		"AccountSid": {testAccountSID}, "CallSid": {testCallSID}, "CallStatus": {"in-progress"}, "Digits": {"1"},
		"Direction": {"outbound-api"}, "From": {testFrom}, "To": {testTo},
	}
	const u = "https://ward.example.com/twilio/voice/tok123"
	for _, c := range []struct {
		token, url string
		params     url.Values
		want       string
	}{
		{testAuthToken, u, params, "NopVJkX9pcogbg6fZwpklTgimV0="},
		{testAuthToken, u + "?x=1&y=a%20b", params, "OtDg5RBkjFYaCTsV5PH3+oMVYo8="},
		{"k", "https://example.com/p", url.Values{"B": {"2", "1"}, "A": {"z"}}, "es4OcowgcJ/StNxqTPjbMfqI7xg="},
		{"k", "https://example.com/p", nil, "1oaKEY7jdTlteZrYNyS9unsN/sA="},
	} {
		if got := TwilioSignature(c.token, c.url, c.params); got != c.want {
			t.Errorf("TwilioSignature(%s) = %s, want %s", c.url, got, c.want)
		}
		if !ValidTwilioSignature(c.token, c.url, c.params, c.want) {
			t.Errorf("ValidTwilioSignature refused %s", c.want)
		}
	}

	good := "NopVJkX9pcogbg6fZwpklTgimV0="
	forged := func(change func(url.Values)) url.Values {
		p := url.Values{}
		for k, v := range params {
			p[k] = append([]string(nil), v...)
		}
		change(p)
		return p
	}
	for _, c := range []struct {
		name, token, url, sig string
		params                url.Values
	}{
		{"wrong token", "another-token", u, good, params},
		{"wrong URL", testAuthToken, "https://ward.example.com/twilio/voice/tok124", good, params},
		{"other scheme", testAuthToken, "http://ward.example.com/twilio/voice/tok123", good, params},
		{"forged digits", testAuthToken, u, good, forged(func(p url.Values) { p.Set("Digits", "2") })},
		{"added parameter", testAuthToken, u, good, forged(func(p url.Values) { p.Set("Extra", "1") })},
		{"dropped parameter", testAuthToken, u, good, forged(func(p url.Values) { p.Del("CallSid") })},
		{"no signature", testAuthToken, u, "", params},
		{"no token", "", u, TwilioSignature("", u, params), params},
		{"not base64", testAuthToken, u, "!!!", params},
	} {
		if ValidTwilioSignature(c.token, c.url, c.params, c.sig) {
			t.Errorf("%s: accepted", c.name)
		}
	}
}
