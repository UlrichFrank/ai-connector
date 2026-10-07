//go:build darwin && arm64

package llm

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// Parity of the logits with mlx-lm: MLXLAB_LLM_DIR=<model folder> MLXLAB_LLM_LOGITS=<file> ({"ids": […],
// "logits": […] of the last position, "logits2": the same with the prompt run in two halves through the cache}).
func TestLogits(t *testing.T) {
	dir, ref := os.Getenv("MLXLAB_LLM_DIR"), os.Getenv("MLXLAB_LLM_LOGITS")
	if dir == "" || ref == "" {
		t.Skip("MLXLAB_LLM_DIR / MLXLAB_LLM_LOGITS not set")
	}
	raw, err := os.ReadFile(ref)
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Prompt          string
		IDs             []int
		Logits, Logits2 []float32
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	e, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if ids := e.tok.Encode(r.Prompt); !slices.Equal(ids, r.IDs) {
		t.Fatalf("prompt tokens differ:\n%v\n%v", ids, r.IDs)
	}
	for _, split := range []int{len(r.IDs), len(r.IDs) / 2} {
		var got []float32
		err := e.do(func() error {
			c := &cache{}
			defer c.free()
			var l array
			for from := 0; from < len(r.IDs); from += split {
				l = e.md.forward(idsArray(r.IDs[from:min(from+split, len(r.IDs))]), c)
			}
			got = l.Float32s()
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		maxDiff, argGot, argRef := float32(0), 0, 0
		var sum, self float64
		for i := range got {
			maxDiff = max(maxDiff, abs32(got[i]-r.Logits[i]))
			sum += float64(abs32(got[i] - r.Logits[i]))
			if len(r.Logits2) == len(got) {
				self += float64(abs32(r.Logits2[i] - r.Logits[i]))
			}
			if got[i] > got[argGot] {
				argGot = i
			}
			if r.Logits[i] > r.Logits[argRef] {
				argRef = i
			}
		}
		t.Logf("split %d: max |Δlogit| %.3f, mean %.4f (mlx-lm whole vs. halves: mean %.4f), top token %d (mlx-lm %d), top logit %.3f (mlx-lm %.3f)",
			split, maxDiff, sum/float64(len(got)), self/float64(len(got)), argGot, argRef, got[argGot], r.Logits[argRef])
		if argGot != argRef || maxDiff > 1.0 {
			t.Errorf("split %d: logits differ from mlx-lm (max %.3f)", split, maxDiff)
		}
	}
}

// Speed: MLXLAB_LLM_DIR=<model folder> MLXLAB_LLM_BENCH=<prompt file> go test -run TestBench -v ./internal/llm
// (twice: the second run has the kernels compiled and the memory warm; the prompt cache is dropped in between).
func TestBench(t *testing.T) {
	dir, p := os.Getenv("MLXLAB_LLM_DIR"), os.Getenv("MLXLAB_LLM_BENCH")
	if dir == "" || p == "" {
		t.Skip("MLXLAB_LLM_DIR / MLXLAB_LLM_BENCH not set")
	}
	prompt, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	e, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	t.Logf("load %.1f s", time.Since(start).Seconds())
	for run := 0; run < 2; run++ {
		e.prompt = nil // no prompt cache
		st, err := e.Generate(context.Background(), string(prompt), Options{MaxTokens: 100}, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		_, peak := e.Memory()
		t.Logf("run %d: prompt %d tokens at %.0f tokens/s (first token %.2f s), %d tokens at %.1f tokens/s, peak %.1f GB",
			run, st.PromptTokens, st.PrefillPerSecond(), st.FirstToken.Seconds(), st.Tokens, st.TokensPerSecond(), float64(peak)/(1<<30))
	}
	if prof {
		t.Log(ProfReport())
	}
}

func abs32(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}

// With a real model: MLXLAB_LLM_DIR=<model folder> go test -run TestGenerate -v ./internal/llm
// (e.g. ~/.lmstudio/models/mlx-community/Qwen3.6-35B-A3B-4bit). MLXLAB_LLM_REF=<file> compares the greedy answer
// with mlx-lm's ({"prompt": …, "text": …}, written by mlx_lm.generate with temperature 0).
func TestGenerate(t *testing.T) {
	dir := os.Getenv("MLXLAB_LLM_DIR")
	if dir == "" {
		t.Skip("MLXLAB_LLM_DIR not set")
	}
	start := time.Now()
	e, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	act, _ := e.Memory()
	t.Logf("loaded %s (%s) in %.1f s, %.1f GB", dir, e.Cfg.ModelType, time.Since(start).Seconds(), float64(act)/(1<<30))

	var ref struct{ Prompt, Text string }
	if p := os.Getenv("MLXLAB_LLM_REF"); p != "" {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &ref); err != nil {
			t.Fatal(err)
		}
	} else {
		ref.Prompt = Render(e.Format, []Message{{Role: "user", Content: "Name three colours of the rainbow, comma-separated."}}, nil, false)
	}
	var out strings.Builder
	st, err := e.Generate(context.Background(), ref.Prompt, Options{MaxTokens: 80}, func(s string) { out.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%q", out.String())
	t.Logf("prompt %d tokens, first token %.2f s, %d tokens, %.1f tokens/s", st.PromptTokens, st.FirstToken.Seconds(), st.Tokens, st.TokensPerSecond())
	if prof {
		t.Log(ProfReport())
	}
	// bf16 rounding differs a little between any two implementations (mlx-lm itself answers differently with the
	// prompt in one piece or two): greedy answers agree at the start, TestLogits checks the numbers
	if n := min(24, len(ref.Text)); ref.Text != "" && !strings.HasPrefix(out.String(), ref.Text[:n]) {
		t.Errorf("differs from mlx-lm:\n got %q\nwant %q", out.String(), ref.Text)
	}

	// a follow-up round continues from the cached prompt
	prompt2 := ref.Prompt + out.String() + "<|im_end|>\n<|im_start|>user\nAnd one more?<|im_end|>\n<|im_start|>assistant\n"
	out.Reset()
	st2, err := e.Generate(context.Background(), prompt2, Options{MaxTokens: 30}, func(s string) { out.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("round 2: %q (cached %d of %d prompt tokens, first token %.2f s)", out.String(), st2.CachedTokens, st2.PromptTokens, st2.FirstToken.Seconds())
	if st2.CachedTokens != st.PromptTokens {
		t.Errorf("round 2 reused %d tokens, want %d", st2.CachedTokens, st.PromptTokens)
	}
}
