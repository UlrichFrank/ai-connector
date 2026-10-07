//go:build !darwin

package tensor

import "errors"

// openGPU: a GPU only on macOS (Metal) for now.
func openGPU() (gpuBackend, error) { return nil, errors.New("no GPU support on this platform") }
