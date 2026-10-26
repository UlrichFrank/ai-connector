//go:build darwin && arm64

package mlx

import (
	"math"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Array is an MLX array (a lazy node of the computation graph until evaluated).
//
// Memory: every array an op returns is a temporary, registered in the current step's list and released by FreeTemps.
// What must outlive the step (weights, the KV cache, results) is taken out with Keep and released with Free. All MLX
// calls come from one goroutine (the model's engine), so the list needs no lock.
type Array struct{ h uintptr }

// Dtype is MLX's element type.
type Dtype int32

const (
	Bool     Dtype = 0
	Uint32   Dtype = 3
	Int32    Dtype = 7
	Float16  Dtype = 9
	Float32  Dtype = 10
	BFloat16 Dtype = 12
)

var temps []uintptr

func wrap(h uintptr) Array {
	temps = append(temps, h)
	return Array{h}
}

// FreeTemps releases every temporary array created since the last call.
func FreeTemps() {
	for _, h := range temps {
		cArrayFree(h)
	}
	temps = temps[:0]
}

// Keep returns a handle to a that FreeTemps does not release (release it with Free).
func Keep(a Array) Array {
	h := cArrayNew()
	check(cArraySet(&h, a.h))
	return Array{h}
}

// Free releases a kept array.
func Free(a Array) {
	if a.h != 0 {
		cArrayFree(a.h)
	}
}

// Valid: the array exists.
func (a Array) Valid() bool { return a.h != 0 }

// Shape is the array's shape.
func (a Array) Shape() []int {
	n := int(cArrayNdim(a.h))
	if n == 0 {
		return nil
	}
	p := unsafe.Slice(cArrayShape(a.h), n)
	out := make([]int, n)
	for i, v := range p {
		out[i] = int(v)
	}
	return out
}

// Dim is the size of one axis (negative: from the end).
func (a Array) Dim(axis int) int {
	s := a.Shape()
	if axis < 0 {
		axis += len(s)
	}
	return s[axis]
}

// Dtype is the element type.
func (a Array) Dtype() Dtype { return Dtype(cArrayDtype(a.h)) }

// Size is the number of elements.
func (a Array) Size() int { return int(cArraySize(a.h)) }

func i32s(v []int) []int32 {
	out := make([]int32, len(v))
	for i, x := range v {
		out[i] = int32(x)
	}
	return out
}

func ptr(v []int32) *int32 {
	if len(v) == 0 {
		return nil
	}
	return &v[0]
}

// FromInt32 makes an int32 array of the given shape (the data is copied).
func FromInt32(data []int32, shape ...int) Array {
	s := i32s(shape)
	return wrap(cArrayNewData(unsafe.Pointer(&data[0]), ptr(s), int32(len(s)), int32(Int32)))
}

// FromFloat32 makes a float32 array of the given shape (the data is copied).
func FromFloat32(data []float32, shape ...int) Array {
	s := i32s(shape)
	return wrap(cArrayNewData(unsafe.Pointer(&data[0]), ptr(s), int32(len(s)), int32(Float32)))
}

// Int is a scalar int32.
func Int(v int) Array { return wrap(cArrayNewInt(int32(v))) }

// Float is a scalar float32.
func Float(v float32) Array { return wrap(cArrayNewFloat32(v)) }

func vec(arrs []Array) uintptr {
	hs := make([]uintptr, len(arrs))
	for i, a := range arrs {
		hs[i] = a.h
	}
	if len(hs) == 0 {
		return cVecArrayNew()
	}
	return cVecArrayNewData(&hs[0], uintptr(len(hs)))
}

// Eval computes the arrays (and waits).
func Eval(arrs ...Array) {
	v := vec(arrs)
	defer cVecArrayFree(v)
	check(cEval(v))
}

// AsyncEval starts computing the arrays.
func AsyncEval(arrs ...Array) {
	v := vec(arrs)
	defer cVecArrayFree(v)
	check(cAsyncEval(v))
}

// ItemInt is the value of a one-element array (evaluated).
func (a Array) ItemInt() int {
	var v int32
	check(cArrayItemInt32(&v, a.h))
	return int(v)
}

// Float32s copies a float32 array's data (evaluated).
func (a Array) Float32s() []float32 {
	check(cArrayEval(a.h))
	n := a.Size()
	p := cArrayDataF32(a.h)
	if p == nil {
		panic(&Error{Msg: "no float32 data"})
	}
	return append([]float32(nil), unsafe.Slice((*float32)(p), n)...)
}

// Int32s copies an int32 array's data (evaluated).
func (a Array) Int32s() []int32 {
	check(cArrayEval(a.h))
	n := a.Size()
	p := cArrayDataI32(a.h)
	if p == nil {
		panic(&Error{Msg: "no int32 data"})
	}
	return append([]int32(nil), unsafe.Slice((*int32)(p), n)...)
}

func un(f unaryFn, a Array) Array {
	r := cArrayNew()
	check(f(&r, a.h, stream))
	return wrap(r)
}

func bin(f binaryFn, a, b Array) Array {
	r := cArrayNew()
	check(f(&r, a.h, b.h, stream))
	return wrap(r)
}

func Add(a, b Array) Array       { return bin(cAdd, a, b) }
func Sub(a, b Array) Array       { return bin(cSubtract, a, b) }
func Mul(a, b Array) Array       { return bin(cMultiply, a, b) }
func Div(a, b Array) Array       { return bin(cDivide, a, b) }
func Maximum(a, b Array) Array   { return bin(cMaximum, a, b) }
func Matmul(a, b Array) Array    { return bin(cMatmul, a, b) }
func Logaddexp(a, b Array) Array { return bin(cLogaddexp, a, b) }
func FloorDiv(a, b Array) Array  { return bin(cFloorDivide, a, b) }
func Less(a, b Array) Array      { return bin(cLess, a, b) }
func GreaterEqual(a, b Array) Array {
	return bin(cGreaterEqual, a, b)
}
func Sigmoid(a Array) Array  { return un(cSigmoid, a) }
func Exp(a Array) Array      { return un(cExp, a) }
func Log(a Array) Array      { return un(cLog, a) }
func Negative(a Array) Array { return un(cNegative, a) }
func Square(a Array) Array   { return un(cSquare, a) }
func Sqrt(a Array) Array     { return un(cSqrt, a) }
func Rsqrt(a Array) Array    { return un(cRsqrt, a) }
func Copy(a Array) Array     { return un(cCopy, a) }
func Erf(a Array) Array      { return un(cErf, a) }

// SiLU is x·sigmoid(x).
func SiLU(a Array) Array { return Mul(a, Sigmoid(a)) }

// Softplus is log(1 + eˣ).
func Softplus(a Array) Array { return Logaddexp(a, Float(0)) }

// QuantizedMatmul is x·wᵀ with w quantised (affine, group size, bits).
func QuantizedMatmul(x, w, scales, biases Array, transpose bool, groupSize, bits int) Array {
	r := cArrayNew()
	check(cQuantizedMatmul(&r, x.h, w.h, scales.h, biases.h, transpose, optInt(groupSize), optInt(bits), "affine", stream))
	return wrap(r)
}

// GatherQMM multiplies x with the experts' quantised matrices picked by rhs (Mixture of Experts).
func GatherQMM(x, w, scales, biases, lhs, rhs Array, transpose bool, groupSize, bits int, sorted bool) Array {
	r := cArrayNew()
	check(cGatherQMM(&r, x.h, w.h, scales.h, biases.h, lhs.h, rhs.h, transpose, optInt(groupSize), optInt(bits), "affine", sorted, stream))
	return wrap(r)
}

// Dequantize unpacks a quantised matrix (into the scales' type).
func Dequantize(w, scales, biases Array, groupSize, bits int) Array {
	r := cArrayNew()
	check(cDequantize(&r, w.h, scales.h, biases.h, optInt(groupSize), optInt(bits), "affine", 0, 0, stream))
	return wrap(r)
}

func Reshape(a Array, shape ...int) Array {
	s := i32s(shape)
	r := cArrayNew()
	check(cReshape(&r, a.h, ptr(s), uintptr(len(s)), stream))
	return wrap(r)
}

func Transpose(a Array, axes ...int) Array {
	s := i32s(axes)
	r := cArrayNew()
	check(cTransposeAxes(&r, a.h, ptr(s), uintptr(len(s)), stream))
	return wrap(r)
}

func Swapaxes(a Array, ax1, ax2 int) Array {
	r := cArrayNew()
	check(cSwapaxes(&r, a.h, int32(ax1), int32(ax2), stream))
	return wrap(r)
}

func Concatenate(arrs []Array, axis int) Array {
	v := vec(arrs)
	defer cVecArrayFree(v)
	r := cArrayNew()
	check(cConcatenate(&r, v, int32(axis), stream))
	return wrap(r)
}

func Stack(arrs []Array, axis int) Array {
	v := vec(arrs)
	defer cVecArrayFree(v)
	r := cArrayNew()
	check(cStack(&r, v, int32(axis), stream))
	return wrap(r)
}

// Slice takes a[start:stop] on every axis (strides 1).
func Slice(a Array, start, stop []int) Array {
	st, sp := i32s(start), i32s(stop)
	one := make([]int32, len(st))
	for i := range one {
		one[i] = 1
	}
	r := cArrayNew()
	check(cSlice(&r, a.h, ptr(st), uintptr(len(st)), ptr(sp), uintptr(len(sp)), ptr(one), uintptr(len(one)), stream))
	return wrap(r)
}

// SliceAxis takes a[..., from:to, ...] on one axis.
func SliceAxis(a Array, axis, from, to int) Array {
	s := a.Shape()
	if axis < 0 {
		axis += len(s)
	}
	start := make([]int, len(s))
	stop := append([]int(nil), s...)
	start[axis], stop[axis] = from, to
	return Slice(a, start, stop)
}

// SliceUpdate writes upd into src[start:stop].
func SliceUpdate(src, upd Array, start, stop []int) Array {
	st, sp := i32s(start), i32s(stop)
	one := make([]int32, len(st))
	for i := range one {
		one[i] = 1
	}
	r := cArrayNew()
	check(cSliceUpdate(&r, src.h, upd.h, ptr(st), uintptr(len(st)), ptr(sp), uintptr(len(sp)), ptr(one), uintptr(len(one)), stream))
	return wrap(r)
}

func Take(a, idx Array, axis int) Array {
	r := cArrayNew()
	check(cTakeAxis(&r, a.h, idx.h, int32(axis), stream))
	return wrap(r)
}

func TakeAlongAxis(a, idx Array, axis int) Array {
	r := cArrayNew()
	check(cTakeAlongAxis(&r, a.h, idx.h, int32(axis), stream))
	return wrap(r)
}

// PutAlongAxis is a copy of a with vals written at idx along axis (vals broadcast to idx's shape).
func PutAlongAxis(a, idx, vals Array, axis int) Array {
	r := cArrayNew()
	check(cPutAlongAxis(&r, a.h, idx.h, vals.h, int32(axis), stream))
	return wrap(r)
}

// Cumsum is the running sum along axis: reverse sums from the end; not inclusive leaves each element itself out.
func Cumsum(a Array, axis int, reverse, inclusive bool) Array {
	r := cArrayNew()
	check(cCumsum(&r, a.h, int32(axis), reverse, inclusive, stream))
	return wrap(r)
}

func AsType(a Array, t Dtype) Array {
	if a.Dtype() == t {
		return a
	}
	r := cArrayNew()
	check(cAstype(&r, a.h, int32(t), stream))
	return wrap(r)
}

func Sum(a Array, axis int, keep bool) Array {
	r := cArrayNew()
	check(cSumAxis(&r, a.h, int32(axis), keep, stream))
	return wrap(r)
}

func Mean(a Array, axis int, keep bool) Array {
	r := cArrayNew()
	check(cMeanAxis(&r, a.h, int32(axis), keep, stream))
	return wrap(r)
}

func Softmax(a Array, axis int, precise bool) Array {
	r := cArrayNew()
	check(cSoftmaxAxis(&r, a.h, int32(axis), precise, stream))
	return wrap(r)
}

func Argpartition(a Array, kth, axis int) Array {
	r := cArrayNew()
	check(cArgpartition(&r, a.h, int32(kth), int32(axis), stream))
	return wrap(r)
}

func Argsort(a Array, axis int) Array {
	r := cArrayNew()
	check(cArgsort(&r, a.h, int32(axis), stream))
	return wrap(r)
}

func Argmax(a Array, axis int, keep bool) Array {
	r := cArrayNew()
	check(cArgmax(&r, a.h, int32(axis), keep, stream))
	return wrap(r)
}

func ExpandDims(a Array, axis int) Array {
	r := cArrayNew()
	check(cExpandDims(&r, a.h, int32(axis), stream))
	return wrap(r)
}

func Squeeze(a Array, axis int) Array {
	r := cArrayNew()
	check(cSqueeze(&r, a.h, int32(axis), stream))
	return wrap(r)
}

func Where(c, x, y Array) Array {
	r := cArrayNew()
	check(cWhere(&r, c.h, x.h, y.h, stream))
	return wrap(r)
}

func Zeros(t Dtype, shape ...int) Array {
	s := i32s(shape)
	r := cArrayNew()
	check(cZeros(&r, ptr(s), uintptr(len(s)), int32(t), stream))
	return wrap(r)
}

// Conv1d: input [N, L, C_in], weight [C_out, K, C_in/groups].
func Conv1d(in, w Array, stride, pad, dilation, groups int) Array {
	r := cArrayNew()
	check(cConv1d(&r, in.h, w.h, int32(stride), int32(pad), int32(dilation), int32(groups), stream))
	return wrap(r)
}

func Contiguous(a Array) Array {
	r := cArrayNew()
	check(cContiguous(&r, a.h, false, stream))
	return wrap(r)
}

func Repeat(a Array, repeats, axis int) Array {
	r := cArrayNew()
	check(cRepeat(&r, a.h, int32(repeats), int32(axis), stream))
	return wrap(r)
}

// RMSNorm over the last axis (w may be the zero Array: no weight).
func RMSNorm(x, w Array, eps float32) Array {
	r := cArrayNew()
	check(cRMSNorm(&r, x.h, w.h, eps, stream))
	return wrap(r)
}

// LayerNorm over the last axis with weight and bias (either may be the zero Array).
func LayerNorm(x, w, b Array, eps float32) Array {
	r := cArrayNew()
	check(cLayerNorm(&r, x.h, w.h, b.h, eps, stream))
	return wrap(r)
}

// RoPE rotates the first dims features of x ([B, H, L, D]) for the positions offset…offset+L−1.
func RoPE(x Array, dims int, traditional bool, base float32, scale float32, offset int) Array {
	r := cArrayNew()
	b := uint64(math.Float32bits(base)) | 1<<32
	check(cRoPE(&r, x.h, int32(dims), traditional, b, scale, int32(offset), 0, stream))
	return wrap(r)
}

// SDPA is softmax(q·kᵀ·scale)·v with grouped-query heads; causal masks the future (aligned at the end).
func SDPA(q, k, v Array, scale float32, causal bool) Array {
	mode := ""
	if causal {
		mode = "causal"
	}
	r := cArrayNew()
	check(cSDPA(&r, q.h, k.h, v.h, scale, mode, 0, 0, stream))
	return wrap(r)
}

// Categorical samples an index along axis with probabilities softmax(logits).
func Categorical(logits Array, axis int) Array {
	r := cArrayNew()
	check(cCategorical(&r, logits.h, int32(axis), 0, stream))
	return wrap(r)
}

// Seed seeds MLX's random generator.
func Seed(seed uint64) { cRandomSeed(seed) }

// ---------- memory ----------

// SetWiredLimit lets MLX keep up to n bytes resident (wired) on the GPU; it returns the previous limit.
func SetWiredLimit(n int) int { var old uintptr; cSetWiredLimit(&old, uintptr(n)); return int(old) }

// MaxWorkingSet is the GPU's recommended working set (Metal's recommendedMaxWorkingSetSize; 0 if unknown).
func MaxWorkingSet() int {
	dev := cDeviceNewType(1, 0) // MLX_GPU
	defer cDeviceFree(dev)
	info := cDeviceInfoNew()
	defer func() { cDeviceInfoFree(info) }()
	if cDeviceInfoGet(&info, dev) != 0 {
		return 0
	}
	var v uintptr
	if cDeviceInfoSize(&v, info, "max_recommended_working_set_size") != 0 {
		return 0
	}
	return int(v)
}
func SetCacheLimit(n int) { var old uintptr; cSetCacheLimit(&old, uintptr(n)) }
func ClearCache()         { cClearCache() }

func ActiveMemory() int { var n uintptr; cActiveMemory(&n); return int(n) }
func PeakMemory() int   { var n uintptr; cPeakMemory(&n); return int(n) }
func CacheMemory() int  { var n uintptr; cCacheMemory(&n); return int(n) }

// ---------- safetensors ----------

// LoadSafetensors maps a safetensors file (the arrays are loaded lazily, on first use). The arrays are kept (Free them).
func LoadSafetensors(path string) (map[string]Array, error) {
	var out map[string]Array
	err := Guard(func() error {
		m, meta := cMapArrNew(), cMapStrNew()
		defer cMapArrFree(m)
		defer cMapStrFree(meta)
		// loading happens on the CPU stream (it reads the file)
		cpu := cCPUStream()
		defer cStreamFree(cpu)
		check(cLoadSafetensors(&m, &meta, path, cpu))
		it := cMapArrItNew(m)
		defer cMapArrItFree(it)
		out = map[string]Array{}
		for {
			var key *byte
			v := cArrayNew()
			if cMapArrItNext(&key, &v, it) != 0 || key == nil {
				cArrayFree(v)
				break
			}
			out[goString(key)] = Array{v}
		}
		return nil
	})
	return out, err
}

// ---------- compiled functions ----------

// Compiled is a function traced once and fused by MLX (mx.compile): a chain of element-wise ops becomes one kernel.
type Compiled struct{ h uintptr }

// Compile compiles f (shapeless: one trace for every input shape — element-wise functions only). f runs while MLX
// traces it, on the calling thread.
func Compile(f func(in []Array) []Array, shapeless bool) *Compiled {
	cb := purego.NewCallback(func(res *uintptr, input uintptr) int32 {
		ok := false
		func() {
			defer func() { _ = recover() }()
			n := int(cVecArraySize(input))
			in := make([]Array, n)
			for i := range in {
				h := cArrayNew()
				check(cVecArrayGet(&h, input, uintptr(i)))
				in[i] = wrap(h)
			}
			out := f(in)
			hs := make([]uintptr, len(out))
			for i, a := range out {
				hs[i] = a.h
			}
			check(cVecArraySetData(res, &hs[0], uintptr(len(hs))))
			ok = true
		}()
		if !ok {
			return 1
		}
		return 0
	})
	fn := cClosureNewFunc(cb)
	defer cClosureFree(fn)
	c := &Compiled{}
	check(cCompile(&c.h, fn, shapeless))
	return c
}

// Call runs the compiled function.
func (c *Compiled) Call(in ...Array) []Array {
	v := vec(in)
	defer cVecArrayFree(v)
	outs := cVecArrayNew()
	defer cVecArrayFree(outs)
	check(cClosureApply(&outs, c.h, v))
	n := int(cVecArraySize(outs))
	res := make([]Array, n)
	for i := range res {
		r := cArrayNew()
		check(cVecArrayGet(&r, outs, uintptr(i)))
		res[i] = wrap(r)
	}
	return res
}

// ---------- custom Metal kernels ----------

// Kernel is a custom Metal kernel (mx.fast.metal_kernel).
type Kernel struct{ h uintptr }

func cstrings(ss []string) (uintptr, func()) {
	bufs := make([][]byte, len(ss))
	ps := make([]uintptr, len(ss))
	for i, s := range ss {
		bufs[i] = append([]byte(s), 0)
		ps[i] = uintptr(unsafe.Pointer(&bufs[i][0]))
	}
	v := cVecStrNewData(&ps[0], uintptr(len(ps)))
	return v, func() { cVecStrFree(v); _ = bufs }
}

// NewKernel compiles (lazily, on first use) a Metal kernel body with the given input and output names.
func NewKernel(name string, inputs, outputs []string, source string) *Kernel {
	in, freeIn := cstrings(inputs)
	defer freeIn()
	out, freeOut := cstrings(outputs)
	defer freeOut()
	return &Kernel{h: cKernelNew(name, in, out, source, "", true, false)}
}

// TemplateArg is a template parameter of a kernel: a dtype or an int.
type TemplateArg struct {
	Name  string
	Dtype Dtype
	Int   int
	IsInt bool
}

// KernelCall is one launch.
type KernelCall struct {
	Inputs      []Array
	Template    []TemplateArg
	Grid, Group [3]int
	OutShapes   [][]int
	OutDtypes   []Dtype
}

// Apply launches the kernel and returns its outputs.
func (k *Kernel) Apply(c KernelCall) []Array {
	cfg := cKCfgNew()
	defer cKCfgFree(cfg)
	for i, s := range c.OutShapes {
		sh := i32s(s)
		check(cKCfgAddOutput(cfg, ptr(sh), uintptr(len(sh)), int32(c.OutDtypes[i])))
	}
	check(cKCfgSetGrid(cfg, int32(c.Grid[0]), int32(c.Grid[1]), int32(c.Grid[2])))
	check(cKCfgSetGroup(cfg, int32(c.Group[0]), int32(c.Group[1]), int32(c.Group[2])))
	for _, t := range c.Template {
		if t.IsInt {
			check(cKCfgTmplInt(cfg, t.Name, int32(t.Int)))
		} else {
			check(cKCfgTmplDtype(cfg, t.Name, int32(t.Dtype)))
		}
	}
	in := vec(c.Inputs)
	defer cVecArrayFree(in)
	outs := cVecArrayNew()
	defer cVecArrayFree(outs)
	check(cKernelApply(&outs, k.h, in, cfg, stream))
	n := int(cVecArraySize(outs))
	res := make([]Array, n)
	for i := range res {
		r := cArrayNew()
		check(cVecArrayGet(&r, outs, uintptr(i)))
		res[i] = wrap(r)
	}
	return res
}
