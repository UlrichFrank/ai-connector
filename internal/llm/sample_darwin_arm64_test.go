//go:build darwin && arm64

package llm

import (
	"math"
	"os"
	"testing"

	"github.com/philipparndt/ai-connector/internal/mlx"
)

func openMLX(t *testing.T) {
	t.Helper()
	if os.Getenv("MLXLAB_MLX") == "0" {
		t.Skip("MLXLAB_MLX=0")
	}
	if _, err := mlx.Open(); err != nil {
		t.Fatal(err)
	}
}

// The candidates top-k, top-p and min-p leave, on the GPU.
func TestSamplerFilter(t *testing.T) {
	openMLX(t)
	defer mlx.FreeTemps()
	// probabilities 0.4, 0.3, 0.15, 0.1, 0.05, ~0 (token 5) → logits = log p
	p := []float32{0.4, 0.3, 0.15, 0.1, 0.05, 1e-6}
	l := make([]float32, len(p))
	for i, v := range p {
		l[i] = float32(math.Log(float64(v)))
	}
	kept := func(o Options) map[int]bool {
		s := newSampler(o)
		vals, idx := s.filter(mlx.FromFloat32(l, 1, len(l)))
		v, id := vals.Float32s(), mlx.AsType(idx, mlx.Int32).Int32s()
		out := map[int]bool{}
		for i := range v {
			if !math.IsInf(float64(v[i]), -1) {
				out[int(id[i])] = true
			}
		}
		return out
	}
	for _, c := range []struct {
		name string
		o    Options
		want []int
	}{
		{"top-k 4", Options{Temperature: 1, TopK: 4}, []int{0, 1, 2, 3}},
		{"top-p 0.8", Options{Temperature: 1, TopK: 20, TopP: 0.8}, []int{0, 1, 2}}, // 0.4+0.3 = 0.7 < 0.8: the third is in
		{"top-p 0.65", Options{Temperature: 1, TopK: 20, TopP: 0.65}, []int{0, 1}},
		{"top-p tiny", Options{Temperature: 1, TopK: 20, TopP: 0.01}, []int{0}},
		{"top-k then top-p", Options{Temperature: 1, TopK: 2, TopP: 0.95}, []int{0, 1}},
		{"min-p 0.3", Options{Temperature: 1, TopK: 20, MinP: 0.3}, []int{0, 1, 2}}, // ≥ 0.12
	} {
		got := kept(c.o)
		if len(got) != len(c.want) {
			t.Errorf("%s: kept %v, want %v", c.name, got, c.want)
			continue
		}
		for _, w := range c.want {
			if !got[w] {
				t.Errorf("%s: kept %v, want %v", c.name, got, c.want)
			}
		}
	}
}

// A presence penalty pushes the tokens already written down: greedy decoding over fixed logits walks down the
// ranking instead of repeating the first token, and the mask stays lazy until evaluated.
func TestSamplerPresence(t *testing.T) {
	openMLX(t)
	defer mlx.FreeTemps()
	l := mlx.FromFloat32([]float32{5, 4, 3, 2, 1}, 1, 5)
	s := newSampler(Options{PresencePenalty: 1.5})
	defer s.free()
	var got []int
	for range 4 {
		y := s.next(l)
		mlx.Eval(append([]array{y}, s.state()...)...)
		got = append(got, y.ItemInt())
	}
	// 5,4,3 → token 0 (now 3.5) → token 1 (now 2.5) → token 0 again: present once is penalised once, not more
	want := []int{0, 1, 0, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokens %v, want %v", got, want)
		}
	}
	// repetition: 5/2 = 2.5 < 4
	r := newSampler(Options{RepetitionPenalty: 2})
	defer r.free()
	a := r.next(l).ItemInt()
	b := r.next(l).ItemInt()
	if a != 0 || b != 1 {
		t.Fatalf("repetition penalty: %d %d", a, b)
	}
}

// Sampling at a temperature follows the probabilities among the kept candidates only.
func TestSamplerDistribution(t *testing.T) {
	openMLX(t)
	defer mlx.FreeTemps()
	mlx.Seed(7)
	p := []float32{0.5, 0.3, 0.2}
	l := make([]float32, len(p))
	for i, v := range p {
		l[i] = float32(math.Log(float64(v)))
	}
	s := newSampler(Options{Temperature: 1, TopK: 20, TopP: 0.7}) // keeps 0 and 1: 5/8, 3/8
	counts := make([]int, 3)
	const n = 2000
	for range n {
		counts[s.next(mlx.FromFloat32(l, 1, 3)).ItemInt()]++
		mlx.FreeTemps()
	}
	if counts[2] != 0 {
		t.Fatalf("a dropped token was sampled: %v", counts)
	}
	if f := float64(counts[0]) / n; math.Abs(f-0.625) > 0.05 {
		t.Fatalf("token 0 share %.3f, want ≈ 0.625 (%v)", f, counts)
	}
}
