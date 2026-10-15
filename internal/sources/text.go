package sources

import (
	"regexp"
	"strings"
)

// wsRunRe is a run of Unicode whitespace (Python's \s: the ideographic space U+3000 of Japanese text
// included; Go's own \s is ASCII only).
var wsRunRe = regexp.MustCompile(`[\s\p{Z}\x{85}]+`)

// collapseWS replaces every run of whitespace with one space.
func collapseWS(s string) string {
	return wsRunRe.ReplaceAllString(s, " ")
}

// strip trims surrounding whitespace.
func strip(s string) string { return strings.TrimSpace(s) }

// cutRunes is the first n characters of s (code points, not bytes).
func cutRunes(s string, n int) string {
	return sliceRunes(s, 0, n)
}

// sliceRunes is s[a:b] in characters for 0 <= a; b < 0 means "to the end". Out-of-range bounds clamp.
func sliceRunes(s string, a, b int) string {
	r := []rune(s)
	if b < 0 || b > len(r) {
		b = len(r)
	}
	if a > b {
		a = b
	}
	return string(r[a:b])
}
