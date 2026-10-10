package hub

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // image.Decode and DecodeConfig read PNGs
)

// Upscales on vboard's board (docs/api.md, "Upscaling"): a generator of kind "upscale" (SeedVR2) takes one image
// and a scale and makes it that many times larger, the longer side at most genUpscaleMax. No prompt.

// planUpscale checks an upscale request: exactly one image, a scale (not one it takes: the nearest below, the
// smallest when none is), a source small enough to grow. The result's size is worked out as the worker does
// (upscaleSize).
func planUpscale(g advertisedGenerator, q *genRequest) (genPlan, error) {
	var p genPlan
	switch {
	case q.Scale < 0 || q.Seed < 0:
		return p, errors.New("scale and seed are 0 (the default) or more")
	case len(q.Images) != 1:
		return p, fmt.Errorf("%s upscales one image: send exactly one (not %d)", g.Name, len(q.Images))
	}
	b, ext, err := genImage(q.Images[0])
	if err != nil {
		return p, err
	}
	w, h, err := imageSize(b)
	if err != nil {
		return p, fmt.Errorf("the image: %v", err)
	}
	if max(w, h) > g.Max/2 {
		return p, fmt.Errorf("the image (%d×%d) is already as large as %s can make it: at most %d pixels on the longer side, "+
			"so the image may have at most %d", w, h, g.Name, g.Max, g.Max/2)
	}
	p.scale = g.Scales[0]
	for _, s := range g.Scales {
		if s <= q.Scale {
			p.scale = s
		}
	}
	p.width, p.height = upscaleSize(w, h, p.scale, g.Max)
	p.images, p.exts = [][]byte{b}, []string{ext}
	return p, nil
}

// upscaleSize is what the worker makes of a w×h image (engine.UpscaleSize, as mflux works it out): the shorter side
// scale times as long (less when the longer side would pass maxSide), the other in proportion, both rounded down
// to even.
func upscaleSize(w, h, scale, maxSide int) (int, int) {
	short, long := min(w, h), max(w, h)
	if short <= 0 || scale <= 0 {
		return 0, 0
	}
	res := short * scale
	if long*scale > maxSide {
		res = short * maxSide / long
	}
	f := float64(res) / float64(short)
	return int(float64(w)*f) / 2 * 2, int(float64(h)*f) / 2 * 2
}

// imageSize reads an image's width and height from its header: PNG and JPEG through image.DecodeConfig, WebP from
// its first chunk (the standard library has no WebP decoder).
func imageSize(b []byte) (int, int, error) {
	if len(b) >= 30 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		switch string(b[12:16]) {
		case "VP8 ": // lossy: the key frame's header after its start code
			if b[23] == 0x9d && b[24] == 0x01 && b[25] == 0x2a {
				return int(binary.LittleEndian.Uint16(b[26:]) & 0x3fff), int(binary.LittleEndian.Uint16(b[28:]) & 0x3fff), nil
			}
		case "VP8L": // lossless: 14 bits each, less one
			if b[20] == 0x2f {
				v := binary.LittleEndian.Uint32(b[21:])
				return int(v&0x3fff) + 1, int(v>>14&0x3fff) + 1, nil
			}
		case "VP8X": // extended: the canvas, 24 bits each, less one
			le24 := func(x []byte) int { return int(x[0]) | int(x[1])<<8 | int(x[2])<<16 }
			return le24(b[24:]) + 1, le24(b[27:]) + 1, nil
		}
		return 0, 0, errors.New("a WebP image whose size cannot be read")
	}
	c, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return 0, 0, fmt.Errorf("its size cannot be read (%v)", err)
	}
	return c.Width, c.Height, nil
}

// asJPEG re-encodes an image (a PNG too large for vboard) as a JPEG of quality 92.
func asJPEG(b []byte) ([]byte, error) {
	im, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, im, &jpeg.Options{Quality: genJPEGQuality}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
