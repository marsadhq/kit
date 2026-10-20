package notify

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxDetail bounds how much of a destination's own error text an error
// carries.
const maxDetail = 200

// detail returns a destination's error text fit for an error message: on one
// line, at most maxDetail bytes, and with every non-empty secret (a token, a
// user key, a topic) replaced by "***" in case the destination echoed it.
func detail(text string, secrets ...string) string {
	for _, s := range secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, "***")
		}
	}
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(text))
	if len(text) > maxDetail {
		cut := maxDetail
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "..."
	}
	return text
}
