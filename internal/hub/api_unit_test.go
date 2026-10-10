package hub

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// The thought and the answer come apart wherever the pieces split the tags.
func TestThinkSplit(t *testing.T) {
	split := func(pieces []string) (thought, answer string) {
		var th, an strings.Builder
		sp := &thinkSplit{emit: func(isThought bool, s string) {
			if isThought {
				th.WriteString(s)
			} else {
				an.WriteString(s)
			}
		}}
		for _, p := range pieces {
			sp.write(p)
		}
		sp.flush()
		return th.String(), an.String()
	}
	for _, c := range []struct {
		text, thought, answer string
	}{
		{"<think>\nLet me think.\n</think>\n\nHello <b>you</b>!", "Let me think.", "Hello <b>you</b>!"},
		{"<think>Hmm\n\nyes.</think>\n\n", "Hmm\n\nyes.", ""},
		{"Just an answer, a < b and <thin", "", "Just an answer, a < b and <thin"},
		{"Answer first <think>not a thought</think>", "", "Answer first <think>not a thought</think>"},
		{"<think>cut off while thinking", "cut off while thinking", ""},
		{"\n\nleading newlines", "", "leading newlines"},
	} {
		// whole, then in every split of two, then one byte at a time
		th, an := split([]string{c.text})
		if th != c.thought || an != c.answer {
			t.Errorf("%q whole: %q / %q", c.text, th, an)
		}
		for i := 1; i < len(c.text); i++ {
			if th, an := split([]string{c.text[:i], c.text[i:]}); th != c.thought || an != c.answer {
				t.Errorf("%q split at %d: %q / %q", c.text, i, th, an)
			}
		}
		var bytes []string
		for i := range len(c.text) {
			bytes = append(bytes, c.text[i:i+1])
		}
		if th, an := split(bytes); th != c.thought || an != c.answer {
			t.Errorf("%q byte by byte: %q / %q", c.text, th, an)
		}
	}
}

func TestFindModel(t *testing.T) {
	ms := []fleetModel{
		{catalogModel: catalogModel{ID: "qwen3-embedding-0.6b", Kind: "embed"}},
		{catalogModel: catalogModel{ID: "laya-typed-decisions", Kind: "classify"}},
		{catalogModel: catalogModel{ID: "qwen3.6-35b-a3b", Kind: "chat"}},
		{catalogModel: catalogModel{ID: "qwen3.5-9b", Kind: "chat"}},
		{catalogModel: catalogModel{ID: "ollama/qwen3:8b", Kind: "chat"}},
		{catalogModel: catalogModel{ID: "lmstudio/Mistral-Small", Kind: "chat"}},
	}
	for name, want := range map[string]string{
		"qwen3-embedding-0.6b":      "qwen3-embedding-0.6b",
		"Qwen/Qwen3-Embedding-0.6B": "qwen3-embedding-0.6b",
		"QWEN3-EMBEDDING":           "qwen3-embedding-0.6b", // a unique prefix
		"typed-decisions":           "laya-typed-decisions",
		"vboard/typed-decisions":    "laya-typed-decisions",
		"qwen3.6":                   "qwen3.6-35b-a3b",
		"ollama/qwen3:8b":           "ollama/qwen3:8b",
		"lmstudio/mistral-small":    "lmstudio/Mistral-Small",
		"qwen3":                     "", // not unique
		"qwen3.5-9b-x":              "",
		"":                          "",
		"gpt-4o":                    "",
	} {
		m, ok := findModel(ms, name)
		if ok != (want != "") || m.ID != want {
			t.Errorf("%q: %q %v, want %q", name, m.ID, ok, want)
		}
	}
}

func TestFloat32Base64(t *testing.T) {
	v := []float32{0.6, -0.8, 1e-7, 0}
	b, err := base64.StdEncoding.DecodeString(float32Base64(v))
	if err != nil || len(b) != 16 {
		t.Fatalf("%d bytes, %v", len(b), err)
	}
	for i, x := range v {
		if got := math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:])); got != x {
			t.Errorf("%d: %v, want %v", i, got, x)
		}
	}
	// what numpy.frombuffer(base64.b64decode(s), dtype="<f4") reads for [1, 2]
	if s := float32Base64([]float32{1, 2}); s != "AACAPwAAAEA=" {
		t.Errorf("1, 2 = %q", s)
	}
}

// The criteria keep the order they were sent in (Laya reads the options in it), whatever their labels.
func TestLayaQuestions(t *testing.T) {
	qs, err := layaQuestions(json.RawMessage(`{"q2":{"type":"choice","instructions":"Which?","criteria":{"zebra":"z","apple":"a","mango":null,"apple":"A again"}},
		"q1":{"instructions":"Other","criteria":{"b":"","a":""}}}`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, q := range qs {
		var labels []string
		for _, o := range q.options {
			labels = append(labels, o["label"]+"="+o["description"])
		}
		got = append(got, q.id+":"+q.instructions+":"+strings.Join(labels, ","))
	}
	if strings.Join(got, " | ") != "q2:Which?:zebra=z,apple=A again,mango= | q1:Other:b=,a=" {
		t.Errorf("questions %v", got)
	}
	for _, bad := range []string{``, `[]`, `{}`, `{"q":{"type":"text","criteria":{"a":""}}}`, `{"q":{"criteria":{}}}`,
		`{"q":{"criteria":{"a":1}}}`, `{"q":{"criteria":["a"]}}`} {
		if _, err := layaQuestions(json.RawMessage(bad)); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

func TestClientMay(t *testing.T) {
	for _, c := range []struct {
		method, path string
		ok           bool
	}{
		{"GET", "/api/models", true},
		{"GET", "/api/jobs", true},
		{"POST", "/api/jobs", true},
		{"GET", "/api/jobs/m1.120000-1", true},
		{"POST", "/api/jobs/m1.120000-1/cancel", true},
		{"GET", "/api/events", true},
		{"GET", "/files/m1/out.png", true},
		{"DELETE", "/api/jobs/m1.120000-1", false},
		{"POST", "/api/jobs/m1.120000-1/title", false},
		{"POST", "/api/jobs/m1.120000-1/approve", false},
		{"GET", "/api/jobs/m1.120000-1/transcript", false},
		{"GET", "/api/workers", false},
		{"GET", "/api/clients", false},
		{"GET", "/api/devices", false},
		{"POST", "/api/upload", false},
		{"POST", "/api/unload", false},
		{"GET", "/api/prompts", false},
		{"GET", "/api/worker/events", false},
	} {
		r := httptestRequest(c.method, c.path)
		if got := clientMay(r); got != c.ok {
			t.Errorf("%s %s: %v", c.method, c.path, got)
		}
	}
}
