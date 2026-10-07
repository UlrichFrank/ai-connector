package mlx

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// The committed runtime (scripts/build-mlx.sh) unpacks to the bytes SHA256SUMS names — on every platform, so CI on
// Linux notices a runtime that was replaced without its sums (or the other way round).
func TestCommittedRuntime(t *testing.T) {
	raw, err := os.ReadFile("darwin_arm64/SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	sums, err := ParseSums(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"libmlxc.dylib", "mlx.metallib"} {
		fh, err := os.Open("darwin_arm64/" + name + ".zst")
		if err != nil {
			t.Fatal(err)
		}
		d, err := zstd.NewReader(fh, zstd.WithDecoderMaxWindow(1<<28))
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		n, err := io.Copy(h, d)
		d.Close()
		fh.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != sums[name] {
			t.Errorf("%s (%d bytes): sha256 %s, SHA256SUMS says %s", name, n, got, sums[name])
		}
	}
	lic, _ := os.ReadFile("darwin_arm64/LICENSE-MLX.txt")
	if !strings.Contains(string(lic), "MIT License") || !strings.Contains(string(lic), "mlx-c") {
		t.Error("LICENSE-MLX.txt lacks the MIT notices of MLX and mlx-c")
	}
}
