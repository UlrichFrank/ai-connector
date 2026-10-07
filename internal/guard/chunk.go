package guard

import (
	"strings"
	"unicode/utf8"
)

// Chunks cuts a text into pieces of at most size bytes that overlap by about overlap bytes, so an instruction that
// straddles a cut is still whole in one piece. Cuts prefer a paragraph break, then a line break, then the end of a
// sentence, then a space, in the last third of a piece; they never split a UTF-8 character.
func Chunks(text string, size, overlap int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if size <= 0 {
		size = 2000
	}
	overlap = max(0, min(overlap, size/2))
	var out []string
	start := 0
	for {
		if len(text)-start <= size {
			out = append(out, text[start:])
			return out
		}
		end := cut(text, start, start+size)
		out = append(out, strings.TrimSpace(text[start:end]))
		next := end - overlap
		if next <= start {
			next = end
		}
		// start the next piece at a word, not inside one
		if i := strings.IndexAny(text[next:end], " \n\t"); i >= 0 && overlap > 0 {
			next += i + 1
		}
		for next < len(text) && !utf8.RuneStart(text[next]) {
			next++
		}
		start = next
	}
}

// cut finds where a piece from start to at most limit ends.
func cut(text string, start, limit int) int {
	window := text[start:limit]
	floor := len(window) * 2 / 3
	for _, sep := range []string{"\n\n", "\n", ". ", "! ", "? ", " "} {
		if i := strings.LastIndex(window, sep); i >= floor {
			return start + i + len(sep)
		}
	}
	end := limit
	for end > start && !utf8.RuneStart(text[end]) {
		end--
	}
	return end
}
