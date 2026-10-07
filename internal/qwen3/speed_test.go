package qwen3

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/tensor"
)

// TestSpeed times Embed on the CPU and the GPU for board-sized batches of short and long notes and checks that both
// give the same vectors: MLXLAB_QWEN=<model dir> MLXLAB_BENCH=1 go test -run Speed -v ./internal/qwen3
// (MLXLAB_BENCH_SIZES="1,7,50", MLXLAB_BENCH_BATCH=32).
func TestSpeed(t *testing.T) {
	dir := os.Getenv("MLXLAB_QWEN")
	if dir == "" || os.Getenv("MLXLAB_BENCH") == "" {
		t.Skip("MLXLAB_QWEN / MLXLAB_BENCH not set")
	}
	_ = tensor.UseGPU(true, 0) // before loading: the weights go into the GPU's memory
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	batch := 32
	if n, err := strconv.Atoi(os.Getenv("MLXLAB_BENCH_BATCH")); err == nil {
		batch = n
	}
	for _, kind := range []string{"short", "long"} {
		for _, n := range benchSizes() {
			texts := make([]string, n)
			tokens := 0
			for i := range texts {
				texts[i] = note(kind, i)
				tokens += len(m.tok.EncodeSpecial(texts[i], MaxTokens))
			}
			start := time.Now()
			for _, s := range texts {
				m.tok.EncodeSpecial(s, MaxTokens)
			}
			tok := time.Since(start)
			var ref [][]float32
			for _, dev := range []string{"cpu", "gpu"} {
				if err := tensor.UseGPU(dev == "gpu", 0); err != nil {
					t.Skip(err)
				}
				m.Embed(texts[:1], batch)
				tensor.ResetProfile()
				start := time.Now()
				got := m.Embed(texts, batch)
				el := time.Since(start)
				p := tensor.ReadProfile()
				fmt.Printf("qwen %-5s n=%-4d %-3s %8.0f ms  linear %6.0f  attention %6.0f  tokens %4.0f  rest %6.0f  (gpu encode %4.0f wait %6.0f copy %4.0f, %d command buffers, %d new buffers; %d tokens)",
					kind, n, dev, ms(el), ms(p.Linear), ms(p.Attention), ms(tok), ms(el-p.Linear-p.Attention-tok), ms(p.GPUEncode), ms(p.GPUWait),
					ms(p.GPUCopy), p.GPUCalls, p.GPUAllocs, tokens)
				if ref == nil {
					ref = got
				} else {
					worst, bad := minCosine(got, ref)
					fmt.Printf("  min cosine %.6f (%d NaN)", worst, bad)
					if !(worst >= 0.9999) {
						t.Errorf("GPU differs from CPU: cosine %.6f", worst)
					}
				}
				fmt.Println()
			}
			// several requests at once on the GPU (each with MLXLAB_OVERLAP batches in flight): the same vectors
			if k := concurrent(); k > 1 && n <= 200 {
				start := time.Now()
				res := make([][][]float32, k)
				var wg sync.WaitGroup
				for i := range k {
					wg.Go(func() { res[i] = m.Embed(texts, batch) })
				}
				wg.Wait()
				worst, bad := 1.0, 0
				for _, got := range res {
					w, b := minCosine(got, ref)
					worst, bad = math.Min(worst, w), bad+b
				}
				fmt.Printf("qwen %-5s n=%-4d gpu x%d at once %8.0f ms  min cosine %.6f (%d NaN)\n", kind, n, k, ms(time.Since(start)), worst, bad)
				if !(worst >= 0.9999) || bad > 0 {
					t.Errorf("concurrent GPU requests differ from CPU: cosine %.6f, %d NaN", worst, bad)
				}
			}
		}
	}
	tensor.UseGPU(false, 0)
}

func ms(d time.Duration) float64 { return float64(d) / 1e6 }

// minCosine is the smallest cosine between got's and ref's vectors (NaN counts as -1), and the number of NaN vectors.
func minCosine(got, ref [][]float32) (float64, int) {
	worst, bad := 1.0, 0
	for i := range got {
		var c float64
		for j := range got[i] {
			c += float64(got[i][j]) * float64(ref[i][j])
		}
		if c != c {
			bad++
			c = -1
		}
		worst = math.Min(worst, c)
	}
	return worst, bad
}

// concurrent is the number of simultaneous requests of the concurrency check (MLXLAB_BENCH_CONCURRENT, 3).
func concurrent() int {
	if n, err := strconv.Atoi(os.Getenv("MLXLAB_BENCH_CONCURRENT")); err == nil {
		return n
	}
	return 3
}

// TestLoadCost: load time, the first and second embedding (the first hands the weights to the GPU) and the peak
// memory, of the backend MLXLAB_BLAS selects — one process per backend.
func TestLoadCost(t *testing.T) {
	dir := os.Getenv("MLXLAB_QWEN")
	if dir == "" || os.Getenv("MLXLAB_BENCH") == "" {
		t.Skip("MLXLAB_QWEN / MLXLAB_BENCH not set")
	}
	start := time.Now()
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	load := time.Since(start)
	texts := []string{note("long", 0)}
	start = time.Now()
	m.Embed(texts, 32)
	first := time.Since(start)
	start = time.Now()
	m.Embed(texts, 32)
	second := time.Since(start)
	fmt.Printf("qwen %s: load %.0f ms, first embedding %.0f ms, second %.0f ms, peak RSS %.0f MB, %d GPU buffers set aside\n", tensor.Backend(), ms(load),
		ms(first), ms(second), maxRSS(), tensor.ReadProfile().GPUSetAside)
}

func benchSizes() []int {
	s := os.Getenv("MLXLAB_BENCH_SIZES")
	if s == "" {
		s = "1,7,50,200,500"
	}
	var out []int
	for _, f := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

var shortNotes = []string{"The build is slow", "Team spirit was great", "Retro every two weeks", "Fix the flaky login test",
	"Pairing helped a lot", "Too many meetings", "Deploys on Friday are scary", "Write down decisions",
	"Great demo to the customer", "Onboarding docs are outdated"}

// note is the i-th benchmark note: short (a few words) or long (~200 tokens).
func note(kind string, i int) string {
	s := shortNotes[i%len(shortNotes)] + fmt.Sprintf(" (#%d)", i)
	if kind == "short" {
		return s
	}
	var b strings.Builder
	b.WriteString(s + ": ")
	for j := 0; b.Len() < 900; j++ {
		b.WriteString(shortNotes[(i+j*3)%len(shortNotes)])
		b.WriteString(fmt.Sprintf(", and in sprint %d we noticed that this keeps coming back. ", (i+j)%40))
	}
	return b.String()
}
