// Package mlx runs Apple's MLX (github.com/ml-explore/mlx, MIT) from Go without cgo: vboard-ai carries MLX and its C
// API (mlx-c) as one dylib plus MLX's Metal kernels (built by scripts/build-mlx.sh, embedded zstd-compressed on
// darwin/arm64), writes them once to ~/.config/mlxlab/lib/<version>/ (checked against their SHA-256) and loads
// them at run time with purego. Only the functions the chat model needs are bound (ops_darwin_arm64.go).
//
// Other platforms get Open() == ErrUnsupported: they have no MLX.
package mlx

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrUnsupported: this build has no MLX (only macOS on Apple silicon has).
var ErrUnsupported = errors.New("MLX runs on macOS on Apple silicon only")

// Error is an error MLX reported (a bad shape, out of memory, a missing kernel …).
type Error struct{ Msg string }

func (e *Error) Error() string { return "mlx: " + e.Msg }

// DefaultLibDir is where the MLX runtime is unpacked: ~/.config/mlxlab/lib ($MLXLAB_LIB_DIR,
// $XDG_CONFIG_HOME/vboard-ai/lib).
func DefaultLibDir() string {
	if d := os.Getenv("MLXLAB_LIB_DIR"); d != "" {
		return d
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "mlxlab", "lib")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "mlxlab", "lib")
	}
	return filepath.Join(os.TempDir(), "mlxlab-lib")
}
