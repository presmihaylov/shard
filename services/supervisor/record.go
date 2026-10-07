package supervisor

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxReason bounds what a guest's reason may take of a record, a log line and a column of ls.
const maxReason = 256

// OneLine makes the guest's reason safe for a record and a log line: no control bytes, valid UTF-8, at most maxReason bytes.
func OneLine(reason string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}

		return r
	}, strings.ToValidUTF8(reason, "?"))
	if len(clean) <= maxReason {
		return clean
	}
	cut := maxReason
	for !utf8.RuneStart(clean[cut]) {
		cut--
	}

	return clean[:cut]
}
