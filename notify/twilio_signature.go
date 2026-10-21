package notify

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/url"
	"slices"
	"sort"
	"strings"
)

// A request Twilio makes to a tool (a <Gather>'s keypresses posted to its
// action URL) carries X-Twilio-Signature, which proves it came from Twilio
// and was not changed: only Twilio and the account hold the auth token it is
// made with. A tool checks it with ValidTwilioSignature against the URL it
// gave Twilio, before it trusts any parameter.

// TwilioSignature is the X-Twilio-Signature Twilio sends with a request to
// fullURL (exactly as Twilio requested it, query string included) carrying
// the form params: base64 of HMAC-SHA1, under the account's auth token, of
// the URL followed by each parameter's name and value, by name (a name's
// several values in order and once each), with no separators.
func TwilioSignature(authToken, fullURL string, params url.Values) string {
	var b strings.Builder
	b.WriteString(fullURL)
	names := make([]string, 0, len(params))
	for k := range params {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		vs := slices.Clone(params[k])
		sort.Strings(vs)
		vs = slices.Compact(vs)
		for _, v := range vs {
			b.WriteString(k)
			b.WriteString(v)
		}
	}
	m := hmac.New(sha1.New, []byte(authToken))
	m.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// ValidTwilioSignature reports whether signature is Twilio's for a request to
// fullURL with params (TwilioSignature), compared in constant time. An empty
// token or signature is never valid.
func ValidTwilioSignature(authToken, fullURL string, params url.Values, signature string) bool {
	if authToken == "" || signature == "" {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(TwilioSignature(authToken, fullURL, params))
	if err != nil {
		return false
	}
	got, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}
