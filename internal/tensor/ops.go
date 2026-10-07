package tensor

import (
	"math"
	"runtime"
	"sync"
	"time"
)

// Parallel calls fn on parts [lo, hi) of 0..n on every core — for n below grain in one call.
func Parallel(n, grain int, fn func(lo, hi int)) {
	workers := min(runtime.GOMAXPROCS(0), n/max(1, grain))
	if workers <= 1 {
		fn(0, n)
		return
	}
	var wg sync.WaitGroup
	step := (n + workers - 1) / workers
	for lo := 0; lo < n; lo += step {
		wg.Go(func() { fn(lo, min(n, lo+step)) })
	}
	wg.Wait()
}

// rowGrain: rows per core below which the norms stay on one core.
const rowGrain = 16

// LayerNorm normalises every row of x (rows×d) in place: (x − mean) / sqrt(var + eps) · w (+ b; b may be nil).
func LayerNorm(x []float32, d int, w, b []float32, eps float32) {
	Parallel(len(x)/d, rowGrain*1024/d, func(lo, hi int) { layerNorm(x[lo*d:hi*d], d, w, b, eps) })
}

func layerNorm(x []float32, d int, w, b []float32, eps float32) {
	for r := 0; r+d <= len(x); r += d {
		row := x[r : r+d]
		var mean float64
		for _, v := range row {
			mean += float64(v)
		}
		mean /= float64(d)
		var vs float64
		for _, v := range row {
			dv := float64(v) - mean
			vs += dv * dv
		}
		inv := float32(1 / math.Sqrt(vs/float64(d)+float64(eps)))
		m := float32(mean)
		for j, v := range row {
			y := (v - m) * inv * w[j]
			if b != nil {
				y += b[j]
			}
			row[j] = y
		}
	}
}

// RMSNorm normalises every row of x (rows×d) in place: x / sqrt(mean(x²) + eps) · w.
func RMSNorm(x []float32, d int, w []float32, eps float32) {
	Parallel(len(x)/d, rowGrain*1024/d, func(lo, hi int) { rmsNorm(x[lo*d:hi*d], d, w, eps) })
}

func rmsNorm(x []float32, d int, w []float32, eps float32) {
	for r := 0; r+d <= len(x); r += d {
		row := x[r : r+d]
		var ss float64
		for _, v := range row {
			ss += float64(v) * float64(v)
		}
		inv := float32(1 / math.Sqrt(ss/float64(d)+float64(eps)))
		for j, v := range row {
			row[j] = v * inv * w[j]
		}
	}
}

// GELU is the exact (erf) GELU of PyTorch's nn.GELU() and transformers' "gelu".
func GELU(x float32) float32 {
	return 0.5 * x * (1 + Erf(x*(1/math.Sqrt2)))
}

// Erf is erf in float32 precision: a rational approximation on [-4, 4] (Eigen's, as PyTorch's CPU kernels use; about
// 1e-7 from math.Erf) — several times faster than math.Erf, and GELU's erf is most of the non-matrix time of Laya.
func Erf(x float32) float32 {
	x = min(4, max(-4, x))
	x2 := x * x
	p := x2*-2.72614225801306e-10 + 2.77068142495902e-08
	p = x2*p + -2.10102402082508e-06
	p = x2*p + -5.69250639462346e-05
	p = x2*p + -7.34990630326855e-04
	p = x2*p + -2.95459980854025e-03
	p = x2*p + -1.60960333262415e-02
	p *= x
	q := x2*-1.45660718464996e-05 + -2.13374055278905e-04
	q = x2*q + -1.68282697438203e-03
	q = x2*q + -7.37332916720468e-03
	q = x2*q + -1.42647390514189e-02
	return p / q
}

// SiLU is x·sigmoid(x).
func SiLU(x float32) float32 {
	return x / (1 + Exp(-x))
}

// Exp is e^x in float32 precision (within 10⁻⁷ of math.Exp, relative; Cephes' expf: x = n·ln 2 + r, a polynomial for e^r) —
// about twice as fast as math.Exp, and the exponentials of softmax and SiLU are much of the non-matrix time.
// Results below the smallest normal float32 are 0.
func Exp(x float32) float32 {
	switch {
	case x != x:
		return x
	case x > 88.72283:
		return float32(math.Inf(1))
	case x < -87.33654:
		return 0
	}
	const log2e, c1, c2 = 1.44269504088896341, 0.693359375, -2.12194440e-4
	t := x * log2e
	if t < 0 {
		t -= 0.5
	} else {
		t += 0.5
	}
	n := int32(t) // round to nearest
	fn := float32(n)
	r := x - fn*c1 - fn*c2
	p := float32(1.9875691500e-4)
	p = p*r + 1.3981999507e-3
	p = p*r + 8.3334519073e-3
	p = p*r + 4.1665795894e-2
	p = p*r + 1.6666665459e-1
	p = p*r + 5.0000001201e-1
	y := p*r*r + r + 1
	if n > 126 { // 2^n in two steps: 2^128 is no float32
		return y * math.Float32frombits(uint32(n-1+127)<<23) * 2
	}
	return y * math.Float32frombits(uint32(n+127)<<23)
}

// Add adds y to x in place.
func Add(x, y []float32) {
	y = y[:len(x)]
	Parallel(len(x), 1<<16, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			x[i] += y[i]
		}
	})
}

// Softmax replaces x by its softmax (in place).
func Softmax(x []float32) {
	if len(x) == 0 {
		return
	}
	mx := x[0]
	for _, v := range x {
		mx = max(mx, v)
	}
	var s float64
	for i, v := range x {
		e := Exp(v - mx)
		x[i] = e
		s += float64(e)
	}
	inv := float32(1 / s)
	for i := range x {
		x[i] *= inv
	}
}

// Rope holds the rotary position embedding tables of one base (theta) and head size: cos and sin per position and
// frequency, "rotate half" layout (the first half of a head pairs with the second half).
type Rope struct {
	dim      int
	cos, sin []float32 // positions × dim/2
}

// NewRope computes the tables for positions 0..n-1.
func NewRope(dim int, theta float64, n int) *Rope {
	half := dim / 2
	r := &Rope{dim: dim, cos: make([]float32, n*half), sin: make([]float32, n*half)}
	for i := 0; i < half; i++ {
		// as transformers: inv_freq in float32, the angle position·inv_freq in float32
		inv := float32(1 / math.Pow(theta, float64(2*i)/float64(dim)))
		for p := 0; p < n; p++ {
			a := float32(p) * inv
			r.cos[p*half+i] = float32(math.Cos(float64(a)))
			r.sin[p*half+i] = float32(math.Sin(float64(a)))
		}
	}
	return r
}

// Grow makes the tables cover n positions.
func (r *Rope) Grow(theta float64, n int) *Rope {
	if len(r.cos)/(r.dim/2) >= n {
		return r
	}
	return NewRope(r.dim, theta, n)
}

// Apply rotates one head vector (dim values) at position pos in place.
func (r *Rope) Apply(v []float32, pos int) {
	half := r.dim / 2
	c, s := r.cos[pos*half:(pos+1)*half], r.sin[pos*half:(pos+1)*half]
	for i := 0; i < half; i++ {
		x1, x2 := v[i], v[i+half]
		v[i] = x1*c[i] - x2*s[i]
		v[i+half] = x2*c[i] + x1*s[i]
	}
}

// Attention computes softmax(Q·Kᵀ·scale + mask)·V for one sequence of n tokens and all heads. q, k, v are n×(heads·dim)
// with row strides qs, ks, vs (so they may be views into a fused projection); kvHeads < heads shares a key/value head
// between heads/kvHeads query heads (grouped-query attention). allow(i, j) says whether token i sees token j (nil:
// all). The result is n×(heads·dim).
func Attention(q []float32, qs int, k []float32, ks int, v []float32, vs int, n, heads, kvHeads, dim int, allow func(i, j int) bool) []float32 {
	defer since(&prof.attention, time.Now())
	out := make([]float32, n*heads*dim)
	scale := float32(1 / math.Sqrt(float64(dim)))
	group := heads / kvHeads
	work := func(h int, qh, kh, vh, sc []float32) {
		kv := h / group
		for i := 0; i < n; i++ {
			copy(qh[i*dim:(i+1)*dim], q[i*qs+h*dim:i*qs+(h+1)*dim])
			copy(kh[i*dim:(i+1)*dim], k[i*ks+kv*dim:i*ks+(kv+1)*dim])
			copy(vh[i*dim:(i+1)*dim], v[i*vs+kv*dim:i*vs+(kv+1)*dim])
		}
		gemmSmall(true, n, n, dim, scale, qh, dim, kh, dim, 0, sc, n)
		for i := 0; i < n; i++ {
			row := sc[i*n : (i+1)*n]
			if allow != nil {
				for j := range row {
					if !allow(i, j) {
						row[j] = float32(math.Inf(-1))
					}
				}
			}
			Softmax(row)
		}
		oh := qh // reuse: the query copy is no longer needed
		gemmSmall(false, n, dim, n, 1, sc, n, vh, dim, 0, oh, dim)
		for i := 0; i < n; i++ {
			copy(out[i*heads*dim+h*dim:i*heads*dim+(h+1)*dim], oh[i*dim:(i+1)*dim])
		}
	}
	workers := min(heads, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	next := make(chan int, heads)
	for h := 0; h < heads; h++ {
		next <- h
	}
	close(next)
	for w := 0; w < workers; w++ {
		wg.Go(func() {
			qh, kh, vh := make([]float32, n*dim), make([]float32, n*dim), make([]float32, n*dim)
			sc := make([]float32, n*n)
			for h := range next {
				work(h, qh, kh, vh, sc)
			}
		})
	}
	wg.Wait()
	return out
}

// Normalize scales v to length 1 (in place; a zero vector stays zero).
func Normalize(v []float32) {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	n := math.Sqrt(s)
	if n < 1e-12 {
		n = 1e-12
	}
	inv := float32(1 / n)
	for i := range v {
		v[i] *= inv
	}
}
