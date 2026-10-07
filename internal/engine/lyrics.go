package engine

import (
	"regexp"
	"strings"
	"unicode"
)

// Lyrics come in one canonical format and are translated for each song model, because each one reads them
// differently and sings whatever it does not understand (HeartMuLa sang "(Heavy bass drop, mechanical breathing)").
//
// The canonical format (what the "write-song" prompt asks for, and what the examples use):
//
//	[intro - heavy bass, mechanical breathing]     a section tag, case-insensitive, with an optional " - description"
//	[verse 2]                                      or ": description"; a number is kept
//	We rise together (we rise)                     a sung line; "(…)" inside a line is a backing vocal
//	(soft rain, distant thunder)                   a direction: a whole line in (), [] (not a tag) or *…* — never sung
//
// Section tags: intro, verse, pre-chorus, chorus, bridge, drop, breakdown, build-up, instrumental, outro (and the
// usual aliases: hook, refrain, prechorus, build, interlude, solo, break, ending …).
//
// Per model (see docs/music.md for the sources):
//   - ACE-Step 1.5: Title-case tags ([Pre-Chorus], [Build], [Drop] …); directions become the tag's description
//     ("[Intro - heavy bass drop, mechanical breathing]"); "(…)" stays (backing vocals).
//   - YuE2: [Intro] [Verse] [Pre-Chorus] [Chorus] [Bridge] [Interlude] [Outro], blank lines between sections, only
//     words to sing; directions go to the style; the song gets an [Outro] if it has none.
//   - HeartMuLa: [Intro] [Verse] [Prechorus] [Chorus] [Bridge] [Interlude] [Outro]; it lowercases and sings every
//     character it gets, so directions go to the style and backing vocals lose their brackets.
//   - LeVo 2: [intro-short] [verse] [chorus] [bridge] [inst-…] [outro-short]; only verse/chorus/bridge carry words
//     (the worker joins lines with "." and sections with " ; "); directions go to the style.
//   - MiniMax Music 3: lowercase tags on their own lines ([intro] [verse] [pre-chorus] [chorus] [post-chorus]
//     [bridge] [instrumental] [solo] [outro]; the model card's set, lowercased as its prompt normaliser does), no
//     numbers, no blank lines (its reference example); directions go to the music description; "(…)" stays.

// lyricSection is one section of parsed lyrics.
type lyricSection struct {
	tag        string   // canonical tag; "" for lines before the first tag
	label      string   // the tag as written, without its description ("Verse 2", "Guitar Solo")
	number     string   // "2" of [Verse 2]
	desc       []string // the tag's description, split at commas
	lines      []string // sung lines, as written (backing vocals still in parentheses)
	directions []string // direction phrases from the section's own lines
}

// canonicalTags maps a written tag name (lowercase, words joined by "-") to its canonical tag.
var canonicalTags = map[string]string{
	"intro": "intro", "opening": "intro",
	"verse": "verse", "rap": "verse", "spoken": "verse", "spoken-word": "verse",
	"pre-chorus": "pre-chorus", "prechorus": "pre-chorus", "pre": "pre-chorus", "lift": "pre-chorus",
	"chorus": "chorus", "hook": "chorus", "refrain": "chorus", "post-chorus": "chorus", "postchorus": "chorus",
	"final-chorus": "chorus",
	"bridge":       "bridge", "middle-eight": "bridge", "middle-8": "bridge",
	"drop": "drop", "beat-drop": "drop",
	"breakdown": "breakdown", "break-down": "breakdown",
	"build-up": "build-up", "buildup": "build-up", "build": "build-up", "rise": "build-up", "riser": "build-up",
	"instrumental": "instrumental", "inst": "instrumental", "interlude": "instrumental", "solo": "instrumental",
	"break": "instrumental", "instrumental-break": "instrumental",
	"outro": "outro", "ending": "outro", "end": "outro", "coda": "outro", "fade-out": "outro",
}

var (
	tagLine     = regexp.MustCompile(`^\[([^\[\]]+)\]\s*(.*)$`)
	tagNumber   = regexp.MustCompile(`^(.*?)[\s-]*(\d+)$`)
	inlineDir   = regexp.MustCompile(`\*[^*]+\*|\[[^\[\]]*\]`)
	inlineParen = regexp.MustCompile(`\(([^()]*)\)`)
	spaces      = regexp.MustCompile(`\s+`)
	commaRun    = regexp.MustCompile(`\s*,(\s*,)+`)
	spaceComma  = regexp.MustCompile(`\s+,`)
	stopComma   = regexp.MustCompile(`([.!?…]),`)
)

// vocables are the sounds a backing vocal is made of: a line of only these in parentheses is sung, not a direction.
var vocables = map[string]bool{"oh": true, "ooh": true, "oooh": true, "ah": true, "aah": true, "yeah": true,
	"la": true, "na": true, "hey": true, "whoa": true, "woah": true, "uh": true, "mm": true, "mmm": true, "woo": true,
	"ha": true, "ho": true, "yo": true, "da": true, "doo": true, "ay": true, "ayy": true, "eh": true, "hmm": true,
	"oh-oh": true, "na-na": true, "la-la": true, "uh-huh": true, "hoo": true, "wo": true, "whoo": true}

// splitTag reads a tag's inside: "Drop - heavy bass" / "verse 2: whispered" / "intro-short" → canonical tag,
// the label as written, its number and the description. ok is false for something that is no section tag.
func splitTag(inner string) (tag, label, number, desc string, ok bool) {
	inner = strings.TrimSpace(inner)
	name, rest := inner, ""
	for _, sep := range []string{":", " - ", " – ", " — ", " | ", ","} {
		if i := strings.Index(inner, sep); i > 0 {
			name, rest = strings.TrimSpace(inner[:i]), strings.TrimSpace(inner[i+len(sep):])
			break
		}
	}
	if name == inner {
		// "Chorus (x2)", "Verse (whispered)"
		if i := strings.Index(inner, "("); i > 0 && strings.HasSuffix(inner, ")") {
			name, rest = strings.TrimSpace(inner[:i]), strings.TrimSpace(inner[i+1:len(inner)-1])
		}
	}
	label = name
	if m := tagNumber.FindStringSubmatch(name); m != nil && m[1] != "" {
		name, number = m[1], m[2]
	}
	key := strings.Join(strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return r == ' ' || r == '-' || r == '_'
	}), "-")
	if t, found := canonicalTags[key]; found {
		return t, label, number, rest, true
	}
	// two words, one of them a tag: "intro-short", "final chorus", "guitar solo", "drum break" ("[heavy bass drop]"
	// is a direction, not a drop)
	if words := strings.Split(key, "-"); len(words) == 2 {
		if t, found := canonicalTags[words[0]]; found {
			return t, label, number, joinNonEmpty(words[1], rest), true
		}
		if t, found := canonicalTags[words[1]]; found {
			d := rest
			if t == "instrumental" {
				d = joinNonEmpty(strings.ToLower(label), rest)
			}
			return t, label, number, d, true
		}
	}
	return "", "", "", "", false
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

// phrases splits a direction or description into short comma-separated phrases.
func phrases(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '/' }) {
		p = strings.TrimFunc(strings.TrimSpace(p), func(r rune) bool {
			return unicode.IsPunct(r) && r != '\'' || unicode.IsSpace(r)
		})
		p = spaces.ReplaceAllString(p, " ")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// directionLine is the inside of a line that is a direction ("(Heavy bass drop)", "*whispered*", "[sound of rain]"),
// or ok false for a line to sing.
func directionLine(line string) (string, bool) {
	if len(line) < 2 {
		return "", false
	}
	open, close := line[0], line[len(line)-1]
	inner := strings.TrimSpace(line[1 : len(line)-1])
	switch {
	case open == '(' && close == ')' && !strings.ContainsAny(inner, "()"):
		// "(ooh, yeah)" and "(1, 2, 3, go!)" are backing vocals, "(Heavy bass drop)" a direction
		if strings.HasSuffix(inner, "!") {
			return "", false
		}
		words := strings.FieldsFunc(strings.ToLower(inner), func(r rune) bool {
			return unicode.IsSpace(r) || r == ',' || r == '!' || r == '.' || r == '…' || r == '~'
		})
		for _, w := range words {
			if !vocables[strings.Trim(w, "-'")] {
				return inner, true
			}
		}
		return "", false
	case open == '[' && close == ']' && !strings.ContainsAny(inner, "[]"),
		open == '*' && close == '*' && !strings.Contains(inner, "*"),
		open == '_' && close == '_' && !strings.Contains(inner, "_"):
		return inner, true
	}
	return "", false
}

// parseLyrics reads lyrics in the canonical format.
func parseLyrics(text string) []lyricSection {
	var secs []lyricSection
	cur := func() *lyricSection {
		if len(secs) == 0 {
			secs = append(secs, lyricSection{})
		}
		return &secs[len(secs)-1]
	}
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if m := tagLine.FindStringSubmatch(line); m != nil {
			if tag, label, number, desc, ok := splitTag(m[1]); ok {
				secs = append(secs, lyricSection{tag: tag, label: label, number: number, desc: phrases(desc)})
				if line = strings.TrimSpace(m[2]); line == "" {
					continue
				}
			}
		}
		if d, ok := directionLine(line); ok {
			s := cur()
			s.directions = append(s.directions, phrases(d)...)
			continue
		}
		// inline directions (*whispered*, [laughs]) are never sung either
		s := cur()
		for _, d := range inlineDir.FindAllString(line, -1) {
			s.directions = append(s.directions, phrases(d[1:len(d)-1])...)
		}
		line = strings.TrimSpace(spaces.ReplaceAllString(inlineDir.ReplaceAllString(line, " "), " "))
		if line != "" {
			s.lines = append(s.lines, line)
		}
	}
	return secs
}

// unbracket makes backing vocals plain words: "we rise (we rise)" → "we rise, we rise".
func unbracket(line string) string {
	line = inlineParen.ReplaceAllStringFunc(line, func(p string) string {
		return ", " + strings.TrimSpace(p[1:len(p)-1]) + ","
	})
	line = strings.NewReplacer("(", "", ")", "").Replace(line)
	line = commaRun.ReplaceAllString(line, ",")
	line = spaceComma.ReplaceAllString(line, ",")
	line = stopComma.ReplaceAllString(line, "$1")
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(spaces.ReplaceAllString(line, " ")), ","))
}

// words is s as lowercase words between single spaces (" heavy bass drop "), to find whole phrases in.
func words(s string) string {
	return " " + strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '\''
	}), " ") + " "
}

// addToStyle appends direction phrases the style does not mention yet (short ones, at most six, deduplicated).
func addToStyle(style string, dirs []string) string {
	seen := words(style)
	var add []string
	for _, d := range dirs {
		l := strings.ToLower(d)
		for _, sep := range []string{" with ", " and ", " while ", " then ", " as "} {
			if i := strings.Index(l, sep); len(l) > 48 && i > 0 {
				l = l[:i] // "heavy industrial bass drop with high-pitched vocal chops" → "heavy industrial bass drop"
			}
		}
		if w := words(l); w == "  " || strings.Contains(seen, w) || len(l) > 48 {
			continue
		}
		seen += words(l)
		add = append(add, l)
		if len(add) == 6 {
			break
		}
	}
	if len(add) == 0 {
		return style
	}
	return joinNonEmpty(strings.TrimRight(strings.TrimSpace(style), ",. "), strings.Join(add, ", "))
}

func dedupe(xs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, x := range xs {
		if l := strings.ToLower(x); !seen[l] {
			seen[l] = true
			out = append(out, x)
		}
	}
	return out
}

// TranslateLyrics turns canonical lyrics into the format of a music runtime ("acestep", "yue2", "heartmula",
// "levo2") and returns them with the style, which gains the directions taken out of the lyrics. Lyrics without a
// single tag or direction to change come back as they are; an unknown runtime gets them unchanged.
func TranslateLyrics(runtime, lyrics, style string) (string, string) {
	if strings.TrimSpace(lyrics) == "" {
		return lyrics, style
	}
	secs := parseLyrics(lyrics)
	var render func([]lyricSection) (string, []string)
	switch runtime {
	case "acestep":
		render = renderACEStep
	case "yue2":
		render = renderYuE2
	case "heartmula":
		render = renderHeartMuLa
	case "levo2":
		render = renderLeVo2
	case "minimax3":
		render = renderMiniMax3
	default:
		return lyrics, style
	}
	out, dirs := render(secs)
	return out, addToStyle(style, dedupe(dirs))
}

// sectionText writes sections as "[Tag]\nline\n…" blocks separated by blank lines.
func sectionText(secs []lyricSection, head func(lyricSection) string, line func(string) string) string {
	var blocks []string
	for _, s := range secs {
		var b []string
		if h := head(s); h != "" {
			b = append(b, h)
		}
		for _, l := range s.lines {
			if l = line(l); l != "" {
				b = append(b, l)
			}
		}
		if len(b) > 0 {
			blocks = append(blocks, strings.Join(b, "\n"))
		}
	}
	return strings.Join(blocks, "\n\n")
}

// titleCase writes "guitar solo" as "Guitar Solo".
func titleCase(s string) string {
	f := strings.Fields(strings.ReplaceAll(s, "_", " "))
	for i, w := range f {
		r := []rune(strings.ToLower(w))
		r[0] = unicode.ToUpper(r[0])
		f[i] = string(r)
	}
	return strings.Join(f, " ")
}

func withNumber(name string, s lyricSection) string {
	if s.number != "" && (s.tag == "verse" || s.tag == "chorus") {
		return name + " " + s.number
	}
	return name
}

var aceNames = map[string]string{"intro": "Intro", "verse": "Verse", "pre-chorus": "Pre-Chorus", "chorus": "Chorus",
	"bridge": "Bridge", "drop": "Drop", "breakdown": "Breakdown", "build-up": "Build", "instrumental": "Instrumental",
	"outro": "Outro"}

// renderACEStep: ACE-Step reads performance hints in the tag ("[Chorus - anthemic]"), so the directions move there:
// at most two short phrases ("don't stack too many tags": the model may sing them); the rest go to the caption. An
// instrumental section keeps its own name ([Break], [Guitar Solo]), which ACE-Step understands.
func renderACEStep(secs []lyricSection) (string, []string) {
	var rest []string
	out := sectionText(secs, func(s lyricSection) string {
		if s.tag == "" {
			rest = append(rest, s.directions...)
			return ""
		}
		name := aceNames[s.tag]
		if s.tag == "instrumental" && !strings.EqualFold(s.label, "inst") {
			name = titleCase(s.label)
		}
		var desc []string
		for _, d := range dedupe(append(append([]string{}, s.desc...), s.directions...)) {
			switch {
			case strings.EqualFold(d, s.label):
			case len(desc) < 2 && len(d) <= 32:
				desc = append(desc, d)
			default:
				rest = append(rest, d)
			}
		}
		h := "[" + withNumber(name, s)
		if len(desc) > 0 {
			h += " - " + strings.ToLower(strings.Join(desc, ", "))
		}
		return h + "]"
	}, func(l string) string { return l })
	return out, rest
}

// sungTag maps a canonical tag to a model's tag set: a section with words needs a tag that is sung.
func sungTag(tag string, sung bool, names map[string]string, wordless string) string {
	if n, ok := names[tag]; ok {
		return n
	}
	if !sung {
		return wordless
	}
	switch tag {
	case "drop":
		return names["chorus"]
	case "build-up":
		return names["pre-chorus"]
	default: // breakdown, instrumental with words
		return names["bridge"]
	}
}

func allDirections(secs []lyricSection, withDesc bool) []string {
	var dirs []string
	for _, s := range secs {
		if withDesc && s.tag != "" {
			dirs = append(dirs, s.desc...)
		}
		dirs = append(dirs, s.directions...)
	}
	return dirs
}

var yue2Names = map[string]string{"intro": "[Intro]", "verse": "[Verse]", "pre-chorus": "[Pre-Chorus]",
	"chorus": "[Chorus]", "bridge": "[Bridge]", "outro": "[Outro]"}

// renderYuE2: only section tags and the words to sing ("keep implementation notes out of lyrics"), blank lines
// between sections, and an outro so the planned song ends instead of stopping.
func renderYuE2(secs []lyricSection) (string, []string) {
	out := sectionText(secs, func(s lyricSection) string {
		if s.tag == "" {
			return ""
		}
		return sungTag(s.tag, len(s.lines) > 0, yue2Names, "[Interlude]")
	}, unbracket)
	if last := len(secs) - 1; last < 0 || secs[last].tag != "outro" {
		out = strings.TrimSpace(out + "\n\n[Outro]")
	}
	return out, allDirections(secs, true)
}

var heartNames = map[string]string{"intro": "[Intro]", "verse": "[Verse]", "pre-chorus": "[Prechorus]",
	"chorus": "[Chorus]", "bridge": "[Bridge]", "outro": "[Outro]"}

// renderHeartMuLa: HeartMuLa lowercases the lyrics and sings every word it gets, brackets or not; tags it knows,
// words without brackets, everything else to the style.
func renderHeartMuLa(secs []lyricSection) (string, []string) {
	out := sectionText(secs, func(s lyricSection) string {
		if s.tag == "" {
			return ""
		}
		return sungTag(s.tag, len(s.lines) > 0, heartNames, "[Interlude]")
	}, unbracket)
	return out, allDirections(secs, true)
}

// renderLeVo2: LeVo's structure tokens; intro/outro/instrumental sections carry no words, so words in them move to a
// verse (an intro with words: a short intro, then the words).
func renderLeVo2(secs []lyricSection) (string, []string) {
	length := func(s lyricSection, def string) string {
		for _, d := range s.desc {
			if d = strings.ToLower(d); d == "short" || d == "medium" || d == "long" {
				return d
			}
		}
		return def
	}
	var out []string
	add := func(tag string, lines []string) {
		b := []string{"[" + tag + "]"}
		for _, l := range lines {
			if l = unbracket(l); l != "" {
				b = append(b, l)
			}
		}
		out = append(out, strings.Join(b, "\n"))
	}
	for _, s := range secs {
		sung := len(s.lines) > 0
		switch s.tag {
		case "", "verse", "pre-chorus":
			if sung {
				add("verse", s.lines)
			}
		case "chorus", "bridge":
			if sung {
				add(s.tag, s.lines)
			}
		case "drop":
			if sung {
				add("chorus", s.lines)
			} else {
				add("inst-"+length(s, "medium"), nil)
			}
		case "build-up", "breakdown", "instrumental":
			if sung {
				add("bridge", s.lines)
			} else {
				add("inst-"+length(s, "short"), nil)
			}
		case "intro":
			add("intro-"+length(s, "short"), nil)
			if sung {
				add("verse", s.lines)
			}
		case "outro":
			if sung {
				add("verse", s.lines)
			}
			add("outro-"+length(s, "short"), nil)
		}
	}
	var dirs []string
	for _, s := range secs {
		for _, d := range s.desc {
			if l := strings.ToLower(d); l != "short" && l != "medium" && l != "long" {
				dirs = append(dirs, d)
			}
		}
		dirs = append(dirs, s.directions...)
	}
	return strings.Join(out, "\n\n"), dirs
}

var minimax3Names = map[string]string{"intro": "[intro]", "verse": "[verse]", "pre-chorus": "[pre-chorus]",
	"chorus": "[chorus]", "bridge": "[bridge]", "instrumental": "[instrumental]", "outro": "[outro]"}

// renderMiniMax3: MiniMax Music 3's section tags, lowercase on their own lines (its prompt normaliser lowercases
// every tag and keeps only the tags of a line that starts with one), one section after the other without blank
// lines as in its reference example; a post-chorus and a solo keep their own tag. Directions and tag descriptions
// go to the music description; backing vocals in "(…)" stay.
func renderMiniMax3(secs []lyricSection) (string, []string) {
	var out []string
	for _, s := range secs {
		if s.tag != "" {
			tag := sungTag(s.tag, len(s.lines) > 0, minimax3Names, "[instrumental]")
			label := strings.ToLower(s.label)
			switch {
			case s.tag == "chorus" && strings.HasPrefix(label, "post"):
				tag = "[post-chorus]"
			case s.tag == "instrumental" && strings.Contains(label, "solo"):
				tag = "[solo]"
			}
			out = append(out, tag)
		}
		out = append(out, s.lines...)
	}
	return strings.Join(out, "\n"), allDirections(secs, true)
}
