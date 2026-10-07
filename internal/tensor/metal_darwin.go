//go:build darwin

package tensor

// The Apple GPU through Metal Performance Shaders — without cgo: the Objective-C runtime and the frameworks are loaded
// at run time (purego, purego/objc), like Accelerate in blas.go. Only the matrix products run on the GPU
// (MPSMatrixMultiplication); several products of one input (q, k, v) share one command buffer.
//
// Memory: Apple Silicon has one memory for CPU and GPU, and every buffer the GPU touches is owned by Metal
// (MTLResourceStorageModeShared) — Go memory is never handed to the GPU:
//   - the weights are read from the file straight into Metal buffers (alloc, used by Weights.F32): the Go slice the
//     model holds is the buffer's contents, so the CPU and the GPU read the same bytes and nothing is copied or kept
//     twice;
//   - the activations live in buffers of a pool (power-of-two sizes): the models take them as Go slices (Scratch,
//     the buffer's contents) and give them back after the layer, so the GPU reads and writes them in place. Any other
//     Go slice is copied into a pool buffer, and a result out of one, around the command buffer. Buffers idle for
//     poolIdle are released, so the memory of a burst of requests is returned.
//
// Two pitfalls of MPS, each one a cause of the wrong (zero, NaN) results of the first version:
//   - No buffer may come near a 2 GiB boundary of the GPU's address space (gpuAddress): MPSMatrixMultiplication
//     computes the rows of a result around such a boundary wrongly (NaN in every other column of a band of some
//     hundred rows, TestBoundary) — also when the buffer ends up to 4 MB below the boundary (its tiles reach past the
//     matrix). It hit long texts (results of 30–130 MB): whether a buffer is near a boundary depends on where Metal
//     places it. newBuffer sets such buffers aside (spanning) and asks again; spanMargin keeps 16 MB of distance.
//   - Encoding is serialised (encode): encodeToCommandBuffer from two threads at once, fresh in a process, left
//     results unwritten (zeros) or wrote NaN and once hung the GPU (TestGPUConcurrent without the lock). Encoding takes
//     microseconds; several goroutines still have command buffers in flight at once (MLXLAB_OVERLAP), each with
//     its own pool buffers, the weights only read.

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

const (
	mpsDataTypeFloat32         = 0x10000000 | 32
	mtlResourceStorageShared   = 0
	mtlResourceHazardUntracked = 1 << 8
	mtlCommandBufferCompleted  = 4

	poolMinBytes = 1 << 16          // the smallest pool buffer
	poolIdle     = 10 * time.Second // pool buffers unused this long are released
	copyGrain    = 1 << 18          // values per core when copying in and out
	weightMin    = 1 << 18          // tensors of fewer values stay in Go memory (norms, biases: never on the GPU)
)

type metalGPU struct {
	name          string
	device, queue objc.ID
	page          uintptr

	encode sync.Mutex // MPS encoding is not thread safe (see above)

	spanMu   sync.Mutex
	spanning []objc.ID // buffers near a 2 GiB boundary of the GPU's addresses, set aside for good (see above)

	regMu   sync.RWMutex
	regions []region // the weights' and the pool's buffers, by start address

	poolMu sync.Mutex
	free   map[uintptr][]pooled // idle pool buffers by size
	trim   *time.Timer

	clsDesc, clsMatrix, clsMul objc.ID
}

// region is a Metal buffer that Go slices point into (weights, Scratch).
type region struct {
	start, end uintptr
	buf        objc.ID
}

// pooled is a pool buffer: its contents and size in bytes.
type pooled struct {
	buf  objc.ID
	ptr  unsafe.Pointer
	size uintptr
	used time.Time
}

func (p pooled) floats(n int) []float32 { return unsafe.Slice((*float32)(p.ptr), n) }

var (
	selAlloc, selRelease, selName, selUTF8, selNewQueue, selNewBuffer, selContents, selCommandBuffer, selCommit, selWait,
	selStatus, selError, selDescription, selDesc, selMatrixInit, selMulInit, selEncode, selHasUnified, selGPUAddress,
	selResponds objc.SEL

	msgID       func(id objc.ID, sel objc.SEL) objc.ID
	msgVoid     func(id objc.ID, sel objc.SEL)
	msgUint     func(id objc.ID, sel objc.SEL) uint
	msgBool     func(id objc.ID, sel objc.SEL) bool
	msgStr      func(id objc.ID, sel objc.SEL) string
	msgPtr      func(id objc.ID, sel objc.SEL) unsafe.Pointer
	msgResponds func(id objc.ID, sel objc.SEL, what objc.SEL) bool
	msgNewBuf   func(dev objc.ID, sel objc.SEL, length uint, options uint) objc.ID
	msgDesc     func(cls objc.ID, sel objc.SEL, rows, cols, rowBytes uint, dataType uint32) objc.ID
	msgMatInit  func(obj objc.ID, sel objc.SEL, buf objc.ID, offset uint, desc objc.ID) objc.ID
	msgMulInit  func(obj objc.ID, sel objc.SEL, dev objc.ID, transL, transR bool, rows, cols, inner uint, alpha, beta float64) objc.ID
	msgEncode   func(k objc.ID, sel objc.SEL, cmd, left, right, result objc.ID)
	poolPush    func() uintptr
	poolPop     func(uintptr)
	createDev   func() objc.ID
	mpsSupports func(dev objc.ID) bool
)

// openMetal finds the GPU (an error: none, no Metal Performance Shaders, or not Apple Silicon's shared memory).
func openMetal() (*metalGPU, error) {
	libobjc, err := purego.Dlopen("/usr/lib/libobjc.A.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, err
	}
	// a command-line program gets no GPU from MTLCreateSystemDefaultDevice unless CoreGraphics is loaded
	_, _ = purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	metal, err := purego.Dlopen("/System/Library/Frameworks/Metal.framework/Metal", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, err
	}
	mps, err := purego.Dlopen("/System/Library/Frameworks/MetalPerformanceShaders.framework/MetalPerformanceShaders", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, err
	}
	send, err := purego.Dlsym(libobjc, "objc_msgSend")
	if err != nil {
		return nil, err
	}
	for _, f := range []any{&msgID, &msgVoid, &msgUint, &msgBool, &msgStr, &msgPtr, &msgResponds, &msgNewBuf, &msgDesc, &msgMatInit, &msgMulInit, &msgEncode} {
		purego.RegisterFunc(f, send)
	}
	purego.RegisterLibFunc(&poolPush, libobjc, "objc_autoreleasePoolPush")
	purego.RegisterLibFunc(&poolPop, libobjc, "objc_autoreleasePoolPop")
	purego.RegisterLibFunc(&createDev, metal, "MTLCreateSystemDefaultDevice")
	purego.RegisterLibFunc(&mpsSupports, mps, "MPSSupportsMTLDevice")
	for p, name := range map[*objc.SEL]string{
		&selAlloc: "alloc", &selRelease: "release", &selName: "name", &selUTF8: "UTF8String",
		&selNewQueue: "newCommandQueue", &selNewBuffer: "newBufferWithLength:options:", &selContents: "contents",
		&selCommandBuffer: "commandBuffer", &selCommit: "commit", &selWait: "waitUntilCompleted", &selStatus: "status",
		&selError: "error", &selDescription: "localizedDescription",
		&selDesc:       "matrixDescriptorWithRows:columns:rowBytes:dataType:",
		&selMatrixInit: "initWithBuffer:offset:descriptor:",
		&selMulInit:    "initWithDevice:transposeLeft:transposeRight:resultRows:resultColumns:interiorColumns:alpha:beta:",
		&selEncode:     "encodeToCommandBuffer:leftMatrix:rightMatrix:resultMatrix:",
		&selHasUnified: "hasUnifiedMemory", &selGPUAddress: "gpuAddress", &selResponds: "respondsToSelector:",
	} {
		*p = objc.RegisterName(name)
	}

	g := &metalGPU{page: uintptr(os.Getpagesize()), free: map[uintptr][]pooled{}}
	pool := poolPush()
	defer poolPop(pool)
	if g.device = createDev(); g.device == 0 {
		return nil, errors.New("no Metal device")
	}
	if !mpsSupports(g.device) {
		return nil, errors.New("Metal Performance Shaders do not support this GPU")
	}
	if !msgBool(g.device, selHasUnified) {
		return nil, errors.New("the GPU has its own memory (only Apple Silicon's shared memory is supported)")
	}
	g.name = msgStr(msgID(g.device, selName), selUTF8)
	g.queue = msgID(g.device, selNewQueue)
	g.clsDesc = objc.ID(objc.GetClass("MPSMatrixDescriptor"))
	g.clsMatrix = objc.ID(objc.GetClass("MPSMatrix"))
	g.clsMul = objc.ID(objc.GetClass("MPSMatrixMultiplication"))
	if g.queue == 0 || g.clsDesc == 0 || g.clsMatrix == 0 || g.clsMul == 0 {
		return nil, errors.New("Metal: no command queue or MPS classes")
	}
	// the buffers' GPU addresses (macOS 13) keep them off the 2 GiB boundaries
	probe := msgNewBuf(g.device, selNewBuffer, uint(g.page), mtlResourceStorageShared)
	if probe == 0 {
		return nil, errors.New("Metal: no buffer")
	}
	defer msgVoid(probe, selRelease)
	if !msgResponds(probe, selResponds, selGPUAddress) {
		return nil, errors.New("Metal: buffers without gpuAddress (macOS 13 or later needed)")
	}
	return g, nil
}

// spanBits, spanMargin: no buffer may come closer than spanMargin to a multiple of 2^spanBits in the GPU's address
// space (see above).
const (
	spanBits   = 31
	spanMargin = 16 << 20
)

// nearBoundary reports whether a buffer of size bytes at GPU address a comes too close to a 2 GiB boundary.
func nearBoundary(a, size uintptr) bool {
	lo, hi := a-min(a, spanMargin), a+size+spanMargin
	return lo>>spanBits != (hi-1)>>spanBits
}

// newBuffer is a Metal-owned buffer of at least size bytes (whole pages; zeroed) and its contents; none (0) when Metal
// has no memory, or no place away from the 2 GiB boundaries.
func (g *metalGPU) newBuffer(size uintptr) (objc.ID, unsafe.Pointer, uintptr) {
	size = (size + g.page - 1) &^ (g.page - 1)
	if size > 1<<(spanBits-1)-2*spanMargin {
		return 0, nil, 0
	}
	var buf objc.ID
	for try := 0; ; try++ {
		if buf = msgNewBuf(g.device, selNewBuffer, uint(size), mtlResourceStorageShared|mtlResourceHazardUntracked); buf == 0 {
			return 0, nil, 0
		}
		if !nearBoundary(uintptr(msgUint(buf, selGPUAddress)), size) {
			break
		}
		// kept, not released (Metal would place the next buffer there again), with a spacer that pushes the next one
		// past the boundary; untouched, they take addresses, not memory
		spacer := msgNewBuf(g.device, selNewBuffer, 2*spanMargin, mtlResourceStorageShared|mtlResourceHazardUntracked)
		g.spanMu.Lock()
		g.spanning = append(g.spanning, buf)
		if spacer != 0 {
			g.spanning = append(g.spanning, spacer)
		}
		g.spanMu.Unlock()
		prof.gpuSpanning.Add(1)
		if try == 8 {
			return 0, nil, 0
		}
	}
	p := msgPtr(buf, selContents)
	if p == nil {
		msgVoid(buf, selRelease)
		return 0, nil, 0
	}
	return buf, p, size
}

// alloc is the memory of a weight tensor of n values: a Metal buffer for the GPU to use as it is (nil: small tensors,
// or Metal failed — then Go memory). Weights live as long as the program.
func (g *metalGPU) alloc(n int) []float32 {
	if n < weightMin {
		return nil
	}
	buf, p, size := g.newBuffer(uintptr(n) * 4)
	if buf == 0 {
		return nil
	}
	g.register(buf, p, size)
	return unsafe.Slice((*float32)(p), n)
}

// register notes a buffer whose contents Go slices point into (find).
func (g *metalGPU) register(buf objc.ID, p unsafe.Pointer, size uintptr) {
	start := uintptr(p)
	g.regMu.Lock()
	defer g.regMu.Unlock()
	i := sort.Search(len(g.regions), func(i int) bool { return g.regions[i].start > start })
	g.regions = append(g.regions, region{})
	copy(g.regions[i+1:], g.regions[i:])
	g.regions[i] = region{start, start + size, buf}
}

func (g *metalGPU) unregister(p unsafe.Pointer) {
	start := uintptr(p)
	g.regMu.Lock()
	defer g.regMu.Unlock()
	i := sort.Search(len(g.regions), func(i int) bool { return g.regions[i].start >= start })
	if i < len(g.regions) && g.regions[i].start == start {
		g.regions = append(g.regions[:i], g.regions[i+1:]...)
	}
}

// find is the buffer (a weight's or the pool's) holding n values at p, and p's offset in it (0: none — Go memory).
func (g *metalGPU) find(p unsafe.Pointer, n int) (objc.ID, uintptr) {
	start := uintptr(p)
	g.regMu.RLock()
	defer g.regMu.RUnlock()
	i := sort.Search(len(g.regions), func(i int) bool { return g.regions[i].start > start }) - 1
	if i < 0 || start+uintptr(n)*4 > g.regions[i].end {
		return 0, 0
	}
	return g.regions[i].buf, start - g.regions[i].start
}

// get takes a pool buffer of at least size bytes.
func (g *metalGPU) get(size uintptr) (pooled, error) {
	size = max(poolMinBytes, uintptr(1)<<bits.Len(uint(size-1)))
	g.poolMu.Lock()
	if l := g.free[size]; len(l) > 0 {
		p := l[len(l)-1]
		g.free[size] = l[:len(l)-1]
		g.poolMu.Unlock()
		return p, nil
	}
	g.poolMu.Unlock()
	buf, ptr, _ := g.newBuffer(size)
	if buf == 0 {
		return pooled{}, fmt.Errorf("Metal: no buffer of %d MB", size>>20)
	}
	g.register(buf, ptr, size)
	prof.gpuAllocs.Add(1)
	return pooled{buf: buf, ptr: ptr, size: size}, nil
}

// put returns pool buffers (their command buffer has completed); the ones idle for poolIdle are released later.
func (g *metalGPU) put(ps []pooled) {
	if len(ps) == 0 {
		return
	}
	now := time.Now()
	g.poolMu.Lock()
	defer g.poolMu.Unlock()
	for _, p := range ps {
		p.used = now
		g.free[p.size] = append(g.free[p.size], p)
	}
	if g.trim == nil {
		g.trim = time.AfterFunc(poolIdle, g.trimIdle)
	}
}

func (g *metalGPU) trimIdle() {
	g.poolMu.Lock()
	defer g.poolMu.Unlock()
	g.trim = nil
	cut := time.Now().Add(-poolIdle)
	left := false
	for size, l := range g.free {
		keep := l[:0]
		for _, p := range l {
			if p.used.Before(cut) {
				g.unregister(p.ptr)
				msgVoid(p.buf, selRelease)
			} else {
				keep = append(keep, p)
			}
		}
		g.free[size] = keep
		left = left || len(keep) > 0
	}
	if left {
		g.trim = time.AfterFunc(poolIdle, g.trimIdle)
	}
}

// take is a pool buffer for n values as a Go slice (Scratch.Alloc).
func (g *metalGPU) take(n int) ([]float32, any) {
	p, err := g.get(uintptr(n) * 4)
	if err != nil {
		return nil, nil
	}
	return p.floats(n), p
}

// give returns what take handed out.
func (g *metalGPU) give(held []any) {
	ps := make([]pooled, 0, len(held))
	for _, h := range held {
		ps = append(ps, h.(pooled))
	}
	g.put(ps)
}

// pooledBytes is the size of the idle pool buffers (tests).
func (g *metalGPU) pooledBytes() (n uintptr) {
	g.poolMu.Lock()
	defer g.poolMu.Unlock()
	for _, l := range g.free {
		for _, p := range l {
			n += p.size
		}
	}
	return n
}

// encodeUnlocked drops the encode lock (a test: TestGPUConcurrent shows what goes wrong without it).
var encodeUnlocked bool

// poison fills pool buffers with NaN (a test: nothing may depend on what a buffer held before).
var poison bool

// gemms runs the products in one command buffer and waits for them.
func (g *metalGPU) gemms(ops []gemmOp) error {
	runtime.LockOSThread() // the autorelease pool belongs to the thread
	defer runtime.UnlockOSThread()
	pool := poolPush()
	defer poolPop(pool)

	var release []objc.ID
	var staged []pooled
	defer func() {
		for _, id := range release {
			msgVoid(id, selRelease)
		}
		g.put(staged) // after the wait (or before the commit): the GPU is done with them
	}()

	// the operands: a weight's own buffer, else a pool buffer with a copy (inputs), shared by the products of one input
	start := time.Now()
	type operand struct {
		buf objc.ID
		off uintptr
		p   pooled // the pool buffer (buf == p.buf) or none
	}
	type span struct {
		p unsafe.Pointer
		n int
	}
	inputs := map[span]operand{}
	input := func(s []float32, n int) (operand, error) {
		key := span{unsafe.Pointer(&s[0]), n}
		if o, ok := inputs[key]; ok {
			return o, nil
		}
		o := operand{}
		if o.buf, o.off = g.find(key.p, n); o.buf == 0 {
			p, err := g.get(uintptr(n) * 4)
			if err != nil {
				return o, err
			}
			staged = append(staged, p)
			if poison {
				fillNaN(p.floats(int(p.size / 4)))
			}
			parCopy(p.floats(n), s[:n])
			o = operand{buf: p.buf, p: p}
		}
		inputs[key] = o
		return o, nil
	}
	outs := make([]operand, len(ops))
	for i, op := range ops {
		n := (op.m-1)*op.ldc + op.n
		if buf, off := g.find(unsafe.Pointer(&op.c[0]), n); buf != 0 { // Scratch memory: written in place
			outs[i] = operand{buf: buf, off: off}
			continue
		}
		p, err := g.get(uintptr(n) * 4)
		if err != nil {
			return err
		}
		staged = append(staged, p)
		if poison {
			fillNaN(p.floats(int(p.size / 4)))
		}
		if op.beta != 0 {
			parCopy(p.floats(n), op.c[:n])
		}
		outs[i] = operand{buf: p.buf, p: p}
	}
	type bound struct{ a, b operand }
	bind := make([]bound, len(ops))
	for i, op := range ops {
		bRows, bCols := op.k, op.n
		if op.transB {
			bRows, bCols = op.n, op.k
		}
		a, err := input(op.a, (op.m-1)*op.lda+op.k)
		if err != nil {
			return err
		}
		b, err := input(op.b, (bRows-1)*op.ldb+bCols)
		if err != nil {
			return err
		}
		bind[i] = bound{a, b}
	}
	since(&prof.gpuCopy, start)

	start = time.Now()
	lock, unlock := g.encode.Lock, g.encode.Unlock
	if encodeUnlocked {
		lock, unlock = func() {}, func() {}
	}
	lock()
	cmd := msgID(g.queue, selCommandBuffer)
	if cmd == 0 {
		unlock()
		return errors.New("Metal: no command buffer")
	}
	matrix := func(o operand, rows, cols, ld int) objc.ID {
		desc := msgDesc(g.clsDesc, selDesc, uint(rows), uint(cols), uint(ld*4), mpsDataTypeFloat32)
		m := msgMatInit(msgID(g.clsMatrix, selAlloc), selMatrixInit, o.buf, uint(o.off), desc)
		release = append(release, m)
		return m
	}
	for i, op := range ops {
		bRows, bCols := op.k, op.n
		if op.transB {
			bRows, bCols = op.n, op.k
		}
		k := msgMulInit(msgID(g.clsMul, selAlloc), selMulInit, g.device, false, op.transB, uint(op.m), uint(op.n), uint(op.k),
			float64(op.alpha), float64(op.beta))
		release = append(release, k)
		msgEncode(k, selEncode, cmd, matrix(bind[i].a, op.m, op.k, op.lda), matrix(bind[i].b, bRows, bCols, op.ldb),
			matrix(outs[i], op.m, op.n, op.ldc))
	}
	msgVoid(cmd, selCommit)
	unlock()
	since(&prof.gpuEncode, start)

	start = time.Now()
	prof.gpuCalls.Add(1)
	msgVoid(cmd, selWait)
	since(&prof.gpuWait, start)
	if st := msgUint(cmd, selStatus); st != mtlCommandBufferCompleted {
		msg := "unknown error"
		if e := msgID(cmd, selError); e != 0 {
			msg = msgStr(msgID(e, selDescription), selUTF8)
		}
		return fmt.Errorf("Metal: command buffer status %d: %s", st, msg)
	}

	start = time.Now()
	for i, op := range ops {
		if outs[i].p.buf == 0 {
			continue
		}
		src := outs[i].p.floats((op.m-1)*op.ldc + op.n)
		if op.ldc == op.n {
			parCopy(op.c[:len(src)], src)
			continue
		}
		// only the result's rows: what lies between them in c is not ours
		Parallel(op.m, max(1, copyGrain/op.n), func(lo, hi int) {
			for r := lo; r < hi; r++ {
				copy(op.c[r*op.ldc:r*op.ldc+op.n], src[r*op.ldc:r*op.ldc+op.n])
			}
		})
	}
	since(&prof.gpuCopy, start)
	return nil
}

// parCopy copies src to dst on several cores when large.
func parCopy(dst, src []float32) {
	Parallel(len(src), copyGrain, func(lo, hi int) { copy(dst[lo:hi], src[lo:hi]) })
}

func fillNaN(x []float32) {
	nan := math.Float32frombits(0x7fc00000)
	for i := range x {
		x[i] = nan
	}
}

func (g *metalGPU) label() string { return "gpu · Metal (" + g.name + ")" }

func openGPU() (gpuBackend, error) { return openMetal() }
