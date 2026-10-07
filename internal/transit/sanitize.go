package transit

import (
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Length caps for strings from the providers.
const (
	maxName    = 120 // stop names, directions, lines
	maxRemark  = 240
	maxID      = 600 // DB journey ids are long
	maxRemarks = 3
)

var tagRE = regexp.MustCompile(`(?s)<[^<>]{0,1000}>`)

// Clean makes a provider string safe to show: no HTML (tags removed, entities decoded, then any "<" or ">" that is
// left removed), no control, format (zero-width, bidi) or private-use characters, whitespace collapsed to single
// spaces, at most max runes (cut with "…").
func Clean(s string, max int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	if strings.ContainsAny(s, "<&") {
		s = tagRE.ReplaceAllString(s, " ")
		s = html.UnescapeString(s)
		s = tagRE.ReplaceAllString(s, " ") // entities that decoded to tags
	}
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r) || r == ' ':
			space = true
			continue
		case r == '<' || r == '>':
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Co, r), unicode.Is(unicode.Cs, r),
			r == utf8.RuneError, r == ' ', r == ' ':
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := b.String()
	if max > 0 && utf8.RuneCountInString(out) > max {
		r := []rune(out)
		out = strings.TrimSpace(string(r[:max-1])) + "…"
	}
	return out
}

// cleanID keeps an opaque id usable: printable ASCII and Latin letters only (DB ids carry "#", "|", "@", "=" and
// station names), no control or format characters, capped.
func cleanID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r < 0x7f || unicode.IsLetter(r) || r == '×' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > maxID {
		return ""
	}
	return out
}

// remarks cleans, de-duplicates and caps a list of remarks.
func remarks(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		s = Clean(s, maxRemark)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) == maxRemarks {
			break
		}
	}
	return out
}
