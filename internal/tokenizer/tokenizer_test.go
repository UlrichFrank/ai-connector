package tokenizer

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// The split patterns, compared with the tokenizers library's pre-tokenisers (ByteLevel(use_regex=True) and Qwen's
// Split) on the same texts; the parity test compares whole tokenizers when the model files are there.
func TestSplit(t *testing.T) {
	for _, c := range []struct {
		text       string
		gpt2, qwen []string
	}{
		{"Hello world", []string{"Hello", " world"}, []string{"Hello", " world"}},
		{"a   b\t\tc\n\nd", []string{"a", "  ", " b", "\t", "\t", "c", "\n", "\n", "d"}, []string{"a", "  ", " b", "\t", "\tc", "\n\n", "d"}},
		{"I'm sure they'LL", []string{"I", "'m", " sure", " they", "'", "LL"}, []string{"I", "'m", " sure", " they", "'LL"}},
		{"10:32 — 500 errors", []string{"10", ":", "32", " —", " 500", " errors"}, []string{"1", "0", ":", "3", "2", " —", " ", "5", "0", "0", " errors"}},
		{"x  \n  y", []string{"x", "  \n ", " y"}, []string{"x", "  \n", " ", " y"}},
		{"Größe: Äpfel!", []string{"Größe", ":", " Äpfel", "!"}, []string{"Größe", ":", " Äpfel", "!"}},
		{"(hello) [x]", []string{"(", "hello", ")", " [", "x", "]"}, []string{"(hello", ")", " [", "x", "]"}},
		{"  ...  ", []string{" ", " ...", "  "}, []string{" ", " ...", "  "}},
		{"1 2  3", []string{"1", " 2", " ", " 3"}, []string{"1", " ", "2", " ", " ", "3"}},
		{"end ", []string{"end", " "}, []string{"end", " "}},
	} {
		if got := splitGPT2(c.text); !slices.Equal(got, c.gpt2) {
			t.Errorf("GPT-2 %q: %q, want %q", c.text, got, c.gpt2)
		}
		if got := splitQwen(c.text, isL, other); !slices.Equal(got, c.qwen) {
			t.Errorf("Qwen %q: %q, want %q", c.text, got, c.qwen)
		}
	}
}

// tiny builds a byte-level BPE tokenizer.json: every printable ASCII character, a few merges, added tokens.
func tiny(t *testing.T, preTok, post string) *Tokenizer {
	t.Helper()
	vocab := map[string]int{}
	for i, s := range bytesToUnicode() {
		vocab[s] = i
	}
	merges := [][2]string{{"l", "o"}, {"lo", "w"}, {"Ġ", "low"}, {"e", "r"}, {"Ġlow", "er"}}
	for _, m := range merges {
		vocab[m[0]+m[1]] = len(vocab)
	}
	raw, _ := json.Marshal(map[string]any{
		"normalizer":     map[string]string{"type": "NFC"},
		"pre_tokenizer":  json.RawMessage(preTok),
		"post_processor": json.RawMessage(post),
		"added_tokens": []map[string]any{
			{"id": 900, "content": "[MASK]", "special": true, "normalized": false, "lstrip": true, "rstrip": false, "single_word": false},
			{"id": 901, "content": "[SEP]", "special": true, "normalized": false},
			{"id": 902, "content": "   ", "special": false, "normalized": true},
		},
		"model": map[string]any{"type": "BPE", "vocab": vocab, "merges": merges},
	})
	tk, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

const byteLevel = `{"type": "ByteLevel", "add_prefix_space": false, "trim_offsets": true, "use_regex": true}`

func TestEncode(t *testing.T) {
	tk := tiny(t, byteLevel, `{"type": "TemplateProcessing", "single": [{"SpecialToken": {"id": "[MASK]", "type_id": 0}}, {"Sequence": {"id": "A", "type_id": 0}}, {"SpecialToken": {"id": "[SEP]", "type_id": 0}}], "special_tokens": {"[MASK]": {"id": "[MASK]", "ids": [900]}, "[SEP]": {"id": "[SEP]", "ids": [901]}}}`)
	id := func(s string) int { i, _ := tk.ID(s); return i }
	// "lower" merges to "low"+"er", " lower" to "Ġlower" (all five merges)
	if got, want := tk.Encode("lower lower"), []int{id("low"), id("er"), id("Ġlower")}; !slices.Equal(got, want) {
		t.Fatalf("merges: %v, want %v", got, want)
	}
	// added tokens: [MASK] takes the space before it (lstrip), a run of three spaces is one token (normalized)
	if got, want := tk.Encode("a [MASK]b   c"), []int{id("a"), 900, id("b"), 902, id("c")}; !slices.Equal(got, want) {
		t.Fatalf("added: %v, want %v", got, want)
	}
	// NFC: e + combining acute is é (two bytes, both in the byte alphabet)
	if got := tk.Encode("é"); len(got) != 2 {
		t.Fatalf("NFC: %v", got)
	}
	// the template around the text, and truncation keeps the special tokens
	if got := tk.EncodeSpecial("lower lower", 3); !slices.Equal(got, []int{900, id("low"), 901}) {
		t.Fatalf("special + truncation: %v", got)
	}
}

// Qwen3.5's pattern counts combining marks as letters (U+0301 after "e" stays in the word).
func TestSplitQwen35(t *testing.T) {
	text := "éx née 12"
	if got, want := splitQwen(text, isL, other), []string{"e", "́x", " ne", "́e", " ", "1", "2"}; !slices.Equal(got, want) {
		t.Errorf("Qwen: %q, want %q", got, want)
	}
	if got, want := splitQwen(text, isLM, otherM), []string{"éx", " née", " ", "1", "2"}; !slices.Equal(got, want) {
		t.Errorf("Qwen3.5: %q, want %q", got, want)
	}
}

func TestDecode(t *testing.T) {
	tk := tiny(t, byteLevel, `null`)
	for _, text := range []string{"lower lower", "Größe: 10 €", "a[MASK]b   c"} {
		ids := tk.Encode(text)
		if got := tk.Decode(ids, false); got != text {
			t.Errorf("decode(encode(%q)) = %q", text, got)
		}
	}
	if got := tk.Decode(tk.Encode("a [MASK]b"), true); got != "ab" {
		t.Errorf("skip special: %q", got)
	}
	// a character split over several tokens: the bytes are whole only together
	ids := tk.Encode("€")
	if b := tk.DecodeBytes(ids[:1], false); len(b) != 1 {
		t.Errorf("partial: %v", b)
	}
}

func TestParseRejects(t *testing.T) {
	for _, c := range []struct{ name, json string }{
		{"unigram", `{"model": {"type": "Unigram"}}`},
		{"byte fallback", `{"model": {"type": "BPE", "byte_fallback": true}}`},
		{"other split", `{"pre_tokenizer": {"type": "Whitespace"}, "model": {"type": "BPE", "vocab": {}, "merges": []}}`},
		{"normalizer", `{"normalizer": {"type": "Lowercase"}, "pre_tokenizer": ` + byteLevel + `, "model": {"type": "BPE", "vocab": {}, "merges": []}}`},
	} {
		if _, err := Parse([]byte(c.json)); err == nil {
			t.Errorf("%s accepted", c.name)
		} else if !strings.Contains(err.Error(), "tokenizer.json") {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
