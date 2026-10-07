// Package tensor holds the few numeric kernels the two models need: a matrix product (a system BLAS when there is
// one, else plain Go), normalisations, activations and rotary position embeddings — all on float32 row-major data.
//
// The matrix product is where the time goes (over 95 % of a forward pass). macOS always has Accelerate (it uses the
// Apple matrix units: about as fast as PyTorch on the CPU); on Linux an installed OpenBLAS is used. Both are loaded at
// run time without cgo (purego), so the binary stays static and cross-compiles; without either the Go kernel below
// runs on every core — correct, but several times slower.
package tensor

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Gemm computes C = alpha·A·op(B) + beta·C on row-major matrices: A is M×K (row stride lda), op(B) is K×N — B itself
// is N×K (stride ldb) when transB, else K×N — and C is M×N (stride ldc).
func Gemm(transB bool, m, n, k int, alpha float32, a []float32, lda int, b []float32, ldb int, beta float32, c []float32, ldc int) {
	if m == 0 || n == 0 {
		return
	}
	if blas != nil && m*n*k >= 4096 {
		blas.gemm(transB, m, n, k, alpha, a, lda, b, ldb, beta, c, ldc)
		return
	}
	goGemm(transB, m, n, k, alpha, a, lda, b, ldb, beta, c, ldc)
}

// Linear computes y = x·Wᵀ (+ bias) for rows x (rows×in) and a PyTorch weight W (out×in): y is rows×out.
func Linear(x []float32, rows, in int, w []float32, bias []float32, out int) []float32 {
	return LinearMany(x, rows, in, Proj{w, bias, out})[0]
}

// Scratch hands out memory for the activations of a forward pass that the GPU reads and writes as it is: with a GPU
// its pool buffers (no copies into and out of them around every product), else Go memory. Free returns them; the
// slices must not be used after that. Not for concurrent use.
type Scratch struct {
	held []any
}

// Alloc is room for n values — not zeroed (with a GPU it held earlier activations): for results and copies.
func (s *Scratch) Alloc(n int) []float32 {
	if g := curGPU(); g != nil && n >= scratchMin {
		if v, h := g.take(n); v != nil {
			s.held = append(s.held, h)
			return v
		}
	}
	return make([]float32, n)
}

// Free returns the memory to the pool.
func (s *Scratch) Free() {
	if len(s.held) == 0 {
		return
	}
	if b := gpuDev.Load(); b != nil {
		b.g.give(s.held)
	}
	s.held = nil
}

// scratchMin: smaller activations stay in Go memory (their products stay on the CPU anyway).
const scratchMin = 1 << 14

// Linear is Linear with the result in the scratch memory.
func (s *Scratch) Linear(x []float32, rows, in int, w []float32, bias []float32, out int) []float32 {
	return s.LinearMany(x, rows, in, Proj{w, bias, out})[0]
}

// LinearMany is LinearMany with the results in the scratch memory.
func (s *Scratch) LinearMany(x []float32, rows, in int, ps ...Proj) [][]float32 {
	return linearMany(s, x, rows, in, ps...)
}

// Proj is one linear layer: a PyTorch weight (Out×in) and its bias (nil: none).
type Proj struct {
	W, Bias []float32
	Out     int
}

// LinearMany applies several linear layers to the same rows x (rows×in) — q, k and v, or gate and up: on the GPU in
// one command buffer.
func LinearMany(x []float32, rows, in int, ps ...Proj) [][]float32 {
	return linearMany(nil, x, rows, in, ps...)
}

func linearMany(s *Scratch, x []float32, rows, in int, ps ...Proj) [][]float32 {
	ys := make([][]float32, len(ps))
	ops := make([]gemmOp, len(ps))
	for i, p := range ps {
		if s != nil {
			ys[i] = s.Alloc(rows * p.Out)
		} else {
			ys[i] = make([]float32, rows*p.Out)
		}
		ops[i] = gemmOp{transB: true, m: rows, n: p.Out, k: in, alpha: 1, a: x, lda: in, b: p.W, ldb: in, c: ys[i], ldc: p.Out}
	}
	runGemms(ops)
	for i, p := range ps {
		if p.Bias == nil {
			continue
		}
		y := ys[i]
		for r := 0; r < rows; r++ {
			row := y[r*p.Out : (r+1)*p.Out]
			for j := range row {
				row[j] += p.Bias[j]
			}
		}
	}
	return ys
}

// gemmOp is one Gemm call.
type gemmOp struct {
	transB      bool
	m, n, k     int
	alpha, beta float32
	a           []float32
	lda         int
	b           []float32
	ldb         int
	c           []float32
	ldc         int
}

// gpuBackend runs matrix products on a GPU (metal_darwin.go).
type gpuBackend interface {
	gemms(ops []gemmOp) error
	// alloc is memory for a weight tensor of n values that the GPU uses without a copy (nil: Go memory will do)
	alloc(n int) []float32
	// take is scratch memory for n values that the GPU uses without a copy (nil: Go memory will do) and its handle
	// for give, which returns it
	take(n int) ([]float32, any)
	give(held []any)
	label() string
}

var (
	// gpuMin is the smallest product (m·n·k) worth sending to the GPU (MLXLAB_GPU_MIN): below it Accelerate is as
	// fast, a command buffer's round trip included.
	gpuMin = 1 << 27
	gpuMu  sync.Mutex
	gpuCur atomic.Pointer[gpuBox]
	// gpuDev is the GPU once opened (also while switched off): the weights loaded after that live in its memory.
	gpuDev atomic.Pointer[gpuBox]
)

type gpuBox struct{ g gpuBackend }

// curGPU is the GPU in use (nil: none).
func curGPU() gpuBackend {
	if b := gpuCur.Load(); b != nil {
		return b.g
	}
	return nil
}

func setGPU(g gpuBackend) {
	if g == nil {
		gpuCur.Store(nil)
		return
	}
	gpuCur.Store(&gpuBox{g})
}

// newWeights is the memory of a weight tensor: the GPU's (shared with the CPU) once a GPU has been opened, else Go's.
func newWeights(n int) []float32 {
	if b := gpuDev.Load(); b != nil {
		if w := b.g.alloc(n); w != nil {
			return w
		}
	}
	return make([]float32, n)
}

// overlap: forward passes run at once with a GPU (MLXLAB_OVERLAP, default 2): while one waits for its matrix
// products on the GPU, the next computes attention and norms on the CPU.
var overlap = 2

// Batches calls fn for the parts [lo, hi) of 0..n, size at most batch: one after the other on the CPU; with a GPU
// `overlap` at a time.
func Batches(n, batch int, fn func(lo, hi int)) {
	workers := 1
	if curGPU() != nil {
		workers = overlap
	}
	if workers <= 1 || n <= batch {
		for lo := 0; lo < n; lo += batch {
			fn(lo, min(n, lo+batch))
		}
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for lo := 0; lo < n; lo += batch {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			fn(lo, min(n, lo+batch))
		})
	}
	wg.Wait()
}

var opened struct {
	sync.Once
	g   gpuBackend
	err error
}

// UseGPU switches the GPU on (for products of at least minFlops = m·n·k; 0 keeps the threshold) or off.
func UseGPU(on bool, minFlops int) error {
	if !on {
		setGPU(nil)
		return nil
	}
	opened.Do(func() {
		defer func() {
			if r := recover(); r != nil { // a framework call that went wrong: no GPU
				opened.g, opened.err = nil, fmt.Errorf("GPU: %v", r)
			}
		}()
		opened.g, opened.err = openGPU()
		if opened.err == nil {
			gpuDev.Store(&gpuBox{opened.g})
		}
	})
	if opened.err != nil {
		return opened.err
	}
	setGPU(opened.g)
	if minFlops > 0 {
		gpuMin = minFlops
	}
	return nil
}

// runGemms computes the products: the large ones on the GPU (if any; together), the rest on the CPU. A GPU that fails
// is dropped for good, and its products are computed on the CPU.
func runGemms(ops []gemmOp) {
	defer since(&prof.linear, time.Now())
	var big []gemmOp
	g := curGPU()
	for _, op := range ops {
		if op.m == 0 || op.n == 0 {
			continue
		}
		if g != nil && op.m*op.n*op.k >= gpuMin {
			big = append(big, op)
			continue
		}
		Gemm(op.transB, op.m, op.n, op.k, op.alpha, op.a, op.lda, op.b, op.ldb, op.beta, op.c, op.ldc)
	}
	if len(big) == 0 {
		return
	}
	err := g.gemms(big)
	if err == nil {
		return
	}
	gpuMu.Lock()
	if curGPU() != nil {
		fmt.Fprintf(os.Stderr, "mlxlab: %v — matrix products on the CPU from now on\n", err)
		setGPU(nil)
	}
	gpuMu.Unlock()
	for _, op := range big {
		Gemm(op.transB, op.m, op.n, op.k, op.alpha, op.a, op.lda, op.b, op.ldb, op.beta, op.c, op.ldc)
	}
}

// Backend names the matrix product in use ("accelerate", "openblas: <lib>", "go"; "… + gpu · Metal (<GPU>)").
func Backend() string {
	name := "go"
	if blas != nil {
		name = blas.name
	}
	if g := curGPU(); g != nil {
		name += " + " + g.label()
	}
	return name
}

// Device is what vboard shows as this machine's device (the bridge's hello; the server keeps 20 characters):
// "gpu · Metal", "cpu · Accelerate", "cpu · OpenBLAS", "cpu".
func Device() string {
	switch {
	case curGPU() != nil:
		return "gpu · Metal"
	case blas == nil:
		return "cpu"
	case blas.name == "accelerate":
		return "cpu · Accelerate"
	case strings.HasPrefix(blas.name, "openblas"):
		return "cpu · OpenBLAS"
	}
	return "cpu · BLAS"
}

// gemmSmall is the product for the many small matrices of attention, computed in parallel per head: the system BLAS
// where it may be called concurrently, else the Go kernel.
func gemmSmall(transB bool, m, n, k int, alpha float32, a []float32, lda int, b []float32, ldb int, beta float32, c []float32, ldc int) {
	if blas != nil && blas.concurrent {
		Gemm(transB, m, n, k, alpha, a, lda, b, ldb, beta, c, ldc)
		return
	}
	goGemm(transB, m, n, k, alpha, a, lda, b, ldb, beta, c, ldc)
}

// UseGo switches to the Go kernel (MLXLAB_BLAS=go; the tests compare both).
func UseGo() { blas = nil; setGPU(nil) }

type blasLib struct {
	name string
	// concurrent: fine to call from several goroutines at once (Accelerate). OpenBLAS runs its own thread pool and,
	// called from many goroutines, oversubscribes the cores (measured: ten times slower than the Go kernel).
	concurrent bool
	gemm       func(transB bool, m, n, k int, alpha float32, a []float32, lda int, b []float32, ldb int, beta float32, c []float32, ldc int)
}

// blas is the system library (nil: the Go kernel), found at start (blas.go).
var blas *blasLib

// ---------- the Go kernel ----------

// goGemm packs op(B) as N×K rows (so every output is a dot product of two contiguous rows) and computes 4×4 tiles of C
// in parallel over row blocks.
func goGemm(transB bool, m, n, k int, alpha float32, a []float32, lda int, b []float32, ldb int, beta float32, c []float32, ldc int) {
	bt := b
	ldbt := ldb
	if !transB {
		bt = make([]float32, n*k)
		for i := 0; i < k; i++ {
			row := b[i*ldb : i*ldb+n]
			for j, v := range row {
				bt[j*k+i] = v
			}
		}
		ldbt = k
	}
	workers := runtime.GOMAXPROCS(0)
	if flops := m * n * k; flops < 1<<18 {
		workers = 1
	}
	// split the longer side into blocks of 4 (the tile), about four blocks per worker
	type block struct{ i0, i1, j0, j1 int }
	var blocks []block
	if m >= n {
		step := max(4, (m/(workers*4)+3)/4*4)
		for i := 0; i < m; i += step {
			blocks = append(blocks, block{i, min(m, i+step), 0, n})
		}
	} else {
		step := max(4, (n/(workers*4)+3)/4*4)
		for j := 0; j < n; j += step {
			blocks = append(blocks, block{0, m, j, min(n, j+step)})
		}
	}
	if workers == 1 || len(blocks) == 1 {
		for _, bl := range blocks {
			gemmBlock(bl.i0, bl.i1, bl.j0, bl.j1, k, alpha, a, lda, bt, ldbt, beta, c, ldc)
		}
		return
	}
	var wg sync.WaitGroup
	ch := make(chan block)
	for w := 0; w < workers; w++ {
		wg.Go(func() {
			for bl := range ch {
				gemmBlock(bl.i0, bl.i1, bl.j0, bl.j1, k, alpha, a, lda, bt, ldbt, beta, c, ldc)
			}
		})
	}
	for _, bl := range blocks {
		ch <- bl
	}
	close(ch)
	wg.Wait()
}

func gemmBlock(i0, i1, j0, j1, k int, alpha float32, a []float32, lda int, b []float32, ldb int, beta float32, c []float32, ldc int) {
	store := func(i, j int, v float32) {
		p := &c[i*ldc+j]
		if beta == 0 {
			*p = alpha * v
		} else {
			*p = alpha*v + beta**p
		}
	}
	i := i0
	for ; i+4 <= i1; i += 4 {
		a0, a1, a2, a3 := a[i*lda:i*lda+k], a[(i+1)*lda:(i+1)*lda+k], a[(i+2)*lda:(i+2)*lda+k], a[(i+3)*lda:(i+3)*lda+k]
		j := j0
		for ; j+4 <= j1; j += 4 {
			b0, b1, b2, b3 := b[j*ldb:j*ldb+k], b[(j+1)*ldb:(j+1)*ldb+k], b[(j+2)*ldb:(j+2)*ldb+k], b[(j+3)*ldb:(j+3)*ldb+k]
			var s00, s01, s02, s03, s10, s11, s12, s13, s20, s21, s22, s23, s30, s31, s32, s33 float32
			for p := 0; p < k; p++ {
				x0, x1, x2, x3 := a0[p], a1[p], a2[p], a3[p]
				y0, y1, y2, y3 := b0[p], b1[p], b2[p], b3[p]
				s00 += x0 * y0
				s01 += x0 * y1
				s02 += x0 * y2
				s03 += x0 * y3
				s10 += x1 * y0
				s11 += x1 * y1
				s12 += x1 * y2
				s13 += x1 * y3
				s20 += x2 * y0
				s21 += x2 * y1
				s22 += x2 * y2
				s23 += x2 * y3
				s30 += x3 * y0
				s31 += x3 * y1
				s32 += x3 * y2
				s33 += x3 * y3
			}
			store(i, j, s00)
			store(i, j+1, s01)
			store(i, j+2, s02)
			store(i, j+3, s03)
			store(i+1, j, s10)
			store(i+1, j+1, s11)
			store(i+1, j+2, s12)
			store(i+1, j+3, s13)
			store(i+2, j, s20)
			store(i+2, j+1, s21)
			store(i+2, j+2, s22)
			store(i+2, j+3, s23)
			store(i+3, j, s30)
			store(i+3, j+1, s31)
			store(i+3, j+2, s32)
			store(i+3, j+3, s33)
		}
		for ; j < j1; j++ {
			bj := b[j*ldb : j*ldb+k]
			store(i, j, dot(a0, bj))
			store(i+1, j, dot(a1, bj))
			store(i+2, j, dot(a2, bj))
			store(i+3, j, dot(a3, bj))
		}
	}
	for ; i < i1; i++ {
		ai := a[i*lda : i*lda+k]
		for j := j0; j < j1; j++ {
			store(i, j, dot(ai, b[j*ldb:j*ldb+k]))
		}
	}
}

func dot(x, y []float32) float32 {
	var s0, s1, s2, s3 float32
	n := len(x)
	y = y[:n]
	p := 0
	for ; p+4 <= n; p += 4 {
		s0 += x[p] * y[p]
		s1 += x[p+1] * y[p+1]
		s2 += x[p+2] * y[p+2]
		s3 += x[p+3] * y[p+3]
	}
	for ; p < n; p++ {
		s0 += x[p] * y[p]
	}
	return s0 + s1 + s2 + s3
}
