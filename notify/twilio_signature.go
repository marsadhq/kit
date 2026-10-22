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
// fullURL with params (TwilioSignature), compared in constant time. Twilio
// signs an https callback without its port, and its own SDKs accept a
// signature over the URL with or without it: so does this, trying the URL
// as given, without its port, and with its scheme's default port (443 for
// https, 80 for http) where it names none. The scheme, host, path and query
// must match. An empty token or signature is never valid.
func ValidTwilioSignature(authToken, fullURL string, params url.Values, signature string) bool {
	if authToken == "" || signature == "" {
		return false
	}
	got, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	valid := false
	for _, u := range signedURLs(fullURL) {
		want, err := base64.StdEncoding.DecodeString(TwilioSignature(authToken, u, params))
		// Every form is checked, so the time taken says nothing of which.
		if err == nil && hmac.Equal(got, want) {
			valid = true
		}
	}
	return valid
}

// signedURLs are the forms of rawURL a signature may be over: as given,
// without its port, and with its scheme's default port where it names none.
// Only the authority changes; the rest is kept byte for byte.
func signedURLs(rawURL string) []string {
	out := []string{rawURL}
	scheme, rest, ok := strings.Cut(rawURL, "://")
	if !ok {
		return out
	}
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	authority, tail := rest[:end], rest[end:]
	if strings.Contains(authority, "@") {
		return out
	}
	host, port := authority, ""
	if i := strings.LastIndexByte(authority, ':'); i >= 0 && !strings.Contains(authority[i:], "]") {
		host, port = authority[:i], authority[i+1:]
	}
	if port != "" {
		return append(out, scheme+"://"+host+tail)
	}
	switch strings.ToLower(scheme) {
	case "https":
		out = append(out, scheme+"://"+host+":443"+tail)
	case "http":
		out = append(out, scheme+"://"+host+":80"+tail)
	}
	return out
}
