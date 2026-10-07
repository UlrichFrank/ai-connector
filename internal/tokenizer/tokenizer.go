// Package tokenizer reads a Hugging Face tokenizer.json of a byte-level BPE model — the tokenizers of ModernBERT (Laya)
// and Qwen3 — and turns text into the same token ids as the Rust tokenizers library: added tokens first, then the
// normaliser (NFC), the pre-tokeniser (GPT-2's or Qwen's split pattern, written out by hand: Go's regexp has no
// look-ahead) and the BPE merges over the byte-level alphabet.
package tokenizer

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Split names the pre-tokeniser's pattern.
type Split int

const (
	// SplitGPT2 is the ByteLevel pre-tokeniser's own pattern (GPT-2, ModernBERT).
	SplitGPT2 Split = iota
	// SplitQwen is Qwen2/3's pattern (single digits, case-insensitive contractions, newline runs).
	SplitQwen
	// SplitQwen35 is Qwen3.5/3.6's: Qwen's, with combining marks (\p{M}) counted as letters.
	SplitQwen35
)

const (
	gpt2Pattern   = `'s|'t|'re|'ve|'m|'ll|'d| ?\p{L}+| ?\p{N}+| ?[^\s\p{L}\p{N}]+|\s+(?!\S)|\s+`
	qwenPattern   = `(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?\p{L}+|\p{N}| ?[^\s\p{L}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+`
	qwen35Pattern = `(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?[\p{L}\p{M}]+|\p{N}| ?[^\s\p{L}\p{M}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+`
)

// Tokenizer is a loaded byte-level BPE tokenizer.
type Tokenizer struct {
	vocab   map[string]int
	ranks   map[[2]string]int
	added   []addedToken // longest first
	nfc     bool
	split   Split
	suffix  []int // appended by Encode with special tokens (Qwen: <|endoftext|>)
	prefix  []int // prepended (ModernBERT: [CLS]); suffix then [SEP]
	byteMap [256]string
	decOnce sync.Once
	tokens  []string      // by id (Decode)
	special map[int]bool  // added tokens marked special
	unByte  map[rune]byte // the byte-level alphabet back to bytes
	mu      sync.Mutex    // guards cache
	cache   map[string][]int
}

type addedToken struct {
	Content    string `json:"content"`
	ID         int    `json:"id"`
	Special    bool   `json:"special"`
	Normalized bool   `json:"normalized"`
	LStrip     bool   `json:"lstrip"`
	RStrip     bool   `json:"rstrip"`
	SingleWord bool   `json:"single_word"`
}

type fileJSON struct {
	AddedTokens []addedToken    `json:"added_tokens"`
	Normalizer  json.RawMessage `json:"normalizer"`
	PreTok      json.RawMessage `json:"pre_tokenizer"`
	PostProc    json.RawMessage `json:"post_processor"`
	Model       struct {
		Type   string            `json:"type"`
		Vocab  map[string]int    `json:"vocab"`
		Merges []json.RawMessage `json:"merges"`
		// continuing_subword_prefix etc. are empty for byte-level models
		ByteFallback bool `json:"byte_fallback"`
	} `json:"model"`
}

// Load reads a tokenizer.json.
func Load(path string) (*Tokenizer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse reads the content of a tokenizer.json.
func Parse(raw []byte) (*Tokenizer, error) {
	var f fileJSON
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("tokenizer.json: %w", err)
	}
	if f.Model.Type != "BPE" || f.Model.ByteFallback {
		return nil, fmt.Errorf("tokenizer.json: model %q not supported (byte-level BPE only)", f.Model.Type)
	}
	t := &Tokenizer{vocab: f.Model.Vocab, ranks: make(map[[2]string]int, len(f.Model.Merges)), cache: map[string][]int{}}
	for i, m := range f.Model.Merges {
		var pair [2]string
		var s string
		if json.Unmarshal(m, &s) == nil {
			a, b, ok := strings.Cut(s, " ")
			if !ok {
				return nil, fmt.Errorf("tokenizer.json: merge %q", s)
			}
			pair = [2]string{a, b}
		} else if err := json.Unmarshal(m, &pair); err != nil {
			return nil, fmt.Errorf("tokenizer.json: merge %d: %w", i, err)
		}
		if _, dup := t.ranks[pair]; !dup {
			t.ranks[pair] = i
		}
	}
	t.added = f.AddedTokens
	sort.SliceStable(t.added, func(i, j int) bool { return len(t.added[i].Content) > len(t.added[j].Content) })
	for _, a := range t.added {
		if a.SingleWord {
			return nil, fmt.Errorf("tokenizer.json: added token %q: single_word not supported", a.Content)
		}
	}

	var nz struct{ Type string }
	if len(f.Normalizer) > 0 && string(f.Normalizer) != "null" {
		if err := json.Unmarshal(f.Normalizer, &nz); err != nil {
			return nil, err
		}
		switch nz.Type {
		case "NFC":
			t.nfc = true
		default:
			return nil, fmt.Errorf("tokenizer.json: normalizer %q not supported", nz.Type)
		}
	}

	type preTok struct {
		Type          string                 `json:"type"`
		UseRegex      bool                   `json:"use_regex"`
		AddPrefix     bool                   `json:"add_prefix_space"`
		Behavior      string                 `json:"behavior"`
		Invert        bool                   `json:"invert"`
		Pattern       struct{ Regex string } `json:"pattern"`
		Pretokenizers []preTok               `json:"pretokenizers"`
	}
	var pt preTok
	if err := json.Unmarshal(f.PreTok, &pt); err != nil {
		return nil, fmt.Errorf("tokenizer.json: pre_tokenizer: %w", err)
	}
	switch {
	case pt.Type == "ByteLevel" && pt.UseRegex && !pt.AddPrefix:
		t.split = SplitGPT2
	case pt.Type == "Sequence" && len(pt.Pretokenizers) == 2 && pt.Pretokenizers[0].Type == "Split" &&
		(pt.Pretokenizers[0].Pattern.Regex == qwenPattern || pt.Pretokenizers[0].Pattern.Regex == qwen35Pattern) &&
		pt.Pretokenizers[0].Behavior == "Isolated" && !pt.Pretokenizers[0].Invert &&
		pt.Pretokenizers[1].Type == "ByteLevel" && !pt.Pretokenizers[1].UseRegex && !pt.Pretokenizers[1].AddPrefix:
		t.split = SplitQwen
		if pt.Pretokenizers[0].Pattern.Regex == qwen35Pattern {
			t.split = SplitQwen35
		}
	default:
		return nil, fmt.Errorf("tokenizer.json: pre_tokenizer %s not supported", string(f.PreTok))
	}

	if err := t.parsePost(f.PostProc); err != nil {
		return nil, err
	}
	for i, s := range bytesToUnicode() {
		t.byteMap[i] = s
	}
	return t, nil
}

// parsePost reads a TemplateProcessing "single" template (possibly inside a Sequence): the special tokens around $A.
func (t *Tokenizer) parsePost(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	type piece struct {
		SpecialToken *struct{ ID string } `json:"SpecialToken"`
		Sequence     *struct{ ID string } `json:"Sequence"`
	}
	type proc struct {
		Type       string  `json:"type"`
		Single     []piece `json:"single"`
		Processors []proc  `json:"processors"`
		Special    map[string]struct {
			IDs []int `json:"ids"`
		} `json:"special_tokens"`
	}
	var p proc
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("tokenizer.json: post_processor: %w", err)
	}
	procs := []proc{p}
	if p.Type == "Sequence" {
		procs = p.Processors
	}
	for _, q := range procs {
		switch q.Type {
		case "ByteLevel":
			// trim_offsets only: ids unchanged
		case "TemplateProcessing":
			seen := false
			for _, pc := range q.Single {
				switch {
				case pc.Sequence != nil:
					seen = true
				case pc.SpecialToken != nil:
					ids := q.Special[pc.SpecialToken.ID].IDs
					if seen {
						t.suffix = append(t.suffix, ids...)
					} else {
						t.prefix = append(t.prefix, ids...)
					}
				}
			}
		default:
			return fmt.Errorf("tokenizer.json: post_processor %q not supported", q.Type)
		}
	}
	return nil
}

// ID returns the id of a token (an added token or a vocabulary entry) and whether it exists.
func (t *Tokenizer) ID(token string) (int, bool) {
	for _, a := range t.added {
		if a.Content == token {
			return a.ID, true
		}
	}
	id, ok := t.vocab[token]
	return id, ok
}

// Encode turns text into token ids without special tokens (add_special_tokens=False).
func (t *Tokenizer) Encode(text string) []int {
	var out []int
	// 1. added tokens that match the raw text (special ones), 2. in the rest, after normalising, the normalised ones
	for _, seg := range t.splitAdded(text, false) {
		if seg.id >= 0 {
			out = append(out, seg.id)
			continue
		}
		s := seg.text
		if t.nfc {
			s = norm.NFC.String(s)
		}
		for _, seg2 := range t.splitAdded(s, true) {
			if seg2.id >= 0 {
				out = append(out, seg2.id)
				continue
			}
			for _, word := range t.preTokenize(seg2.text) {
				out = append(out, t.bpe(word)...)
			}
		}
	}
	return out
}

// EncodeSpecial encodes with the model's special tokens (add_special_tokens=True), keeping at most maxLen ids in
// total (0: no limit) — the text is cut, the special tokens stay (truncation=True, max_length).
func (t *Tokenizer) EncodeSpecial(text string, maxLen int) []int {
	ids := t.Encode(text)
	if maxLen > 0 {
		room := max(0, maxLen-len(t.prefix)-len(t.suffix))
		if len(ids) > room {
			ids = ids[:room]
		}
	}
	out := make([]int, 0, len(ids)+len(t.prefix)+len(t.suffix))
	out = append(out, t.prefix...)
	out = append(out, ids...)
	return append(out, t.suffix...)
}

type segment struct {
	text string
	id   int // −1: text to tokenise
}

// splitAdded cuts the added tokens out of s (leftmost, longest first), those with normalized == normalized.
func (t *Tokenizer) splitAdded(s string, normalized bool) []segment {
	var out []segment
	start := 0
	for i := 0; i < len(s); {
		matched := -1
		for k, a := range t.added {
			if a.Normalized != normalized || a.Content == "" {
				continue
			}
			if strings.HasPrefix(s[i:], a.Content) {
				matched = k
				break // sorted longest first
			}
		}
		if matched < 0 {
			_, w := utf8.DecodeRuneInString(s[i:])
			i += w
			continue
		}
		a := t.added[matched]
		from, to := i, i+len(a.Content)
		if a.LStrip { // the token takes the white space before it
			for from > start {
				r, w := utf8.DecodeLastRuneInString(s[start:from])
				if !unicode.IsSpace(r) {
					break
				}
				from -= w
			}
		}
		if a.RStrip { // and after it
			for to < len(s) {
				r, w := utf8.DecodeRuneInString(s[to:])
				if !unicode.IsSpace(r) {
					break
				}
				to += w
			}
		}
		if start < from {
			out = append(out, segment{s[start:from], -1})
		}
		out = append(out, segment{a.Content, a.ID})
		i, start = to, to
	}
	if start < len(s) {
		out = append(out, segment{s[start:], -1})
	}
	return out
}

// preTokenize splits s by the model's pattern and maps every word to the byte-level alphabet.
func (t *Tokenizer) preTokenize(s string) []string {
	var words []string
	switch t.split {
	case SplitQwen:
		words = splitQwen(s, isL, other)
	case SplitQwen35:
		words = splitQwen(s, isLM, otherM)
	default:
		words = splitGPT2(s)
	}
	for i, w := range words {
		var b strings.Builder
		for j := 0; j < len(w); j++ {
			b.WriteString(t.byteMap[w[j]])
		}
		words[i] = b.String()
	}
	return words
}

// bpe merges one byte-level word and returns its ids.
func (t *Tokenizer) bpe(word string) []int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ids, ok := t.cache[word]; ok {
		return ids
	}
	if id, ok := t.vocab[word]; ok {
		t.remember(word, []int{id})
		return []int{id}
	}
	var sym []string
	for _, r := range word {
		sym = append(sym, string(r))
	}
	for len(sym) > 1 {
		best, at := -1, -1
		for i := 0; i+1 < len(sym); i++ {
			if r, ok := t.ranks[[2]string{sym[i], sym[i+1]}]; ok && (best < 0 || r < best) {
				best, at = r, i
			}
		}
		if at < 0 {
			break
		}
		a, b := sym[at], sym[at+1]
		merged := sym[:0:0]
		for i := 0; i < len(sym); i++ {
			if i+1 < len(sym) && sym[i] == a && sym[i+1] == b {
				merged = append(merged, a+b)
				i++
				continue
			}
			merged = append(merged, sym[i])
		}
		sym = merged
	}
	ids := make([]int, 0, len(sym))
	for _, s := range sym {
		if id, ok := t.vocab[s]; ok {
			ids = append(ids, id)
		}
		// a byte-level vocabulary has every single byte: nothing is unknown
	}
	t.remember(word, ids)
	return ids
}

func (t *Tokenizer) remember(word string, ids []int) {
	if len(t.cache) > 50000 {
		t.cache = map[string][]int{}
	}
	t.cache[word] = ids
}

// bytesToUnicode is GPT-2's map from bytes to printable characters.
func bytesToUnicode() []string {
	var bs []int
	for b := '!'; b <= '~'; b++ {
		bs = append(bs, int(b))
	}
	for b := 0xA1; b <= 0xAC; b++ {
		bs = append(bs, b)
	}
	for b := 0xAE; b <= 0xFF; b++ {
		bs = append(bs, b)
	}
	in := map[int]bool{}
	for _, b := range bs {
		in[b] = true
	}
	out := make([]string, 256)
	n := 0
	for b := 0; b < 256; b++ {
		if in[b] {
			out[b] = string(rune(b))
		} else {
			out[b] = string(rune(256 + n))
			n++
		}
	}
	return out
}

// ---------- the split patterns ----------

func isL(r rune) bool  { return unicode.IsLetter(r) }
func isN(r rune) bool  { return unicode.IsNumber(r) }
func isS(r rune) bool  { return unicode.IsSpace(r) }
func isNL(r rune) bool { return r == '\r' || r == '\n' }

// contraction matches 's|'t|'re|'ve|'m|'ll|'d at rs[i:] (fold: case-insensitive) and returns its length in runes.
func contraction(rs []rune, i int, fold bool) int {
	if rs[i] != '\'' || i+1 >= len(rs) {
		return 0
	}
	low := func(k int) rune {
		if k >= len(rs) {
			return 0
		}
		if fold {
			return unicode.ToLower(rs[k])
		}
		return rs[k]
	}
	for _, c := range []string{"s", "t", "re", "ve", "m", "ll", "d"} {
		ok := true
		for j, r := range c {
			if low(i+1+j) != r {
				ok = false
				break
			}
		}
		if ok {
			return 1 + len(c)
		}
	}
	return 0
}

// run is the number of runes from i on that satisfy f.
func run(rs []rune, i int, f func(rune) bool) int {
	n := 0
	for i+n < len(rs) && f(rs[i+n]) {
		n++
	}
	return n
}

// spaces matches \s+(?!\S)|\s+ at i: a whitespace run, less its last space when a non-space follows.
func spaces(rs []rune, i int) int {
	n := run(rs, i, isS)
	if n > 1 && i+n < len(rs) {
		return n - 1
	}
	return n
}

func other(r rune) bool { return !isS(r) && !isL(r) && !isN(r) }

func isLM(r rune) bool   { return unicode.IsLetter(r) || unicode.IsMark(r) }
func otherM(r rune) bool { return !isS(r) && !isLM(r) && !isN(r) }

func splitGPT2(s string) []string {
	rs := []rune(s)
	var out []string
	for i := 0; i < len(rs); {
		n := contraction(rs, i, false)
		if n == 0 {
			sp := 0
			if rs[i] == ' ' && i+1 < len(rs) {
				sp = 1
			}
			switch c := rs[i+sp]; {
			case isL(c):
				n = sp + run(rs, i+sp, isL)
			case isN(c):
				n = sp + run(rs, i+sp, isN)
			case other(c):
				n = sp + run(rs, i+sp, other)
			default:
				n = spaces(rs, i)
			}
		}
		out = append(out, string(rs[i:i+n]))
		i += n
	}
	return out
}

// splitQwen splits by Qwen's pattern; letter is \p{L} (Qwen2/3) or [\p{L}\p{M}] (Qwen3.5), other the rest that is
// neither white space, letter nor number.
func splitQwen(s string, letter, other func(rune) bool) []string {
	rs := []rune(s)
	var out []string
	for i := 0; i < len(rs); {
		n := contraction(rs, i, true)
		if n == 0 {
			c := rs[i]
			switch {
			// [^\r\n\p{L}\p{N}]?\p{L}+
			case !isNL(c) && !isL(c) && !isN(c) && i+1 < len(rs) && letter(rs[i+1]):
				n = 1 + run(rs, i+1, letter)
			case letter(c):
				n = run(rs, i, letter)
			// \p{N}
			case isN(c):
				n = 1
			default:
				// ' ?[^\s\p{L}\p{N}]+[\r\n]*'
				sp := 0
				if c == ' ' && i+1 < len(rs) && other(rs[i+1]) {
					sp = 1
				}
				if other(rs[i+sp]) {
					n = sp + run(rs, i+sp, other)
					n += run(rs, i+n, isNL)
					break
				}
				// \s*[\r\n]+ : the whitespace run up to its last line break
				w := run(rs, i, isS)
				last := -1
				for k := 0; k < w; k++ {
					if isNL(rs[i+k]) {
						last = k
					}
				}
				if last >= 0 {
					n = last + 1
					break
				}
				n = spaces(rs, i)
			}
		}
		out = append(out, string(rs[i:i+n]))
		i += n
	}
	return out
}

// ---------- decoding ----------

func (t *Tokenizer) initDecode() {
	n := 0
	for _, id := range t.vocab {
		n = max(n, id+1)
	}
	for _, a := range t.added {
		n = max(n, a.ID+1)
	}
	t.tokens = make([]string, n)
	t.special = map[int]bool{}
	for s, id := range t.vocab {
		t.tokens[id] = s
	}
	t.unByte = map[rune]byte{}
	for b, s := range bytesToUnicode() {
		r, _ := utf8.DecodeRuneInString(s)
		t.unByte[r] = byte(b)
	}
	for _, a := range t.added {
		t.tokens[a.ID] = "\x00added" + a.Content
		if a.Special {
			t.special[a.ID] = true
		}
	}
}

// DecodeBytes turns token ids back into the bytes of the text (added tokens as their text; skipSpecial leaves out the
// special ones). A token may end inside a UTF-8 character: the bytes are only whole text together with the next ones.
func (t *Tokenizer) DecodeBytes(ids []int, skipSpecial bool) []byte {
	t.decOnce.Do(t.initDecode)
	var out []byte
	for _, id := range ids {
		if id < 0 || id >= len(t.tokens) {
			continue
		}
		if skipSpecial && t.special[id] {
			continue
		}
		s := t.tokens[id]
		if c, ok := strings.CutPrefix(s, "\x00added"); ok {
			out = append(out, c...)
			continue
		}
		for _, r := range s {
			if b, ok := t.unByte[r]; ok {
				out = append(out, b)
			}
		}
	}
	return out
}

// Decode turns token ids back into text.
func (t *Tokenizer) Decode(ids []int, skipSpecial bool) string {
	return strings.ToValidUTF8(string(t.DecodeBytes(ids, skipSpecial)), "\uFFFD")
}
