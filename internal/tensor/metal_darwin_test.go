//go:build darwin

package tensor

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sync"
	"testing"
	"unsafe"

	"github.com/ebitengine/purego/objc"
)

// testGPU opens the GPU for a test (skips where there is none).
func testGPU(t testing.TB) *metalGPU {
	if err := UseGPU(true, 0); err != nil {
		t.Skipf("no GPU: %v", err)
	}
	return opened.g.(*metalGPU)
}

// withGPU sends every product through runGemms to the GPU (threshold 1) for the test.
func withGPU(t testing.TB) *metalGPU {
	g := testGPU(t)
	saved, savedMin := curGPU(), gpuMin
	setGPU(g)
	gpuMin = 1
	t.Cleanup(func() { setGPU(saved); gpuMin = savedMin })
	return g
}

// weightsOf copies w into GPU memory (as a model's weights are loaded).
func weightsOf(g *metalGPU, w []float32) []float32 {
	out := g.alloc(len(w))
	if out == nil {
		out = make([]float32, len(w))
	}
	copy(out, w)
	return out
}

// The GPU's products against the definition: both B layouts, strides, alpha and beta, views into larger slices, B in
// GPU memory (a weight) or in Go memory, and several products in one command buffer. The pool buffers are filled with
// NaN first: nothing may depend on what they held.
func TestGemmGPU(t *testing.T) {
	g := testGPU(t)
	poison = true
	defer func() { poison = false }()
	r := rand.New(rand.NewPCG(5, 6))
	for _, sz := range [][3]int{{1, 1, 1}, {3, 5, 7}, {17, 9, 33}, {64, 70, 130}, {5, 300, 64}, {130, 4, 256}, {300, 1024, 1024},
		{257, 3072, 1024}, {100, 1024, 4096}} {
		m, n, k := sz[0], sz[1], sz[2]
		for _, transB := range []bool{true, false} {
			for _, weight := range []bool{false, true} {
				for _, beta := range []float32{0, 0.25} {
					lda, ldb, ldc := k+3, n+2, n+1
					if transB {
						ldb = k + 2
					}
					a := randVec(r, m*lda+5)[5:] // an offset view
					b := randVec(r, max(n, k)*ldb)
					if weight {
						b = weightsOf(g, b)
					}
					c0 := randVec(r, m*ldc)
					want := append([]float32(nil), c0...)
					if m*n*k < 1<<22 {
						naive(transB, m, n, k, 0.5, a, lda, b, ldb, beta, want, ldc)
					} else { // the model sizes: against the CPU product (checked against the definition in TestGemm)
						Gemm(transB, m, n, k, 0.5, a, lda, b, ldb, beta, want, ldc)
					}
					// the output inside a larger slice: the values around it (and between its rows) must stay
					const pad = 8192
					buf := randVec(r, pad+m*ldc+pad)
					guard := append([]float32(nil), buf...)
					got := buf[pad : pad+m*ldc]
					copy(got, c0)
					op := gemmOp{transB: transB, m: m, n: n, k: k, alpha: 0.5, a: a, lda: lda, b: b, ldb: ldb, beta: beta, c: got, ldc: ldc}
					// twice in one command buffer on separate outputs, the second into a copy
					got2 := append([]float32(nil), c0...)
					op2 := op
					op2.c = got2
					if err := g.gemms([]gemmOp{op, op2}); err != nil {
						t.Fatal(err)
					}
					what := fmt.Sprintf("%dx%dx%d transB=%v weight=%v beta=%v", m, n, k, transB, weight, beta)
					close32(t, what, got, want, 1e-3)
					for i := range pad {
						if buf[i] != guard[i] || buf[len(buf)-1-i] != guard[len(buf)-1-i] {
							t.Fatalf("%s: wrote outside the result (%d from its start or end)", what, i)
						}
					}
					for i := 0; i < m; i++ {
						if i < m-1 && got[i*ldc+n] != c0[i*ldc+n] {
							t.Fatalf("%s: wrote between the rows (row %d)", what, i)
						}
					}
					close32(t, what+" (2nd)", got2, want, 1e-3)
				}
			}
		}
	}
	t.Log(g.label())
}

// Linear and LinearMany through the GPU (a threshold of 1: everything goes there) against the CPU.
func TestLinearGPU(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	rows, in := 37, 256
	x := randVec(r, rows*in)
	w1, w2, b2 := randVec(r, 300*in), randVec(r, 64*in), randVec(r, 64)
	g := withGPU(t)
	setGPU(nil)
	want := LinearMany(x, rows, in, Proj{w1, nil, 300}, Proj{w2, b2, 64})
	setGPU(g)
	for _, weight := range []bool{false, true} {
		v1, v2 := w1, w2
		if weight {
			v1, v2 = weightsOf(g, w1), weightsOf(g, w2)
		}
		got := LinearMany(x, rows, in, Proj{v1, nil, 300}, Proj{v2, b2, 64})
		close32(t, "w1", got[0], want[0], 1e-3)
		close32(t, "w2", got[1], want[1], 1e-3)
		close32(t, "linear", Linear(x, rows, in, v1, nil, 300), want[0], 1e-3)
	}
}

// Products on Scratch memory (read and written in place) against the CPU; after Free the memory is reused.
func TestScratchGPU(t *testing.T) {
	r := rand.New(rand.NewPCG(13, 14))
	rows, in := 300, 1024
	x0 := randVec(r, rows*in)
	w1, w2, b2 := randVec(r, 2048*in), randVec(r, 1024*in), randVec(r, 1024)
	g := withGPU(t)
	setGPU(nil)
	want := LinearMany(x0, rows, in, Proj{w1, nil, 2048}, Proj{w2, b2, 1024})
	setGPU(g)
	w1, w2 = weightsOf(g, w1), weightsOf(g, w2)
	for range 3 {
		var s Scratch
		x := s.Alloc(len(x0))
		copy(x, x0)
		if buf, _ := g.find(unsafeData(x), len(x)); buf == 0 {
			t.Fatal("Scratch memory is not the GPU's")
		}
		got := s.LinearMany(x, rows, in, Proj{w1, nil, 2048}, Proj{w2, b2, 1024})
		close32(t, "w1", got[0], want[0], 1e-3)
		close32(t, "w2", got[1], want[1], 1e-3)
		s.Free()
	}
}

// Weights loaded after the GPU is opened live in its memory, and the GPU finds them there.
func TestWeightsInGPUMemory(t *testing.T) {
	g := testGPU(t)
	w := newWeights(1 << 20)
	if buf, off := g.find(unsafeData(w), len(w)); buf == 0 || off != 0 {
		t.Fatalf("a weight tensor is not in GPU memory")
	}
	if buf, off := g.find(unsafeData(w[1000:]), len(w)-1000); buf == 0 || off != 4000 {
		t.Fatalf("a view into a weight: buffer %v offset %d", buf, off)
	}
	if buf, _ := g.find(unsafeData(w[1000:]), len(w)); buf != 0 {
		t.Fatalf("a range beyond the weight's buffer was found")
	}
	if buf, _ := g.find(unsafeData(make([]float32, 10)), 10); buf != 0 {
		t.Fatalf("Go memory was taken for GPU memory")
	}
	if small := newWeights(100); small == nil || len(small) != 100 {
		t.Fatalf("small tensor")
	}
}

// Several goroutines running products on the GPU at once (as MLXLAB_OVERLAP does) get the CPU's results. This
// reproduced the NaN embeddings of the first Metal version: without the encode lock (MLXLAB_GPU_RACE=1 here), in a
// fresh process, whole results stayed zero or came back NaN (and the GPU hung once), with and without copying the
// activations into Metal's own buffers — concurrent MPS encoding, not the memory, was the cause.
func TestGPUConcurrent(t *testing.T) {
	g := withGPU(t)
	race := os.Getenv("MLXLAB_GPU_RACE") != ""
	encodeUnlocked = race
	defer func() { encodeUnlocked = false }()
	r := rand.New(rand.NewPCG(11, 12))
	in := 1024
	ws := []Proj{{randVec(r, 2048*in), nil, 2048}, {randVec(r, 1024*in), nil, 1024}, {randVec(r, 1024*in), nil, 1024}}
	type job struct {
		x    []float32
		rows int
		want [][]float32
	}
	var jobs []job
	setGPU(nil)
	for _, rows := range []int{37, 128, 300, 513, 64, 200, 1000, 17} {
		x := randVec(r, rows*in)
		jobs = append(jobs, job{x, rows, LinearMany(x, rows, in, ws...)})
	}
	setGPU(g)
	for i := range ws {
		ws[i].W = weightsOf(g, ws[i].W)
	}
	iters := 100
	if testing.Short() {
		iters = 20
	}
	var mu sync.Mutex
	bad := map[string]int{}
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for it := range iters {
				j := jobs[(w+it)%len(jobs)]
				x := append([]float32(nil), j.x...)
				got := LinearMany(x, j.rows, in, ws...)
				local := map[string]int{}
				for p := range got {
					for i := range got[p] {
						if d := math.Abs(float64(got[p][i] - j.want[p][i])); !(d <= 1e-2*(1+math.Abs(float64(j.want[p][i])))) {
							kind := "wrong"
							if got[p][i] != got[p][i] {
								kind = "NaN"
							} else if got[p][i] == 0 {
								kind = "zero"
							}
							local[fmt.Sprintf("rows=%d proj=%d %s", j.rows, p, kind)]++
						}
					}
				}
				mu.Lock()
				for k, v := range local {
					bad[k] += v
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if curGPU() == nil {
		t.Fatal("the GPU failed and was dropped")
	}
	switch {
	case race:
		t.Logf("without the encode lock: %v", bad)
	case len(bad) > 0:
		t.Errorf("concurrent GPU products differ from the CPU: %v", bad)
	}
	if g.pooledBytes() == 0 {
		t.Error("no pool buffers returned")
	}
}

// Pool buffers idle for poolIdle are released.
func TestPoolTrim(t *testing.T) {
	g := testGPU(t)
	p, err := g.get(3 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if p.size != 4<<20 {
		t.Fatalf("bucket %d, want 4 MB", p.size)
	}
	g.put([]pooled{p})
	q, _ := g.get(4 << 20)
	if q.buf != p.buf {
		t.Fatal("the pool did not reuse the buffer")
	}
	g.put([]pooled{q})
	g.poolMu.Lock()
	for _, l := range g.free { // everything idle long enough
		for i := range l {
			l[i].used = l[i].used.Add(-2 * poolIdle)
		}
	}
	g.poolMu.Unlock()
	g.trimIdle()
	if n := g.pooledBytes(); n != 0 {
		t.Fatalf("%d bytes of idle buffers left", n)
	}
}

// go test -run x -bench GemmGPU ./internal/tensor: the linear layers of the two models, CPU (Accelerate) against GPU
// (with the copies into and out of the pool), and the cost of an empty round trip.
func BenchmarkGemmGPU(b *testing.B) {
	g := testGPU(b)
	r := rand.New(rand.NewPCG(9, 10))
	shapes := []struct {
		name string
		out  int
		in   int
	}{{"qwen-q", 2048, 1024}, {"qwen-kv", 1024, 1024}, {"qwen-up", 3072, 1024}, {"qwen-down", 1024, 3072},
		{"laya-qkv", 3072, 1024}, {"laya-wi", 5248, 1024}, {"laya-wo2", 1024, 2624}}
	for _, rows := range []int{16, 64, 256, 1024, 4096} {
		for _, s := range shapes {
			x, w := randVec(r, rows*s.in), weightsOf(g, randVec(r, s.out*s.in))
			y := make([]float32, rows*s.out)
			op := gemmOp{transB: true, m: rows, n: s.out, k: s.in, alpha: 1, a: x, lda: s.in, b: w, ldb: s.in, c: y, ldc: s.out}
			flops := 2 * float64(rows*s.out*s.in)
			b.Run(fmt.Sprintf("%s/rows=%d/cpu", s.name, rows), func(b *testing.B) {
				for b.Loop() {
					Gemm(true, rows, s.out, s.in, 1, x, s.in, w, s.in, 0, y, s.out)
				}
				b.ReportMetric(flops*float64(b.N)/b.Elapsed().Seconds()/1e9, "GFLOP/s")
			})
			b.Run(fmt.Sprintf("%s/rows=%d/gpu", s.name, rows), func(b *testing.B) {
				ResetProfile()
				for b.Loop() {
					if err := g.gemms([]gemmOp{op}); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(flops*float64(b.N)/b.Elapsed().Seconds()/1e9, "GFLOP/s")
				b.ReportMetric(float64(ReadProfile().GPUCopy.Microseconds())/float64(b.N), "copy-µs/op")
			})
		}
	}
	// the fixed cost of one command buffer: a 1×1×1 product
	tiny := gemmOp{transB: true, m: 1, n: 1, k: 1, alpha: 1, a: []float32{1}, lda: 1, b: []float32{2}, ldb: 1, c: make([]float32, 1), ldc: 1}
	b.Run("roundtrip", func(b *testing.B) {
		for b.Loop() {
			if err := g.gemms([]gemmOp{tiny}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func unsafeData(s []float32) unsafe.Pointer { return unsafe.Pointer(&s[0]) }

// The 2 GiB boundaries of the GPU's addresses: newBuffer never hands out a buffer near one, and products written
// into its buffers are right. A product into a buffer across a boundary (made directly) shows MPS's fault: logged,
// not asserted (a later macOS may fix it).
func TestBoundary(t *testing.T) {
	if nearBoundary(0x1570000000, 0x8000000) || !nearBoundary(0x1578000000, 0x7ff8000) || !nearBoundary(0x157ff00000, 1<<20) ||
		!nearBoundary(0x1580000000, 1<<20) || nearBoundary(0x1581000000, 1<<20) {
		t.Fatal("nearBoundary")
	}
	g := testGPU(t)
	r := rand.New(rand.NewPCG(9, 9))
	m, n, k := 10000, 3072, 1024
	a := randVec(r, m*k)
	b := weightsOf(g, randVec(r, n*k))
	want := make([]float32, m*n)
	Gemm(true, m, n, k, 1, a, k, b, k, 0, want, n)
	product := func(buf objc.ID, p unsafe.Pointer, size uintptr) int {
		g.register(buf, p, size)
		defer g.unregister(p)
		c := unsafe.Slice((*float32)(p), m*n)
		op := gemmOp{transB: true, m: m, n: n, k: k, alpha: 1, a: a, lda: k, b: b, ldb: k, c: c, ldc: n}
		if err := g.gemms([]gemmOp{op}); err != nil {
			t.Fatal(err)
		}
		bad := 0
		for i := range want {
			if d := want[i] - c[i]; !(d < 1e-2 && d > -1e-2) {
				bad++
			}
		}
		return bad
	}
	const size = 128 << 20
	var bufs []objc.ID
	defer func() {
		for _, b := range bufs {
			msgVoid(b, selRelease)
		}
	}()
	spans := func(buf objc.ID) bool { return nearBoundary(uintptr(msgUint(buf, selGPUAddress)), size) }
	shown, check := false, true
	for range 40 { // 5 GB of addresses (not of memory: the buffers stay untouched but for two)
		raw := msgNewBuf(g.device, selNewBuffer, size, mtlResourceStorageShared|mtlResourceHazardUntracked)
		bufs = append(bufs, raw)
		if !shown && spans(raw) {
			shown, check = true, true
			t.Logf("a product into a buffer near a 2 GiB boundary (GPU %#x): %d of %d values wrong",
				msgUint(raw, selGPUAddress), product(raw, msgPtr(raw, selContents), size), m*n)
		}
		buf, p, _ := g.newBuffer(size)
		if buf == 0 {
			t.Fatal("no buffer")
		}
		bufs = append(bufs, buf)
		if spans(buf) {
			t.Fatalf("newBuffer handed out a buffer near a 2 GiB boundary: %#x", msgUint(buf, selGPUAddress))
		}
		if check { // the first one, and the one right after a buffer across a boundary
			check = false
			if bad := product(buf, p, size); bad > 0 {
				t.Fatalf("%d values wrong", bad)
			}
		}
	}
}
