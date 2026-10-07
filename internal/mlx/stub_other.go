//go:build !(darwin && arm64)

package mlx

// Open: no MLX in this build.
func Open() (string, error) { return "", ErrUnsupported }

// Version: no MLX in this build.
func Version() string { return "" }
