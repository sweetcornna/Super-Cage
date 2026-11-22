package termfilter

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Controls reports whether text could carry more than printable characters
// to a terminal: invalid UTF-8, or a control character (C0, DEL or C1), read
// by rune so no multi-byte character is mistaken for one.
func Controls(text string) bool {
	if !utf8.ValidString(text) {
		return true
	}
	return strings.IndexFunc(text, unicode.IsControl) >= 0
}

// Plain makes text from elsewhere (a server's message) safe to print as it
// is: invalid UTF-8 is replaced, and every control character but a newline
// becomes a space, by rune, so no sequence survives.
func Plain(text string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(text, "\uFFFD"))
}
