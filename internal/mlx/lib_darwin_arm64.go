//go:build darwin && arm64

package mlx

import (
	"fmt"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The mlx-c functions, bound with purego. MLX's handle types (mlx_array, mlx_stream, mlx_vector_array …) are structs
// holding one pointer: on arm64 they travel like a pointer, so they are uintptr here. mlx_optional_int/float ({value,
// has_value}, 8 bytes) travel in one register (or 8 bytes on the stack): packed into a uint64.

type (
	unaryFn  func(res *uintptr, a, s uintptr) int32
	binaryFn func(res *uintptr, a, b, s uintptr) int32
)

var (
	cArrayNew        func() uintptr
	cArrayFree       func(a uintptr) int32
	cArraySet        func(dst *uintptr, src uintptr) int32
	cArrayNewData    func(data unsafe.Pointer, shape *int32, dim int32, dtype int32) uintptr
	cArrayNewInt     func(v int32) uintptr
	cArrayNewFloat32 func(v float32) uintptr
	cArrayNdim       func(a uintptr) uintptr
	cArrayShape      func(a uintptr) *int32
	cArrayDtype      func(a uintptr) int32
	cArraySize       func(a uintptr) uintptr
	cArrayEval       func(a uintptr) int32
	cArrayItemInt32  func(res *int32, a uintptr) int32
	cArrayItemF32    func(res *float32, a uintptr) int32
	cArrayDataF32    func(a uintptr) unsafe.Pointer
	cArrayDataI32    func(a uintptr) unsafe.Pointer
	cArrayDataU32    func(a uintptr) unsafe.Pointer

	cGPUStream  func() uintptr
	cCPUStream  func() uintptr
	cStreamFree func(s uintptr) int32

	cVecArrayNewData func(data *uintptr, n uintptr) uintptr
	cVecArrayFree    func(v uintptr) int32
	cVecArraySize    func(v uintptr) uintptr
	cVecArrayGet     func(res *uintptr, v uintptr, i uintptr) int32
	cVecArrayNew     func() uintptr
	cVecStrNewData   func(data *uintptr, n uintptr) uintptr
	cVecStrFree      func(v uintptr) int32

	cEval      func(v uintptr) int32
	cAsyncEval func(v uintptr) int32

	cLoadSafetensors func(m *uintptr, meta *uintptr, file string, s uintptr) int32
	cMapArrNew       func() uintptr
	cMapArrFree      func(m uintptr) int32
	cMapArrItNew     func(m uintptr) uintptr
	cMapArrItFree    func(it uintptr) int32
	cMapArrItNext    func(key **byte, val *uintptr, it uintptr) int32
	cMapStrNew       func() uintptr
	cMapStrFree      func(m uintptr) int32

	cSetErrorHandler func(h, data, dtor uintptr)
	cMetalAvailable  func(res *bool) int32
	cSetWiredLimit   func(res *uintptr, limit uintptr) int32
	cSetCacheLimit   func(res *uintptr, limit uintptr) int32
	cSetMemoryLimit  func(res *uintptr, limit uintptr) int32
	cActiveMemory    func(res *uintptr) int32
	cPeakMemory      func(res *uintptr) int32
	cCacheMemory     func(res *uintptr) int32
	cClearCache      func() int32

	cAdd, cSubtract, cMultiply, cDivide, cMaximum, cMatmul, cLogaddexp, cFloorDivide binaryFn
	cLess, cGreaterEqual                                                             binaryFn
	cCumsum                                                                          func(res *uintptr, a uintptr, axis int32, reverse, inclusive bool, s uintptr) int32
	cPutAlongAxis                                                                    func(res *uintptr, a, idx, vals uintptr, axis int32, s uintptr) int32
	cCategorical                                                                     func(res *uintptr, logits uintptr, axis int32, key, s uintptr) int32
	cRandomSeed                                                                      func(seed uint64) int32
	cSigmoid, cExp, cLog, cNegative, cSquare, cSqrt, cRsqrt, cCopy, cErf             unaryFn
	cQuantizedMatmul                                                                 func(res *uintptr, x, w, scales, biases uintptr, transpose bool, gs, bits uint64, mode string, s uintptr) int32
	cGatherQMM                                                                       func(res *uintptr, x, w, scales, biases, lhs, rhs uintptr, transpose bool, gs, bits uint64, mode string, sorted bool, s uintptr) int32
	cDequantize                                                                      func(res *uintptr, w, scales, biases uintptr, gs, bits uint64, mode string, globalScale uintptr, dtype uint64, s uintptr) int32
	cReshape                                                                         func(res *uintptr, a uintptr, shape *int32, n uintptr, s uintptr) int32
	cTransposeAxes                                                                   func(res *uintptr, a uintptr, axes *int32, n uintptr, s uintptr) int32
	cSwapaxes                                                                        func(res *uintptr, a uintptr, ax1, ax2 int32, s uintptr) int32
	cConcatenate                                                                     func(res *uintptr, v uintptr, axis int32, s uintptr) int32
	cStack                                                                           func(res *uintptr, v uintptr, axis int32, s uintptr) int32
	cSlice                                                                           func(res *uintptr, a uintptr, start *int32, ns uintptr, stop *int32, ne uintptr, strides *int32, nst uintptr, s uintptr) int32
	cSliceUpdate                                                                     func(res *uintptr, src, upd uintptr, start *int32, ns uintptr, stop *int32, ne uintptr, strides *int32, nst uintptr, s uintptr) int32
	cTakeAxis                                                                        func(res *uintptr, a, idx uintptr, axis int32, s uintptr) int32
	cTakeAlongAxis                                                                   func(res *uintptr, a, idx uintptr, axis int32, s uintptr) int32
	cAstype                                                                          func(res *uintptr, a uintptr, dtype int32, s uintptr) int32
	cSumAxis                                                                         func(res *uintptr, a uintptr, axis int32, keep bool, s uintptr) int32
	cMeanAxis                                                                        func(res *uintptr, a uintptr, axis int32, keep bool, s uintptr) int32
	cSoftmaxAxis                                                                     func(res *uintptr, a uintptr, axis int32, precise bool, s uintptr) int32
	cArgpartition                                                                    func(res *uintptr, a uintptr, kth, axis int32, s uintptr) int32
	cArgsort                                                                         func(res *uintptr, a uintptr, axis int32, s uintptr) int32
	cArgmax                                                                          func(res *uintptr, a uintptr, axis int32, keep bool, s uintptr) int32
	cExpandDims                                                                      func(res *uintptr, a uintptr, axis int32, s uintptr) int32
	cSqueeze                                                                         func(res *uintptr, a uintptr, axis int32, s uintptr) int32
	cWhere                                                                           func(res *uintptr, c, x, y, s uintptr) int32
	cZeros                                                                           func(res *uintptr, shape *int32, n uintptr, dtype int32, s uintptr) int32
	cConv1d                                                                          func(res *uintptr, in, w uintptr, stride, pad, dil, groups int32, s uintptr) int32
	cContiguous                                                                      func(res *uintptr, a uintptr, colMajor bool, s uintptr) int32
	cRepeat                                                                          func(res *uintptr, a uintptr, repeats, axis int32, s uintptr) int32
	cRMSNorm                                                                         func(res *uintptr, x, w uintptr, eps float32, s uintptr) int32
	cLayerNorm                                                                       func(res *uintptr, x, w, b uintptr, eps float32, s uintptr) int32
	cRoPE                                                                            func(res *uintptr, x uintptr, dims int32, traditional bool, base uint64, scale float32, offset int32, freqs, s uintptr) int32
	cSDPA                                                                            func(res *uintptr, q, k, v uintptr, scale float32, maskMode string, mask, sinks, s uintptr) int32

	cKernelNew       func(name string, inputs, outputs uintptr, source, header string, rowContiguous, atomicOutputs bool) uintptr
	cKernelFree      func(k uintptr)
	cKernelApply     func(outs *uintptr, k, inputs, cfg, s uintptr) int32
	cKCfgNew         func() uintptr
	cKCfgFree        func(c uintptr)
	cKCfgAddOutput   func(c uintptr, shape *int32, n uintptr, dtype int32) int32
	cKCfgSetGrid     func(c uintptr, a, b, d int32) int32
	cKCfgSetGroup    func(c uintptr, a, b, d int32) int32
	cKCfgTmplDtype   func(c uintptr, name string, dtype int32) int32
	cKCfgTmplInt     func(c uintptr, name string, v int32) int32
	cKCfgSetInit     func(c uintptr, v float32) int32
	cDeviceNewType   func(typ int32, index int32) uintptr
	cDeviceFree      func(d uintptr) int32
	cDeviceInfoNew   func() uintptr
	cDeviceInfoGet   func(info *uintptr, dev uintptr) int32
	cDeviceInfoSize  func(v *uintptr, info uintptr, key string) int32
	cDeviceInfoFree  func(info uintptr) int32
	cClosureNewFunc  func(fn uintptr) uintptr
	cClosureFree     func(c uintptr) int32
	cClosureApply    func(res *uintptr, c, input uintptr) int32
	cCompile         func(res *uintptr, fn uintptr, shapeless bool) int32
	cVecArraySetData func(vec *uintptr, data *uintptr, n uintptr) int32
	cAutoreleasePush func() uintptr
	cAutoreleasePop  func(p uintptr)
)

var (
	openOnce sync.Once
	openErr  error
	openDir  string
	stream   uintptr // the default GPU stream
	lastErr  string
)

// Open unpacks the embedded runtime into DefaultLibDir()/<version>/ (once; checked) and loads it. It is safe to call
// more than once; the first call does the work.
func Open() (string, error) {
	openOnce.Do(func() { openDir, openErr = open(DefaultLibDir()) })
	return openDir, openErr
}

func open(libDir string) (string, error) {
	files, err := payloads()
	if err != nil {
		return "", err
	}
	dir, err := Extract(libDir, Version(), files)
	if err != nil {
		return "", fmt.Errorf("unpacking the MLX runtime into %s: %w", libDir, err)
	}
	// a command-line program gets no GPU from Metal unless CoreGraphics is loaded (see internal/tensor)
	_, _ = purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if objc, err := purego.Dlopen("/usr/lib/libobjc.A.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL); err == nil {
		purego.RegisterLibFunc(&cAutoreleasePush, objc, "objc_autoreleasePoolPush")
		purego.RegisterLibFunc(&cAutoreleasePop, objc, "objc_autoreleasePoolPop")
	}
	lib, err := purego.Dlopen(dir+"/libmlxc.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return "", fmt.Errorf("loading %s/libmlxc.dylib: %w", dir, err)
	}
	if err := bind(lib); err != nil {
		return "", err
	}
	cSetErrorHandler(purego.NewCallback(func(msg *byte, _ uintptr) {
		lastErr = goString(msg)
	}), 0, 0)
	var ok bool
	if cMetalAvailable(&ok) != 0 || !ok {
		return "", fmt.Errorf("MLX finds no Metal GPU")
	}
	stream = cGPUStream()
	return dir, nil
}

func bind(lib uintptr) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("libmlxc.dylib: %v", r)
		}
	}()
	reg := func(fptr any, name string) { purego.RegisterLibFunc(fptr, lib, name) }
	reg(&cArrayNew, "mlx_array_new")
	reg(&cArrayFree, "mlx_array_free")
	reg(&cArraySet, "mlx_array_set")
	reg(&cArrayNewData, "mlx_array_new_data")
	reg(&cArrayNewInt, "mlx_array_new_int")
	reg(&cArrayNewFloat32, "mlx_array_new_float32")
	reg(&cArrayNdim, "mlx_array_ndim")
	reg(&cArrayShape, "mlx_array_shape")
	reg(&cArrayDtype, "mlx_array_dtype")
	reg(&cArraySize, "mlx_array_size")
	reg(&cArrayEval, "mlx_array_eval")
	reg(&cArrayItemInt32, "mlx_array_item_int32")
	reg(&cArrayItemF32, "mlx_array_item_float32")
	reg(&cArrayDataF32, "mlx_array_data_float32")
	reg(&cArrayDataI32, "mlx_array_data_int32")
	reg(&cArrayDataU32, "mlx_array_data_uint32")
	reg(&cGPUStream, "mlx_default_gpu_stream_new")
	reg(&cCPUStream, "mlx_default_cpu_stream_new")
	reg(&cStreamFree, "mlx_stream_free")
	reg(&cVecArrayNewData, "mlx_vector_array_new_data")
	reg(&cVecArrayNew, "mlx_vector_array_new")
	reg(&cVecArrayFree, "mlx_vector_array_free")
	reg(&cVecArraySize, "mlx_vector_array_size")
	reg(&cVecArrayGet, "mlx_vector_array_get")
	reg(&cVecStrNewData, "mlx_vector_string_new_data")
	reg(&cVecStrFree, "mlx_vector_string_free")
	reg(&cEval, "mlx_eval")
	reg(&cAsyncEval, "mlx_async_eval")
	reg(&cLoadSafetensors, "mlx_load_safetensors")
	reg(&cMapArrNew, "mlx_map_string_to_array_new")
	reg(&cMapArrFree, "mlx_map_string_to_array_free")
	reg(&cMapArrItNew, "mlx_map_string_to_array_iterator_new")
	reg(&cMapArrItFree, "mlx_map_string_to_array_iterator_free")
	reg(&cMapArrItNext, "mlx_map_string_to_array_iterator_next")
	reg(&cMapStrNew, "mlx_map_string_to_string_new")
	reg(&cMapStrFree, "mlx_map_string_to_string_free")
	reg(&cSetErrorHandler, "mlx_set_error_handler")
	reg(&cMetalAvailable, "mlx_metal_is_available")
	reg(&cSetWiredLimit, "mlx_set_wired_limit")
	reg(&cSetCacheLimit, "mlx_set_cache_limit")
	reg(&cSetMemoryLimit, "mlx_set_memory_limit")
	reg(&cActiveMemory, "mlx_get_active_memory")
	reg(&cPeakMemory, "mlx_get_peak_memory")
	reg(&cCacheMemory, "mlx_get_cache_memory")
	reg(&cClearCache, "mlx_clear_cache")
	for name, f := range map[string]*binaryFn{"add": &cAdd, "subtract": &cSubtract, "multiply": &cMultiply, "divide": &cDivide,
		"maximum": &cMaximum, "matmul": &cMatmul, "logaddexp": &cLogaddexp, "floor_divide": &cFloorDivide,
		"less": &cLess, "greater_equal": &cGreaterEqual} {
		reg(f, "mlx_"+name)
	}
	for name, f := range map[string]*unaryFn{"sigmoid": &cSigmoid, "exp": &cExp, "log": &cLog, "negative": &cNegative,
		"square": &cSquare, "sqrt": &cSqrt, "rsqrt": &cRsqrt, "copy": &cCopy, "erf": &cErf} {
		reg(f, "mlx_"+name)
	}
	reg(&cCategorical, "mlx_random_categorical")
	reg(&cRandomSeed, "mlx_random_seed")
	reg(&cQuantizedMatmul, "mlx_quantized_matmul")
	reg(&cGatherQMM, "mlx_gather_qmm")
	reg(&cDequantize, "mlx_dequantize")
	reg(&cReshape, "mlx_reshape")
	reg(&cTransposeAxes, "mlx_transpose_axes")
	reg(&cSwapaxes, "mlx_swapaxes")
	reg(&cConcatenate, "mlx_concatenate_axis")
	reg(&cStack, "mlx_stack_axis")
	reg(&cSlice, "mlx_slice")
	reg(&cSliceUpdate, "mlx_slice_update")
	reg(&cTakeAxis, "mlx_take_axis")
	reg(&cTakeAlongAxis, "mlx_take_along_axis")
	reg(&cPutAlongAxis, "mlx_put_along_axis")
	reg(&cCumsum, "mlx_cumsum")
	reg(&cAstype, "mlx_astype")
	reg(&cSumAxis, "mlx_sum_axis")
	reg(&cMeanAxis, "mlx_mean_axis")
	reg(&cSoftmaxAxis, "mlx_softmax_axis")
	reg(&cArgpartition, "mlx_argpartition_axis")
	reg(&cArgsort, "mlx_argsort_axis")
	reg(&cArgmax, "mlx_argmax_axis")
	reg(&cExpandDims, "mlx_expand_dims")
	reg(&cSqueeze, "mlx_squeeze_axis")
	reg(&cWhere, "mlx_where")
	reg(&cZeros, "mlx_zeros")
	reg(&cConv1d, "mlx_conv1d")
	reg(&cContiguous, "mlx_contiguous")
	reg(&cRepeat, "mlx_repeat_axis")
	reg(&cRMSNorm, "mlx_fast_rms_norm")
	reg(&cLayerNorm, "mlx_fast_layer_norm")
	reg(&cRoPE, "mlx_fast_rope")
	reg(&cSDPA, "mlx_fast_scaled_dot_product_attention")
	reg(&cDeviceNewType, "mlx_device_new_type")
	reg(&cDeviceFree, "mlx_device_free")
	reg(&cDeviceInfoNew, "mlx_device_info_new")
	reg(&cDeviceInfoGet, "mlx_device_info_get")
	reg(&cDeviceInfoSize, "mlx_device_info_get_size")
	reg(&cDeviceInfoFree, "mlx_device_info_free")
	reg(&cClosureNewFunc, "mlx_closure_new_func")
	reg(&cClosureFree, "mlx_closure_free")
	reg(&cClosureApply, "mlx_closure_apply")
	reg(&cCompile, "mlx_compile")
	reg(&cVecArraySetData, "mlx_vector_array_set_data")
	reg(&cKernelNew, "mlx_fast_metal_kernel_new")
	reg(&cKernelFree, "mlx_fast_metal_kernel_free")
	reg(&cKernelApply, "mlx_fast_metal_kernel_apply")
	reg(&cKCfgNew, "mlx_fast_metal_kernel_config_new")
	reg(&cKCfgFree, "mlx_fast_metal_kernel_config_free")
	reg(&cKCfgAddOutput, "mlx_fast_metal_kernel_config_add_output_arg")
	reg(&cKCfgSetGrid, "mlx_fast_metal_kernel_config_set_grid")
	reg(&cKCfgSetGroup, "mlx_fast_metal_kernel_config_set_thread_group")
	reg(&cKCfgTmplDtype, "mlx_fast_metal_kernel_config_add_template_arg_dtype")
	reg(&cKCfgTmplInt, "mlx_fast_metal_kernel_config_add_template_arg_int")
	reg(&cKCfgSetInit, "mlx_fast_metal_kernel_config_set_init_value")
	return nil
}

func goString(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

// check turns a failed call into a panic with MLX's message (the chat round recovers it: Guard).
func check(rc int32) {
	if rc != 0 {
		msg := lastErr
		lastErr = ""
		if msg == "" {
			msg = "an MLX call failed"
		}
		if i := strings.LastIndex(msg, " at /"); i > 0 { // the source location in the build tree
			msg = msg[:i]
		}
		panic(&Error{Msg: msg})
	}
}

// Guard runs f and turns an MLX error (a panic of check) into an error.
func Guard(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*Error); ok {
				err = e
				return
			}
			panic(r)
		}
	}()
	return f()
}

func optInt(v int) uint64 { return uint64(uint32(int32(v))) | 1<<32 }

// WithAutoreleasePool runs f inside an Objective-C autorelease pool (MLX's Metal backend autoreleases objects; a Go
// thread has no pool of its own).
func WithAutoreleasePool(f func()) {
	if cAutoreleasePush == nil {
		f()
		return
	}
	p := cAutoreleasePush()
	defer cAutoreleasePop(p)
	f()
}
