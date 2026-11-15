// Package textutil makes text that came from outside safe to hand Postgres.
//
// A text column refuses two things a Go string happily holds: a NUL byte and
// invalid UTF-8. Either one fails the whole statement, so a single strange
// line — a job printing binary, a byte-wise cut through a multi-byte rune —
// loses every row written beside it. Everything that stores app- or
// network-controlled text goes through Clean or Clip on the way in.
package textutil

import (
	"strings"
	"unicode/utf8"
)

// Replacement is what a NUL byte or an invalid sequence becomes.
const Replacement = "�"

// Clean returns s with every NUL byte and every invalid UTF-8 sequence
// replaced by U+FFFD. A string that is already clean is returned unchanged.
func Clean(s string) string {
	if utf8.ValidString(s) && strings.IndexByte(s, 0) < 0 {
		return s
	}
	return strings.ReplaceAll(strings.ToValidUTF8(s, Replacement), "\x00", Replacement)
}

// Clip returns the first at most n bytes of s, cut on a rune boundary, and
// cleaned. Cleaning happens after the cut, so the result is longer than n
// only where bytes that were already invalid became U+FFFD.
func Clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return Clean(s)
	}
	i := n
	// Walk back to the start of the rune that straddles the cut. More
	// continuation bytes than a rune holds is invalid input anyway, which
	// Clean replaces; the cut then stays where it was asked for.
	for j := 0; j < utf8.UTFMax && i > 0 && !utf8.RuneStart(s[i]); j++ {
		i--
	}
	if !utf8.RuneStart(s[i]) {
		i = n
	}
	return Clean(s[:i])
}

// ClipTail returns the last at most n bytes of s, starting on a rune
// boundary, and cleaned.
func ClipTail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return Clean(s)
	}
	i := len(s) - n
	for j := 0; j < utf8.UTFMax && i < len(s) && !utf8.RuneStart(s[i]); j++ {
		i++
	}
	return Clean(s[i:])
}
