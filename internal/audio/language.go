package audio

import (
	"strings"
	"unicode"
)

// GuessLanguage tells a text's language where it is easy: by its script (Chinese, Japanese, Korean, Hindi, Russian,
// Arabic, Greek, Hebrew), else German from English by common words ("" when it cannot tell).
func GuessLanguage(text string) string {
	scripts := map[string]int{}
	letters := 0
	for _, r := range text {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		switch {
		case unicode.In(r, unicode.Hiragana, unicode.Katakana):
			scripts["ja"] += 3 // kana: Japanese even among kanji
		case unicode.Is(unicode.Han, r):
			scripts["zh"]++
		case unicode.Is(unicode.Hangul, r):
			scripts["ko"]++
		case unicode.Is(unicode.Devanagari, r):
			scripts["hi"]++
		case unicode.Is(unicode.Cyrillic, r):
			scripts["ru"]++
		case unicode.Is(unicode.Arabic, r):
			scripts["ar"]++
		case unicode.Is(unicode.Greek, r):
			scripts["el"]++
		case unicode.Is(unicode.Hebrew, r):
			scripts["he"]++
		}
	}
	best, n := "", 0
	for k, v := range scripts {
		if v > n || v == n && k < best {
			best, n = k, v
		}
	}
	if best == "zh" && scripts["ja"] > 0 {
		best = "ja"
	}
	if n*2 > letters {
		return best
	}
	de, en := 0, 0
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) }) {
		switch w {
		case "der", "die", "das", "und", "ist", "nicht", "ein", "eine", "ich", "du", "wir", "sie", "es", "mit", "auf",
			"für", "zu", "den", "dem", "von", "auch", "sich", "noch", "wie", "aber", "heute", "guten", "danke", "bitte":
			de++
		case "the", "and", "is", "not", "a", "an", "i", "you", "we", "they", "it", "with", "on", "for", "to", "of",
			"also", "this", "that", "are", "be", "what", "how", "but", "today", "good", "thanks", "please":
			en++
		}
		if strings.ContainsAny(w, "äöüß") {
			de += 2
		}
	}
	switch {
	case de > en:
		return "de"
	case en > de:
		return "en"
	}
	return ""
}
