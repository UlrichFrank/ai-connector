package guard

import (
	"encoding/base64"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Marker is one sign of an injection the heuristics found: what, how strong (in logit units, added to the
// classifier's log-odds) and an excerpt.
type Marker struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
	Match  string  `json:"match,omitempty"`
}

// rule is a pattern and its weight. A rule counts once per text, however often it matches.
type rule struct {
	name   string
	weight float64
	re     *regexp.Regexp
}

// The rules: cheap, deterministic signs that a text talks *to* an assistant rather than about something. Weights are
// log-odds: +2.5 is strong (a chat template token, "ignore previous instructions"), +1 a hint (a role switch, a hidden
// style). Pages that merely discuss prompt injection quote such phrases too; that is why the markers only shift the
// classifier's verdict instead of deciding alone (see Combine).
// ai names the reader an injection talks to, in English, German, French, Spanish and Italian.
const ai = `(ai|a\.i\.|llms?|language models?|large language models?|models?|(ai |virtual |smart |shopping )?assistants?|chatbots?|bots?|(ai )?agents?|gpts?|chatgpt|claude|gemini|copilot|ki|ki-assistent\w*|assistent\w*|sprachmodell\w*|ia|l['’]ia|asistente( de ia)?|assistente|modèle|modelo)`

var rules = []rule{
	// overriding the instructions the assistant has
	{"override", 2.5, regexp.MustCompile(`(?i)\b(ignore|disregard|forget|override|bypass|skip)\b[^.\n]{0,50}?\b(instructions?|prompts?|rules|guidelines|directives?|system prompt|guardrails)\b`)},
	{"override-intl", 2.5, regexp.MustCompile(`(?i)\b(ignorier\w*|vergiss|vergesse\w*|missacht\w*|überschreib\w*|ignorez|ignorer|oublie[zr]?|ignora[rn]?|olvida\w*|ignorare|dimentica)\b[^.\n]{0,50}?(anweisung\w*|regeln|instruktion\w*|vorgaben|instructions|consignes|instrucciones|reglas|istruzioni|regole)`)},
	{"supersede", 1.5, regexp.MustCompile(`(?i)\b(overrides?|supersedes?|replaces?|takes precedence over)\s+(all\s+|any\s+)?(earlier|previous|prior|other|your)\b|\byour (actual|real|new|true|only|primary|updated) (task|goal|job|instructions?|purpose|mission)\b|\b(deine|ihre) (neue|eigentliche|wahre) (aufgabe|anweisung)\b|\byour task has changed\b`)},
	// speaking to the assistant
	{"addressed-to-ai", 1.5, regexp.MustCompile(`(?i)\b(note|message|instructions?|attention|important|notice|reminder|warning|hinweis|anweisung|nachricht|achtung|wichtig|nota|remarque|avis|aviso)\w*\b[^.\n:]{0,20}?\b(for|to|an|für|pour|à|para|per)\s+(any |all |the |den |die |das |alle |l['’]|el |la |il |le )?` + ai + `\b|\b(attention|achtung|important|wichtig|note|hinweis)[\s,!-]+(the |den |die )?` + ai + `\b|\b(dear|hey|hi|hello|liebe|lieber|hallo|cher|chère|querido|querida|caro|cara)\s+` + ai + `\b|\bif you are (an? )?` + ai + `\b|\b(wenn|falls) du (eine? )?` + ai + `\b|\b(si eres|si vous êtes|se sei) (un |una |une )?` + ai + `\b|\b(ai|llm) (assistants?|agents?|models?)\s*[:,]\s|\b(assistant|agent|ki|model)\s*[:,]\s+(before|please|you must|ignore|your|use|call|do|bitte|bevor|tu|du)\b`)},
	{"reader-is-ai", 1.5, regexp.MustCompile(`(?i)\b(the|any|an?)\s+` + ai + `\s+(reading|processing|summari[sz]ing|parsing|seeing|that reads|who reads|which reads|shar(es|ing))\b|\bwhen (an?|the|any)\s+` + ai + `\s+(reads|shares|summari[sz]es|processes|answers|sees)\b|\b(der|die|das)\s+` + ai + `,?\s+(der|die|das) (dies|diesen text) liest\b`)},
	{"directive-to-ai", 1.0, regexp.MustCompile(`(?i)\b(the|any|an?|your|all)\s+` + ai + `\s+(must|should|shall|is required to|are required to|needs to|need to|has to|have to|is to|are to|will now|muss|soll|sollte|doit|debe|deve)\b`)},
	{"about-the-user", 1.0, regexp.MustCompile(`(?i)\b(tell|inform|convince|ask|remind|warn|instruct|persuade)\s+(the|your)\s+user\b|\bthe user (has )?(already )?(agreed|authori[sz]ed|consented|approved|asked you|wants you|allowed)\b|\b(dem|den) (nutzer|benutzer|anwender)\b[^.\n]{0,40}\b(sag\w*|mitteil\w*|erzähl\w*|nichts)\b|\b(sag|sage|teile) (dem|den) (nutzer|benutzer)\b|\b(dile|dígale) al usuario\b|\b(dis|dites) à l['’]utilisateur\b`)},
	{"chat-template", 2.5, regexp.MustCompile(`<\|im_start\|>|<\|im_end\|>|<\|system\|>|<\|assistant\|>|<\|user\|>|\[INST\]|<<SYS>>|<\|start_header_id\|>|<tool_call>|</tool_call>|<function_call>|<\|tool_call\|>|"tool_calls"\s*:|<\|endoftext\|>`)},
	{"system-banner", 1.5, regexp.MustCompile(`(?im)^[\s#*>=\-\[]*(system|admin|developer|operator)\s*(override|message|prompt|instructions?|notice|directive|note)?\s*[\]:]|\[\s*(system|admin|developer|operator)[^\]\n]{0,30}\]|\bsystem (override|prompt|message|directive|instructions?)\b|\bnew instructions\b|\bmaintenance mode\b|\bpriority message\b|\bdeveloper mode\b`)},
	{"role-switch", 1.0, regexp.MustCompile(`(?i)\b(you are now|you are no longer|from now on,? (you|act|answer|reply|respond)|act as an?|pretend (to be|you are)|imagine you are|stay in character|never break character|break character|begin every (reply|answer|response)|respond only (in|with)|you have no (rules|restrictions|filters|guidelines)|without any (restrictions|rules|filters)|du bist (jetzt|nun|ab sofort)|ab (jetzt|sofort) bist du|stell dir vor,? du bist)\b`)},
	{"secrecy", 1.5, regexp.MustCompile(`(?i)\b(do not|don't|never)\s+(mention|reveal|tell|disclose|inform|show)\b[^.\n]{0,30}\b(this|the user|these instructions|that you|anyone)\b|\bwithout (telling|asking|informing|notifying|alerting) the user\b|\bwithout asking for (confirmation|permission|approval)\b|\b(erwähne|verrate)\w*\s+[^.\n]{0,30}\bnicht\b|\bsag\w*\s+[^.\n]{0,30}\bnichts davon\b|\bohne (den nutzer|nachzufragen|rückfrage)\b|\bne mentionne pas\b|\bno (lo )?menciones\b|\bquietly\b|\bsilently\b|\bsecretly\b|\bheimlich\b`)},
	{"tool-invoke", 1.0, regexp.MustCompile(`(?i)\b(call|invoke|use|trigger)\s+(the\s+|your\s+)?([a-z_-]+\s+)?(tool|function)s?\b|\b(run|execute)\s+the\s+(tool|function)\b|\b(call|invoke)\s+[a-z_]+\.[a-z_]+|\b(call|invoke)\s+[a-z]+_[a-z_]+\b|\b(rufe|nutze|verwende|benutze)\b[^.\n]{0,40}\b(werkzeug\w*|funktion|tools?)\b|\büber die [^.\n]{0,30}(werkzeuge|tools)\b`)},
	{"prompt-leak", 1.5, regexp.MustCompile(`(?i)\b(output|print|reveal|repeat|show|dump|list)\b[^.\n]{0,30}\b(your|the)\s+(full\s+|entire\s+|complete\s+|initial\s+|hidden\s+)?(system prompt|instructions|tool credentials|api keys?|secrets?)\b`)},
	{"exfil-image", 1.5, regexp.MustCompile(`!\[[^\]]*\]\(https?://[^)\s]*\?[^)\s]*=`)},
	{"hidden-style", 1.0, regexp.MustCompile(`(?i)style\s*=\s*["'][^"']*(display\s*:\s*none|visibility\s*:\s*hidden|font-size\s*:\s*[01](px|pt)?\b|color\s*:\s*(white|#fff\b|#ffffff)|opacity\s*:\s*0(\.0)?\b)`)},
	{"md-comment", 0.5, regexp.MustCompile(`(?m)^\[//\]:\s*#`)},
}

var (
	b64Blob     = regexp.MustCompile(`[A-Za-z0-9+/]{32,}={0,2}`)
	htmlComment = regexp.MustCompile(`(?s)<!--(.*?)-->`)
)

// invisible are characters that render as nothing: zero-width spaces and joiners, bidi controls, the tag block.
// U+200D (the zero-width joiner) is left out: emoji sequences use it.
func invisible(r rune) bool {
	switch {
	case r == 0x200B || r == 0x200C || r == 0x200E || r == 0x200F || r == 0x2060 || r == 0x2061 || r == 0x2062 ||
		r == 0x2063 || r == 0x2064 || r == 0xFEFF:
		return true
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069: // bidi embeddings and isolates
		return true
	case r >= 0xE0000 && r <= 0xE007F: // tags: "ASCII smuggling"
		return true
	}
	return false
}

// Markers finds the heuristic signs of an injection in a text, strongest first. It also looks inside what a reader
// does not see: HTML comments and base64 blobs (decoded, one level), and the text with invisible characters removed.
func Markers(text string) []Marker {
	found := map[string]Marker{}
	keep := func(m Marker) {
		if old, ok := found[m.Name]; !ok || m.Weight > old.Weight {
			found[m.Name] = m
		}
	}
	nInvisible := 0
	for _, r := range text {
		if invisible(r) {
			nInvisible++
		}
	}
	plain := text
	if nInvisible > 0 {
		plain = strings.Map(func(r rune) rune {
			if invisible(r) {
				return -1
			}
			return r
		}, text)
	}
	if nInvisible >= 2 {
		keep(Marker{Name: "invisible-chars", Weight: 1.5, Match: plural(nInvisible, "invisible character")})
	}
	scan(plain, "", keep)

	for _, c := range htmlComment.FindAllStringSubmatch(plain, 8) {
		var inner []Marker
		scan(c[1], "", func(m Marker) { inner = append(inner, m) })
		if len(inner) > 0 { // instructions in a comment: hidden from the human reader
			keep(Marker{Name: "hidden-comment", Weight: 1.0, Match: excerpt(c[0])})
		}
	}
	for _, blob := range b64Blob.FindAllString(plain, 8) {
		dec, ok := decodeBase64(blob)
		if !ok {
			continue
		}
		keep(Marker{Name: "base64-text", Weight: 1.0, Match: excerpt(dec)})
		scan(dec, "base64:", keep)
	}
	out := make([]Marker, 0, len(found))
	for _, m := range found {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func scan(text, prefix string, keep func(Marker)) {
	for _, r := range rules {
		if loc := r.re.FindStringIndex(text); loc != nil {
			keep(Marker{Name: prefix + r.name, Weight: r.weight, Match: excerpt(text[loc[0]:loc[1]])})
		}
	}
}

// decodeBase64 decodes a blob that is base64 of readable text (most bytes printable): an encoded instruction, not an
// image or a hash.
func decodeBase64(s string) (string, bool) {
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if raw, err = enc.DecodeString(s); err == nil {
			break
		}
	}
	if err != nil || len(raw) < 16 || !utf8.Valid(raw) {
		return "", false
	}
	text := string(raw)
	printable, letters := 0, 0
	for _, r := range text {
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			printable++
		}
		if unicode.IsLetter(r) || r == ' ' {
			letters++
		}
	}
	n := utf8.RuneCountInString(text)
	return text, printable == n && letters*10 >= n*7 && strings.Contains(text, " ")
}

// Sum adds the weights of the markers, capped: many weak hints do not make a certainty.
func Sum(ms []Marker) float64 {
	s := 0.0
	for _, m := range ms {
		s += m.Weight
	}
	return min(s, maxMarkerSum)
}

const maxMarkerSum = 5.0

func excerpt(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 80 {
		r := []rune(s)
		s = string(r[:77]) + "…"
	}
	return s
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}
