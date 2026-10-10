package hub

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func pngChunk(typ string, data []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	b = append(b, typ...)
	b = append(b, data...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(append([]byte(typ), data...)))
}

func TestPNGWithoutMeta(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	img.Set(1, 1, color.RGBA{200, 10, 10, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	plain := buf.Bytes()
	// the metadata mflux writes, after IHDR (8 signature + 25 IHDR bytes)
	var meta []byte
	for _, c := range []string{"iTXt", "tEXt", "eXIf", "pHYs"} {
		meta = append(meta, pngChunk(c, []byte("prompt: a lighthouse"))...)
	}
	with := append(append(append([]byte{}, plain[:33]...), meta...), plain[33:]...)
	got := pngWithoutMeta(with)
	if !bytes.Equal(got, plain) {
		t.Fatalf("got %d bytes, want the %d of the plain PNG", len(got), len(plain))
	}
	m, err := png.Decode(bytes.NewReader(got))
	if err != nil || m.Bounds().Dx() != 4 {
		t.Fatalf("decode: %v", err)
	}
	if r, _, _, _ := m.At(1, 1).RGBA(); r>>8 != 200 {
		t.Fatalf("pixel %v", m.At(1, 1))
	}
	// not a PNG, or cut short: as it was
	for _, b := range [][]byte{[]byte("GIF89a"), with[:40]} {
		if !bytes.Equal(pngWithoutMeta(b), b) {
			t.Fatalf("%q changed", b[:6])
		}
	}
}
