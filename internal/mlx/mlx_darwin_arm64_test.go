//go:build darwin && arm64

package mlx

import (
	"math"
	"os"
	"testing"
)

// The embedded runtime unpacks, loads and computes (needs the Apple GPU; MLXLAB_MLX=0 skips).
func TestRuntime(t *testing.T) {
	if os.Getenv("MLXLAB_MLX") == "0" {
		t.Skip("MLXLAB_MLX=0")
	}
	t.Setenv("MLXLAB_LIB_DIR", t.TempDir())
	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	defer FreeTemps()
	a := FromFloat32([]float32{1, 2, 3, 4, 5, 6}, 2, 3)
	b := FromFloat32([]float32{1, 0, 0, 1, 1, 1}, 3, 2)
	c := Matmul(a, b)
	got := c.Float32s()
	want := []float32{4, 5, 10, 11}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("matmul %v, want %v", got, want)
		}
	}
	if s := c.Shape(); len(s) != 2 || s[0] != 2 || s[1] != 2 {
		t.Fatalf("shape %v", s)
	}
	// optional-int/-float arguments: RoPE with a base, quantised matmul with group size and bits
	x := FromFloat32([]float32{1, 0, 0, 0}, 1, 1, 1, 4)
	r := RoPE(x, 4, false, 10000, 1, 1).Float32s()
	// position 1, pairs (0,2) with θ=1 and (1,3) with θ=1/100: x0' = cos(1), x2' = sin(1)
	if math.Abs(float64(r[0])-math.Cos(1)) > 1e-5 || math.Abs(float64(r[2])-math.Sin(1)) > 1e-5 {
		t.Fatalf("rope %v", r)
	}
	// the sampler's ops: an exclusive reverse running sum, a comparison, a scatter
	p := FromFloat32([]float32{0.1, 0.2, 0.3, 0.4}, 1, 4)
	if got := Cumsum(p, -1, true, false).Float32s(); math.Abs(float64(got[0]-0.9)) > 1e-6 || got[3] != 0 {
		t.Fatalf("cumsum %v", got)
	}
	if got := AsType(Less(p, Float(0.25)), Float32).Float32s(); got[0] != 1 || got[1] != 1 || got[2] != 0 {
		t.Fatalf("less %v", got)
	}
	if got := PutAlongAxis(Zeros(Float32, 1, 4), FromInt32([]int32{2}, 1, 1), Float(1), -1).Float32s(); got[2] != 1 || got[0] != 0 {
		t.Fatalf("put along axis %v", got)
	}
	// errors come back as panics that Guard turns into errors
	err := Guard(func() error { Matmul(a, a).Float32s(); return nil })
	if err == nil {
		t.Fatal("a bad matmul did not fail")
	}
	t.Log(err)
}
