//go:build darwin || (linux && (amd64 || arm64))

package tensor

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The system BLAS through its Fortran interface (sgemm_): every argument is a pointer, which keeps the call the same on
// every ABI (the C interface puts 32-bit ints on the stack, which Apple's arm64 ABI packs differently).
func init() {
	v := os.Getenv("MLXLAB_BLAS")
	if n, err := strconv.Atoi(os.Getenv("MLXLAB_GPU_MIN")); err == nil && n > 0 {
		gpuMin = n
	}
	if n, err := strconv.Atoi(os.Getenv("MLXLAB_OVERLAP")); err == nil && n > 0 {
		overlap = n
	}
	switch v {
	case "go":
		return
	case "", "metal", "accelerate", "openblas", "cpu":
	default:
		blas = openBLAS(v, v)
		return
	}
	// the GPU: by default on Apple Silicon (Metal), MLXLAB_BLAS=accelerate (or cpu, openblas) keeps to the CPU
	if v == "metal" || v == "" && runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		if err := UseGPU(true, 0); err != nil && v == "metal" {
			fmt.Fprintf(os.Stderr, "mlxlab: no GPU (%v): matrix products on the CPU\n", err)
		}
	}
	switch runtime.GOOS {
	case "darwin":
		if blas = openBLAS("/System/Library/Frameworks/Accelerate.framework/Accelerate", "accelerate"); blas != nil {
			blas.concurrent = true
		}
	case "linux":
		for _, lib := range []string{"libopenblas.so.0", "libopenblas.so", "libopenblasp.so.0", "libmkl_rt.so"} {
			if blas = openBLAS(lib, "openblas: "+lib); blas != nil {
				return
			}
		}
	}
}

func openBLAS(path, name string) *blasLib {
	h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil
	}
	sym, err := purego.Dlsym(h, "sgemm_")
	if err != nil {
		return nil
	}
	return &blasLib{name: name, gemm: func(transB bool, m, n, k int, alpha float32, a []float32, lda int, b []float32, ldb int, beta float32, c []float32, ldc int) {
		// row-major C = A·op(B) is column-major Cᵀ = op(B)ᵀ·Aᵀ, and a row-major matrix is its transpose column-major
		ta := byte('N')
		if transB {
			ta = 'T'
		}
		tb := byte('N')
		mm, nn, kk := int32(n), int32(m), int32(k)
		la, lb, lc := int32(ldb), int32(lda), int32(ldc)
		al, be := alpha, beta
		purego.SyscallN(sym,
			uintptr(unsafe.Pointer(&ta)), uintptr(unsafe.Pointer(&tb)),
			uintptr(unsafe.Pointer(&mm)), uintptr(unsafe.Pointer(&nn)), uintptr(unsafe.Pointer(&kk)),
			uintptr(unsafe.Pointer(&al)), uintptr(unsafe.Pointer(&b[0])), uintptr(unsafe.Pointer(&la)),
			uintptr(unsafe.Pointer(&a[0])), uintptr(unsafe.Pointer(&lb)),
			uintptr(unsafe.Pointer(&be)), uintptr(unsafe.Pointer(&c[0])), uintptr(unsafe.Pointer(&lc)))
		runtime.KeepAlive(a)
		runtime.KeepAlive(b)
		runtime.KeepAlive(c)
	}}
}
