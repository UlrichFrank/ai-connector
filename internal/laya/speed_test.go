package laya

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

// TestSpeed times Choose on the CPU and the GPU for board-sized batches of short and long notes and checks that both
// give the same answers: MLXLAB_LAYA=<checkpoint dir> MLXLAB_BENCH=1 go test -run Speed -v ./internal/laya
// (MLXLAB_BENCH_SIZES="1,7,50", MLXLAB_BENCH_BATCH=32).
func TestSpeed(t *testing.T) {
	dir := os.Getenv("MLXLAB_LAYA")
	if dir == "" || os.Getenv("MLXLAB_BENCH") == "" {
		t.Skip("MLXLAB_LAYA / MLXLAB_BENCH not set")
	}
	_ = tensor.UseGPU(true, 0) // before loading: the weights go into the GPU's memory
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	opts := []Option{{"Went well", "things that went well in the sprint, e.g. Team spirit was great"},
		{"To improve", "problems and things to improve, e.g. The build is slow"}, {"Action items", ""}}
	batch := 32
	if n, err := strconv.Atoi(os.Getenv("MLXLAB_BENCH_BATCH")); err == nil {
		batch = n
	}
	for _, kind := range []string{"short", "long"} {
		for _, n := range BenchSizes() {
			states := make([]string, n)
			for i := range states {
				states[i] = `{"text": "` + Note(kind, i) + `"}`
			}
			var ref []Answer
			for _, dev := range []string{"cpu", "gpu"} {
				if err := tensor.UseGPU(dev == "gpu", 0); err != nil {
					t.Skip(err)
				}
				m.Choose(states[:1], "Which frame does this sticky note belong to?", opts, batch) // warm (weights to the GPU)
				tensor.ResetProfile()
				start := time.Now()
				got, err := m.Choose(states, "Which frame does this sticky note belong to?", opts, batch)
				if err != nil {
					t.Fatal(err)
				}
				el := time.Since(start)
				p := tensor.ReadProfile()
				diff, same := 0.0, 0
				if ref == nil {
					ref = got
				} else {
					for i := range got {
						if got[i].Choice == ref[i].Choice {
							same++
						}
						for k, v := range ref[i].Probabilities {
							diff = math.Max(diff, math.Abs(v-got[i].Probabilities[k]))
						}
					}
				}
				fmt.Printf("laya %-5s n=%-4d %-3s %8.0f ms  linear %6.0f  attention %6.0f  rest %6.0f  (gpu encode %4.0f wait %6.0f copy %4.0f, %d command buffers, %d new buffers)",
					kind, n, dev, ms(el), ms(p.Linear), ms(p.Attention), ms(el-p.Linear-p.Attention), ms(p.GPUEncode), ms(p.GPUWait),
					ms(p.GPUCopy), p.GPUCalls, p.GPUAllocs)
				if dev == "gpu" {
					fmt.Printf("  same choice %d/%d, max |Δp| %.4f", same, n, diff)
					if same != n || diff > 2e-3 {
						t.Errorf("GPU differs from CPU: %d/%d choices, max |Δp| %.4f", same, n, diff)
					}
				}
				fmt.Println()
			}
			// several requests at once on the GPU (each with MLXLAB_OVERLAP batches in flight): the same answers
			if k := concurrent(); k > 1 && n <= 200 {
				start := time.Now()
				res := make([][]Answer, k)
				var wg sync.WaitGroup
				for i := range k {
					wg.Go(func() {
						var err error
						if res[i], err = m.Choose(states, "Which frame does this sticky note belong to?", opts, batch); err != nil {
							t.Error(err)
						}
					})
				}
				wg.Wait()
				same, diff := 0, 0.0
				for _, got := range res {
					for i := range got {
						if got[i].Choice == ref[i].Choice {
							same++
						}
						for key, v := range ref[i].Probabilities {
							diff = math.Max(diff, math.Abs(v-got[i].Probabilities[key]))
						}
					}
				}
				fmt.Printf("laya %-5s n=%-4d gpu x%d at once %8.0f ms  same choice %d/%d, max |Δp| %.4f\n", kind, n, k, ms(time.Since(start)),
					same, k*n, diff)
				if same != k*n || diff > 2e-3 {
					t.Errorf("concurrent GPU requests differ from CPU: %d/%d choices, max |Δp| %.4f", same, k*n, diff)
				}
			}
		}
	}
	tensor.UseGPU(false, 0)
}

func ms(d time.Duration) float64 { return float64(d) / 1e6 }

// concurrent is the number of simultaneous requests of the concurrency check (MLXLAB_BENCH_CONCURRENT, 3).
func concurrent() int {
	if n, err := strconv.Atoi(os.Getenv("MLXLAB_BENCH_CONCURRENT")); err == nil {
		return n
	}
	return 3
}

// TestLoadCost: load time, the first and second answer (the first hands the weights to the GPU) and the peak memory,
// of the backend MLXLAB_BLAS selects — one process per backend.
func TestLoadCost(t *testing.T) {
	dir := os.Getenv("MLXLAB_LAYA")
	if dir == "" || os.Getenv("MLXLAB_BENCH") == "" {
		t.Skip("MLXLAB_LAYA / MLXLAB_BENCH not set")
	}
	start := time.Now()
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	load := time.Since(start)
	opts := []Option{{"Went well", ""}, {"To improve", ""}}
	states := []string{`{"text": "` + Note("long", 0) + `"}`}
	start = time.Now()
	m.Choose(states, "Which frame?", opts, 32)
	first := time.Since(start)
	start = time.Now()
	m.Choose(states, "Which frame?", opts, 32)
	second := time.Since(start)
	fmt.Printf("laya %s: load %.0f ms, first answer %.0f ms, second %.0f ms, peak RSS %.0f MB, %d GPU buffers set aside\n", tensor.Backend(), ms(load),
		ms(first), ms(second), maxRSS(), tensor.ReadProfile().GPUSetAside)
}

// BenchSizes are the batch sizes (MLXLAB_BENCH_SIZES, default 1,7,50,200,500).
func BenchSizes() []int {
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

// Note is the i-th benchmark note: short (a few words, ~10 tokens) or long (~200 tokens).
func Note(kind string, i int) string {
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
