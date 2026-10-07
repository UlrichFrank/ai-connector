package laya

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/tokenizer"
)

// charModel is a Model with a tokenizer that makes one token per character (no merges) — enough to check how rows are
// built (laya/common.py build_sequence) without the checkpoint.
func charModel(t *testing.T, maxLen, headMaxLen int) *Model {
	t.Helper()
	vocab := map[string]int{} // printable ASCII and Ġ (the space) are all the tests use
	chars := `!"#$%&'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\]^_` + "`" + `abcdefghijklmnopqrstuvwxyz{|}~Ġ`
	for i, r := range chars {
		vocab[string(r)] = 1000 + i
	}
	raw, _ := json.Marshal(map[string]any{
		"pre_tokenizer": map[string]any{"type": "ByteLevel", "add_prefix_space": false, "use_regex": true},
		"added_tokens": []map[string]any{
			{"id": 1, "content": "[CLS]", "special": true}, {"id": 2, "content": "[SEP]", "special": true},
			{"id": 3, "content": "[MASK]", "special": true, "lstrip": true}, {"id": 4, "content": "[PAD]", "special": true},
		},
		"model": map[string]any{"type": "BPE", "vocab": vocab, "merges": []string{}},
	})
	tk, err := tokenizer.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &Model{tok: tk, cls: 1, sep: 2, mask: 3, pad: 4, cfg: agentConfig{MaxLen: maxLen, HeadMaxLen: headMaxLen,
		Temperature:   []float64{1.0148, 1.0374, 1.0575},
		TempByOptions: map[string]float64{"choice:2": 1.9063563346862793, "choice:3-5": 1.76, "choice:11+": 0.1006}}}
}

func TestQuestionRow(t *testing.T) {
	m := charModel(t, 1024, 256)
	ids, markers := m.question("ab", []Option{{"x", ""}, {"y", "z"}})
	// [CLS] "choice question: ab" [SEP] [MASK] " x" [MASK] " y: z" [SEP]
	head := m.tok.Encode("choice question: ab")
	want := append([]int{1}, head...)
	want = append(want, 2, 3)
	want = append(want, m.tok.Encode(" x")...)
	want = append(want, 3)
	want = append(want, m.tok.Encode(" y: z")...)
	want = append(want, 2)
	if !slices.Equal(ids, want) {
		t.Fatalf("row\n got %v\nwant %v", ids, want)
	}
	if !slices.Equal(markers, []int{len(head) + 2, len(head) + 5}) {
		t.Fatalf("markers %v", markers)
	}
	// [MASK] in a text is a space for the model (it marks the options)
	a, _ := m.question("a[MASK]b", []Option{{"x", ""}})
	b, _ := m.question("a b", []Option{{"x", ""}})
	if !slices.Equal(a, b) {
		t.Fatalf("[MASK] in instructions: %v vs %v", a, b)
	}
}

func TestQuestionBudget(t *testing.T) {
	m := charModel(t, 1024, 64)
	long := strings.Repeat("o", 100)
	// an option is cut at 48 tokens; with less than 16 left for the instructions, every option at (64−16)/2 = 24, and
	// the instructions get what remains (64 − 48 = 16)
	ids, markers := m.question(strings.Repeat("i", 50), []Option{{long, ""}, {long, ""}})
	if len(markers) != 2 || markers[1]-markers[0] != 24 {
		t.Fatalf("options not cut to 24: markers %v", markers)
	}
	if markers[0] != 1+16+1 {
		t.Fatalf("instructions not cut to 16: first marker at %d", markers[0])
	}
	if len(ids) != markers[1]+24+1 {
		t.Fatalf("row length %d", len(ids))
	}
}

func TestSequenceTruncation(t *testing.T) {
	m := charModel(t, 40, 256)
	head, markers := m.question("q", []Option{{"a", ""}, {"b", ""}})
	s := m.sequence(head, markers, strings.Repeat("s", 100))
	if len(s.ids) != 40 || s.ids[39] != 2 || !slices.Equal(s.ids[:len(head)], head) {
		t.Fatalf("state not cut to max_len with a final [SEP]: %v", s.ids)
	}
	short := m.sequence(head, markers, "hi")
	if len(short.ids) != len(head)+3 {
		t.Fatalf("short state: %v", short.ids)
	}
}

// Probabilities: softmax of logit/temperature with laya's buckets (clamped to [0.5, 5]); the choice is the first
// maximum; answer_confidence is its probability; four decimals like the Python package.
func TestAnswer(t *testing.T) {
	m := charModel(t, 1024, 256)
	a := m.answer([]float32{1, 0}, []Option{{"A", ""}, {"B", ""}})
	p := 1 / (1 + math.Exp(-1/1.9063563346862793))
	if a.Choice != "A" || a.Probabilities["A"] != math.Round(p*1e4)/1e4 || a.AnswerConfidence != a.Probabilities["A"] {
		t.Fatalf("two options: %+v (want P(A)=%.4f)", a, p)
	}
	if b := m.answer([]float32{2, 2, 1}, []Option{{"x", ""}, {"y", ""}, {"z", ""}}); b.Choice != "x" {
		t.Fatalf("tie: %+v", b)
	}
	if got := m.temperature(0, 12); got != 0.5 {
		t.Fatalf("choice:11+ = %v, want the clamp 0.5", got)
	}
	if got := m.temperature(0, 7); got != 1.0148 {
		t.Fatalf("no bucket for choice:6-10: %v, want temperature[0]", got)
	}
	if got := m.temperature(0, 4); got != 1.76 {
		t.Fatalf("choice:3-5 = %v", got)
	}
}
