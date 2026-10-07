package tensor

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

func randVec(r *rand.Rand, n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

// naive is the definition: C = alpha·A·op(B) + beta·C.
func naive(transB bool, m, n, k int, alpha float32, a []float32, lda int, b []float32, ldb int, beta float32, c []float32, ldc int) {
	for i := 0; i < m; i++ {
		for j := 0; j < n; j++ {
			var s float64
			for p := 0; p < k; p++ {
				bv := b[p*ldb+j]
				if transB {
					bv = b[j*ldb+p]
				}
				s += float64(a[i*lda+p]) * float64(bv)
			}
			c[i*ldc+j] = alpha*float32(s) + beta*c[i*ldc+j]
		}
	}
}

func close32(t *testing.T, what string, got, want []float32, tol float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: length %d, want %d", what, len(got), len(want))
	}
	for i := range want {
		if d := math.Abs(float64(got[i] - want[i])); d > tol*(1+math.Abs(float64(want[i]))) {
			t.Fatalf("%s[%d] = %v, want %v", what, i, got[i], want[i])
		}
	}
}

// The Go kernel and the system BLAS (when there is one) against the definition, on odd sizes, strides, alpha and beta.
func TestGemm(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, sz := range [][3]int{{1, 1, 1}, {3, 5, 7}, {17, 9, 33}, {64, 70, 130}, {5, 300, 64}, {130, 4, 256}} {
		m, n, k := sz[0], sz[1], sz[2]
		for _, transB := range []bool{true, false} {
			lda, ldb, ldc := k+3, n+2, n+1
			if transB {
				ldb = k + 2
			}
			a := randVec(r, m*lda)
			b := randVec(r, max(n, k)*ldb)
			c0 := randVec(r, m*ldc)
			want := append([]float32(nil), c0...)
			naive(transB, m, n, k, 0.5, a, lda, b, ldb, 0.25, want, ldc)

			got := append([]float32(nil), c0...)
			goGemm(transB, m, n, k, 0.5, a, lda, b, ldb, 0.25, got, ldc)
			close32(t, "go", got, want, 1e-4)

			got = append([]float32(nil), c0...)
			Gemm(transB, m, n, k, 0.5, a, lda, b, ldb, 0.25, got, ldc)
			close32(t, Backend(), got, want, 1e-4)
		}
	}
	t.Logf("backend: %s", Backend())
}

func TestLinear(t *testing.T) {
	x := []float32{1, 2, 3, 4, 5, 6} // 2×3
	w := []float32{1, 0, 0, 0, 1, 1} // 2×3: y0 = x0, y1 = x1 + x2
	got := Linear(x, 2, 3, w, []float32{10, 20}, 2)
	close32(t, "linear", got, []float32{11, 25, 14, 31}, 1e-6)
}

func TestNorms(t *testing.T) {
	x := []float32{1, 2, 3, 4, -1, 0, 1, 0}
	LayerNorm(x, 4, []float32{1, 1, 1, 1}, []float32{0, 0, 0, 1}, 0)
	// row 1: mean 2.5, var 1.25
	s := float32(1 / math.Sqrt(1.25))
	close32(t, "layernorm", x[:4], []float32{-1.5 * s, -0.5 * s, 0.5 * s, 1.5*s + 1}, 1e-5)
	y := []float32{3, 4}
	RMSNorm(y, 2, []float32{1, 2}, 0)
	r := float32(math.Sqrt(12.5))
	close32(t, "rmsnorm", y, []float32{3 / r, 8 / r}, 1e-6)
	v := []float32{3, 4}
	Normalize(v)
	close32(t, "normalize", v, []float32{0.6, 0.8}, 1e-6)
}

func TestErf(t *testing.T) {
	worst := 0.0
	for x := -6.0; x <= 6; x += 1e-4 {
		worst = math.Max(worst, math.Abs(float64(Erf(float32(x)))-math.Erf(x)))
	}
	if worst > 1e-6 {
		t.Fatalf("Erf: max error %g", worst)
	}
	t.Logf("Erf: max error %g", worst)
}

func TestActivations(t *testing.T) {
	// reference values of torch.nn.functional.gelu / silu
	for _, c := range []struct{ x, gelu, silu float32 }{{0, 0, 0}, {1, 0.8413447, 0.7310586}, {-2, -0.0455003, -0.2384058}, {3, 2.9959502, 2.8577223}} {
		if d := math.Abs(float64(GELU(c.x) - c.gelu)); d > 1e-6 {
			t.Errorf("GELU(%v) = %v, want %v", c.x, GELU(c.x), c.gelu)
		}
		if d := math.Abs(float64(SiLU(c.x) - c.silu)); d > 1e-6 {
			t.Errorf("SiLU(%v) = %v, want %v", c.x, SiLU(c.x), c.silu)
		}
	}
	p := []float32{1, 2, 3}
	Softmax(p)
	close32(t, "softmax", p, []float32{0.09003057, 0.24472847, 0.66524096}, 1e-6)
}

// RoPE rotates pairs (i, i+half) by position·theta^(−2i/dim): at position 0 nothing changes, the length stays.
func TestRope(t *testing.T) {
	r := NewRope(4, 10000, 8)
	v := []float32{1, 2, 3, 4}
	r.Apply(v, 0)
	close32(t, "pos 0", v, []float32{1, 2, 3, 4}, 0)
	v = []float32{1, 0, 0, 0}
	r.Apply(v, 1) // frequency 0: angle 1 rad; pairs (0,2) and (1,3)
	close32(t, "pos 1", v, []float32{float32(math.Cos(1)), 0, float32(math.Sin(1)), 0}, 1e-6)
	if r.Grow(10000, 4) != r || len(r.Grow(10000, 20).cos) != 20*2 {
		t.Fatal("grow")
	}
}

// Attention against the definition, with a causal mask and grouped key/value heads.
func TestAttention(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	n, heads, kvHeads, dim := 7, 4, 2, 8
	q, k, v := randVec(rng, n*heads*dim), randVec(rng, n*kvHeads*dim), randVec(rng, n*kvHeads*dim)
	causal := func(i, j int) bool { return j <= i }
	got := Attention(q, heads*dim, k, kvHeads*dim, v, kvHeads*dim, n, heads, kvHeads, dim, causal)
	for h := 0; h < heads; h++ {
		kv := h / (heads / kvHeads)
		for i := 0; i < n; i++ {
			var w []float64
			var sum float64
			for j := 0; j <= i; j++ {
				var s float64
				for d := 0; d < dim; d++ {
					s += float64(q[i*heads*dim+h*dim+d]) * float64(k[j*kvHeads*dim+kv*dim+d])
				}
				e := math.Exp(s / math.Sqrt(float64(dim)))
				w = append(w, e)
				sum += e
			}
			for d := 0; d < dim; d++ {
				var o float64
				for j, wj := range w {
					o += wj / sum * float64(v[j*kvHeads*dim+kv*dim+d])
				}
				if g := got[i*heads*dim+h*dim+d]; math.Abs(float64(g)-o) > 1e-4 {
					t.Fatalf("head %d token %d dim %d: %v, want %v", h, i, d, g, o)
				}
			}
		}
	}
}

func TestHalfFloats(t *testing.T) {
	for _, c := range []struct {
		bits uint16
		want float32
	}{{0x3c00, 1}, {0xc000, -2}, {0x3555, 0.33325195}, {0x0001, 5.9604645e-08}, {0x0000, 0}, {0x7bff, 65504}} {
		if got := f16ToF32(c.bits); got != c.want {
			t.Errorf("f16 %#x = %v, want %v", c.bits, got, c.want)
		}
	}
	h := Half{BF16: true, Data: []byte{0x80, 0x3f, 0x40, 0xc0}} // 1.0, -3.0
	if h.At(0) != 1 || h.At(1) != -3 || h.Len() != 2 {
		t.Fatalf("bf16: %v %v", h.At(0), h.At(1))
	}
	if got := f16ToF32(0x7c00); !math.IsInf(float64(got), 1) {
		t.Errorf("f16 inf = %v", got)
	}
}

// A small .safetensors file: the header, dtypes, shapes, and which tensors were read.
func TestSafetensors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.safetensors")
	var data []byte
	put16 := func(v uint16) { data = binary.LittleEndian.AppendUint16(data, v) }
	put32 := func(v float32) { data = binary.LittleEndian.AppendUint32(data, math.Float32bits(v)) }
	put16(0x3c00) // a: F16 [2] = 1, -2
	put16(0xc000)
	put16(0x3f80) // b: BF16 [1] = 1
	put32(2.5)    // c: F32 [2,1]
	put32(-1)
	head, _ := json.Marshal(map[string]any{
		"__metadata__": map[string]string{"format": "pt"},
		"a":            map[string]any{"dtype": "F16", "shape": []int{2}, "data_offsets": []int{0, 4}},
		"b":            map[string]any{"dtype": "BF16", "shape": []int{1}, "data_offsets": []int{4, 6}},
		"c":            map[string]any{"dtype": "F32", "shape": []int{2, 1}, "data_offsets": []int{6, 14}},
	})
	file := binary.LittleEndian.AppendUint64(nil, uint64(len(head)))
	file = append(append(file, head...), data...)
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := OpenSafetensors(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	a, err := w.F32("a", 2)
	if err != nil || a[0] != 1 || a[1] != -2 {
		t.Fatalf("a: %v %v", a, err)
	}
	if _, err := w.F32("a", 3); err == nil {
		t.Fatal("a wrong shape accepted")
	}
	b, err := w.Half("b")
	if err != nil || b.At(0) != 1 {
		t.Fatalf("b: %v", err)
	}
	c, err := w.Half("c", 2, 1)
	if err != nil || c.At(0) != 2.5 || c.At(1) != -1 {
		t.Fatalf("c: %v", err)
	}
	if _, err := w.F32("missing"); err == nil {
		t.Fatal("missing tensor")
	}
	if u := w.Unused(); len(u) != 0 {
		t.Fatalf("unused %v", u)
	}
	if !w.Has("a") || w.Has("z") {
		t.Fatal("Has")
	}
}

// Exp against math.Exp: relative error within 3·10⁻⁷ over float32's range, 0 below it, Inf above, NaN stays NaN.
func TestExp(t *testing.T) {
	worst := 0.0
	for x := float32(-87.3); x < 88.7; x += 0.000731 {
		want := math.Exp(float64(x))
		got := float64(Exp(x))
		if e := math.Abs(got-want) / want; e > worst {
			worst = e
		}
	}
	if worst > 3e-7 {
		t.Errorf("max relative error %g", worst)
	}
	if Exp(-100) != 0 || Exp(float32(math.Inf(-1))) != 0 || !math.IsInf(float64(Exp(100)), 1) || Exp(0) != 1 {
		t.Error("limits")
	}
	if v := Exp(float32(math.NaN())); v == v {
		t.Error("NaN")
	}
	t.Logf("max relative error %g", worst)
}

func BenchmarkExp(b *testing.B) {
	x := make([]float32, 4096)
	for i := range x {
		x[i] = float32(i)/256 - 12
	}
	b.Run("float32", func(b *testing.B) {
		for b.Loop() {
			for i, v := range x {
				x[i] = Exp(v)*0 + v
			}
		}
	})
	b.Run("math", func(b *testing.B) {
		for b.Loop() {
			for i, v := range x {
				x[i] = float32(math.Exp(float64(v)))*0 + v
			}
		}
	})
}
