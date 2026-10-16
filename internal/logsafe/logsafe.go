// Package logsafe makes a string someone else chose safe to log
// (threat model INV-18): quoted, so it cannot forge a line or hide in
// one, and truncated, so it cannot flood the log.
package logsafe

import (
	"strconv"
	"unicode/utf8"
)

// Max is how many bytes of a string Quote keeps.
const Max = 200

// Quote is s quoted as Go would, cut to its first Max bytes (on a rune
// boundary) and marked with how long it was if it was cut.
func Quote(s string) string {
	if len(s) <= Max {
		return strconv.Quote(s)
	}
	n := Max
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strconv.Quote(s[:n]) + "…(" + strconv.Itoa(len(s)) + " bytes)"
}
