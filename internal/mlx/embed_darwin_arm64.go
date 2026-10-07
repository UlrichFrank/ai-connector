//go:build darwin && arm64

package mlx

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// The MLX runtime, built by scripts/build-mlx.sh: libmlxc.dylib (MLX + mlx-c) and mlx.metallib, zstd-compressed
// (about 11 MB; 126 MB unpacked), their SHA-256 sums and the MIT licence notices.
//
//go:embed darwin_arm64/libmlxc.dylib.zst darwin_arm64/mlx.metallib.zst darwin_arm64/SHA256SUMS darwin_arm64/VERSION darwin_arm64/LICENSE-MLX.txt
var runtimeFS embed.FS

// Version names the embedded runtime, e.g. "mlx-v0.31.1-3b50fd86506d" (the directory it is unpacked into).
func Version() string {
	v, _ := runtimeFS.ReadFile("darwin_arm64/VERSION")
	sums, _ := runtimeFS.ReadFile("darwin_arm64/SHA256SUMS")
	h := sha256.Sum256(sums)
	name := strings.Fields(string(v))
	tag := "mlx"
	if len(name) >= 2 {
		tag = "mlx-" + name[1]
	}
	return tag + "-" + hex.EncodeToString(h[:6])
}

// payloads are the files to unpack.
func payloads() ([]Payload, error) {
	raw, err := runtimeFS.ReadFile("darwin_arm64/SHA256SUMS")
	if err != nil {
		return nil, err
	}
	sums, err := ParseSums(string(raw))
	if err != nil {
		return nil, err
	}
	var out []Payload
	for _, name := range []string{"libmlxc.dylib", "mlx.metallib"} {
		sum, ok := sums[name]
		if !ok {
			return nil, fmt.Errorf("SHA256SUMS lists no %s", name)
		}
		out = append(out, Payload{Name: name, SHA256: sum, Open: func() (io.ReadCloser, error) {
			z, err := runtimeFS.ReadFile("darwin_arm64/" + name + ".zst")
			if err != nil {
				return nil, err
			}
			d, err := zstd.NewReader(bytes.NewReader(z), zstd.WithDecoderMaxWindow(1<<28), zstd.WithDecoderConcurrency(1))
			if err != nil {
				return nil, err
			}
			return d.IOReadCloser(), nil
		}})
	}
	lic, err := runtimeFS.ReadFile("darwin_arm64/LICENSE-MLX.txt")
	if err != nil {
		return nil, err
	}
	ls := sha256.Sum256(lic)
	out = append(out, Payload{Name: "LICENSE-MLX.txt", SHA256: hex.EncodeToString(ls[:]), Open: func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(lic)), nil
	}})
	return out, nil
}

// License is the licence notice of MLX and mlx-c (MIT).
func License() string {
	b, _ := runtimeFS.ReadFile("darwin_arm64/LICENSE-MLX.txt")
	return string(b)
}
