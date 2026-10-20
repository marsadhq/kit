package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDetail(t *testing.T) {
	if got := detail("  user key ABC is invalid\nsee https://x  ", "ABC", ""); got != "user key *** is invalid see https://x" {
		t.Errorf("detail = %q", got)
	}
	long := strings.Repeat("é", 150) // 300 bytes
	got := detail(long)
	if !strings.HasSuffix(got, "...") || len(got) > maxDetail+3 || !utf8.ValidString(got) {
		t.Errorf("detail of a long text = %q (%d bytes), want at most %d bytes of valid UTF-8 and an ellipsis", got, len(got), maxDetail)
	}
}
