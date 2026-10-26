package llm

// Images for the built-in vision-language models (Qwen3.5/3.6's vision tower; Qwen3-VL's preprocessing, after
// transformers' Qwen2VLImageProcessor and mlx-vlm's processing_qwen3_vl.py): the picture is scaled so both sides are
// multiples of 32 px and its area lies within [MinImagePixels, MaxImagePixels] (smart_resize), cut into 16×16 patches,
// and each 2×2 block of patches becomes one image token of the prompt. The rotary positions of those tokens are
// three-dimensional (MRoPE): a token's row and column in the picture, not its place in the sequence.

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"strings"

	_ "golang.org/x/image/webp"
)

// The area an image is scaled into. The minimum is the checkpoints' preprocessor_config.json (shortest_edge: 256×256);
// their maximum (longest_edge: 16 MP) would make a photo tens of thousands of tokens, so the engine stops at ~2 MP:
// a full-HD screenshot keeps its resolution (1920×1088, 2040 tokens).
const (
	MinImagePixels = 65536
	MaxImagePixels = 1 << 21
	// MaxSourcePixels: the largest image read at all (its header is checked before the pixels are decoded: a few MB
	// of PNG can unpack to gigapixels)
	MaxSourcePixels = 50_000_000
)

// Pixels is an image ready for the vision tower.
type Pixels struct {
	GridH, GridW int // patches down and across (multiples of the merge size)
	// Patches: GridH·GridW patches of PatchSize²·3 values in [−1, 1], ordered as the merger folds them — block row,
	// block column, row in the block, column in the block — each patch's pixels row by row, R, G, B per pixel (the
	// layout of the patch embedding's weight [out, T, P, P, C] with the frames summed).
	Patches []float32
}

// Tokens is the number of image tokens of a grid (one per MergeSize² patches).
func (v *VisionConfig) Tokens(gridH, gridW int) int {
	return gridH * gridW / (v.MergeSize * v.MergeSize)
}

// imageGrid is the patch grid an image of w×h pixels is scaled to.
func (v *VisionConfig) imageGrid(w, h int) (gridH, gridW int, err error) {
	rh, rw, err := smartResize(h, w, v.Factor(), MinImagePixels, MaxImagePixels)
	if err != nil {
		return 0, 0, err
	}
	return rh / v.PatchSize, rw / v.PatchSize, nil
}

// ImageGrid is the patch grid of an image, from its header alone (the prompt's token count; the pixels are read only
// for an image the prompt cache does not hold).
func (v *VisionConfig) ImageGrid(data []byte) (gridH, gridW int, err error) {
	cfg, err := imageSize(data)
	if err != nil {
		return 0, 0, err
	}
	return v.imageGrid(cfg.Width, cfg.Height)
}

// imageSize is the image's size from its header, at most MaxSourcePixels.
func imageSize(data []byte) (image.Config, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return cfg, fmt.Errorf("reading the image: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxSourcePixels {
		return cfg, fmt.Errorf("an image of %d×%d pixels: at most %d megapixels", cfg.Width, cfg.Height, MaxSourcePixels/1_000_000)
	}
	return cfg, nil
}

// smartResize is transformers' smart_resize: the size closest to h×w with both sides multiples of factor and the
// area within [minPixels, maxPixels], the aspect ratio kept as well as that allows.
func smartResize(h, w, factor, minPixels, maxPixels int) (int, int, error) {
	if h <= 0 || w <= 0 {
		return 0, 0, fmt.Errorf("an image of %d×%d pixels", w, h)
	}
	if float64(max(h, w))/float64(min(h, w)) > 200 {
		return 0, 0, fmt.Errorf("an image of %d×%d pixels: the sides' ratio is above 200", w, h)
	}
	f := float64(factor)
	hb := int(math.RoundToEven(float64(h)/f)) * factor // Python's round
	wb := int(math.RoundToEven(float64(w)/f)) * factor
	switch {
	case hb*wb > maxPixels:
		beta := math.Sqrt(float64(h*w) / float64(maxPixels))
		hb = max(factor, int(math.Floor(float64(h)/beta/f))*factor)
		wb = max(factor, int(math.Floor(float64(w)/beta/f))*factor)
	case hb*wb < minPixels:
		beta := math.Sqrt(float64(minPixels) / float64(h*w))
		hb = int(math.Ceil(float64(h)*beta/f)) * factor
		wb = int(math.Ceil(float64(w)*beta/f)) * factor
	}
	return hb, wb, nil
}

// PrepareImage decodes an image (PNG, JPEG, WebP, GIF: its first frame on the canvas), scales it (smartResize,
// bicubic like PIL) and cuts it into patches. Transparent pixels are laid over white. The size is the header's (as
// ImageGrid's): a GIF frame smaller than its canvas is placed on the canvas.
func (v *VisionConfig) PrepareImage(data []byte) (*Pixels, error) {
	cfg, err := imageSize(data)
	if err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("reading the image: %w", err)
	}
	if canvas := image.Rect(0, 0, cfg.Width, cfg.Height); img.Bounds() != canvas {
		c := image.NewNRGBA(canvas)
		draw.Draw(c, canvas, img, image.Point{}, draw.Src)
		img = c
	}
	gh, gw, err := v.imageGrid(cfg.Width, cfg.Height)
	if err != nil {
		return nil, err
	}
	P, M := v.PatchSize, v.MergeSize
	rgb := resizeBicubic(toRGB(img), cfg.Width, cfg.Height, gw*P, gh*P)
	W := gw * P
	px := &Pixels{GridH: gh, GridW: gw, Patches: make([]float32, 0, gh*gw*P*P*3)}
	for bh := 0; bh < gh/M; bh++ {
		for bw := 0; bw < gw/M; bw++ {
			for mh := 0; mh < M; mh++ {
				for mw := 0; mw < M; mw++ {
					y0, x0 := (bh*M+mh)*P, (bw*M+mw)*P
					for y := y0; y < y0+P; y++ {
						row := rgb[(y*W+x0)*3 : (y*W+x0+P)*3]
						for _, c := range row {
							// rescale to [0, 1], normalise with mean and std 0.5 (preprocessor_config.json)
							px.Patches = append(px.Patches, (float32(c)/255-0.5)/0.5)
						}
					}
				}
			}
		}
	}
	return px, nil
}

// toRGB is the image's pixels as R, G, B bytes, row by row (alpha laid over white).
func toRGB(img image.Image) []uint8 {
	b := img.Bounds()
	out := make([]uint8, 0, b.Dx()*b.Dy()*3)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			if c.A == 255 {
				out = append(out, c.R, c.G, c.B)
				continue
			}
			a := uint32(c.A)
			blend := func(v uint8) uint8 { return uint8((uint32(v)*a + 255*(255-a) + 127) / 255) }
			out = append(out, blend(c.R), blend(c.G), blend(c.B))
		}
	}
	return out
}

// resizeBicubic scales RGB bytes from w×h to nw×nh the way PIL's Image.resize(BICUBIC) does: a separable bicubic
// filter (a = −0.5) widened by the scale when shrinking (antialiasing), horizontal pass first, each pass rounded to
// bytes.
func resizeBicubic(src []uint8, w, h, nw, nh int) []uint8 {
	if w != nw {
		src = resamplePass(src, w, h, nw, true)
		w = nw
	}
	if h != nh {
		src = resamplePass(src, w, h, nh, false)
	}
	return src
}

func bicubic(x float64) float64 {
	const a = -0.5
	x = math.Abs(x)
	switch {
	case x < 1:
		return ((a+2)*x-(a+3))*x*x + 1
	case x < 2:
		return (((x-5)*x+8)*x - 4) * a
	}
	return 0
}

// resamplePass scales one axis (horizontal: the width w to n; else the height h to n).
func resamplePass(src []uint8, w, h, n int, horizontal bool) []uint8 {
	in := h
	if horizontal {
		in = w
	}
	scale := float64(in) / float64(n)
	fscale := max(scale, 1)
	support := 2 * fscale
	type tap struct {
		from int
		w    []float64
	}
	taps := make([]tap, n)
	for i := range taps {
		center := (float64(i) + 0.5) * scale
		lo := max(int(center-support+0.5), 0)
		hi := min(int(center+support+0.5), in)
		ws := make([]float64, hi-lo)
		sum := 0.0
		for k := range ws {
			ws[k] = bicubic((float64(lo+k) - center + 0.5) / fscale)
			sum += ws[k]
		}
		if sum != 0 {
			for k := range ws {
				ws[k] /= sum
			}
		}
		taps[i] = tap{lo, ws}
	}
	clip := func(v float64) uint8 { return uint8(min(max(math.Round(v), 0), 255)) }
	if horizontal {
		out := make([]uint8, n*h*3)
		for y := 0; y < h; y++ {
			for i, t := range taps {
				var r, g, b float64
				for k, wt := range t.w {
					p := src[(y*w+t.from+k)*3:]
					r, g, b = r+wt*float64(p[0]), g+wt*float64(p[1]), b+wt*float64(p[2])
				}
				o := out[(y*n+i)*3:]
				o[0], o[1], o[2] = clip(r), clip(g), clip(b)
			}
		}
		return out
	}
	out := make([]uint8, w*n*3)
	for i, t := range taps {
		for x := 0; x < w; x++ {
			var r, g, b float64
			for k, wt := range t.w {
				p := src[((t.from+k)*w+x)*3:]
				r, g, b = r+wt*float64(p[0]), g+wt*float64(p[1]), b+wt*float64(p[2])
			}
			o := out[(i*w+x)*3:]
			o[0], o[1], o[2] = clip(r), clip(g), clip(b)
		}
	}
	return out
}

// ---------- the prompt ----------

// The placeholder Render writes where an image goes (Qwen3.5's chat template); the engine widens the image token to
// as many as the image has (VisionConfig.Tokens).
const imagePlaceholder = "<|vision_start|><|image_pad|><|vision_end|>"

// PromptImages are the images of msgs in the order Render places them (the user messages' Parts).
func PromptImages(msgs []Message) []Image {
	var out []Image
	for _, m := range msgs {
		if m.Role != "user" || len(m.Images) == 0 {
			continue
		}
		for _, p := range Parts(m.Content, m.Images) {
			if p.Image != nil {
				out = append(out, *p.Image)
			}
		}
	}
	return out
}

// userContent is a user message's text for the prompt: each image's placeholder after its tag.
func userContent(m Message) string {
	if len(m.Images) == 0 {
		return m.Content
	}
	var b strings.Builder
	for _, p := range Parts(m.Content, m.Images) {
		if p.Image != nil {
			b.WriteString(imagePlaceholder)
		} else {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// imageSpan is an image's run of tokens in the prompt.
type imageSpan struct {
	id           string
	from, n      int // the first image token and how many
	gridH, gridW int
	sum          [32]byte // the image's SHA-256: the prompt cache tells images apart by it
	data         []byte
}

// expandImages widens each image token of ids to its image's tokens. The prompt has as many image tokens as images.
func (v *VisionConfig) expandImages(ids []int, images []Image) ([]int, []imageSpan, error) {
	n := 0
	for _, id := range ids {
		if id == v.ImageToken {
			n++
		}
	}
	if n != len(images) {
		return nil, nil, fmt.Errorf("the prompt has %d image places for %d images", n, len(images))
	}
	if n == 0 {
		return ids, nil, nil
	}
	out := make([]int, 0, len(ids))
	spans := make([]imageSpan, 0, n)
	for _, id := range ids {
		if id != v.ImageToken {
			out = append(out, id)
			continue
		}
		im := images[len(spans)]
		gh, gw, err := v.ImageGrid(im.Data)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", im.ID, err)
		}
		s := imageSpan{id: im.ID, from: len(out), n: v.Tokens(gh, gw), gridH: gh, gridW: gw, sum: sha256.Sum256(im.Data), data: im.Data}
		for range s.n {
			out = append(out, v.ImageToken)
		}
		spans = append(spans, s)
	}
	return out, spans, nil
}

// ropePositions are the tokens' MRoPE positions (T, H, W) and the position after the last: a text token's is its
// place on all three, counted on from the last; an image's tokens start at the next place and add their row (H) and
// column (W) in merged tokens; the text after an image goes on after its larger side (Qwen3-VL's get_rope_index).
func (v *VisionConfig) ropePositions(n int, spans []imageSpan) ([][3]int, int) {
	out := make([][3]int, n)
	p, i := 0, 0
	for _, s := range spans {
		for ; i < s.from; i++ {
			out[i] = [3]int{p, p, p}
			p++
		}
		h, w := s.gridH/v.MergeSize, s.gridW/v.MergeSize
		for k := 0; k < s.n; k++ {
			out[i+k] = [3]int{p, p + k/w, p + k%w}
		}
		i += s.n
		p += max(h, w)
	}
	for ; i < n; i++ {
		out[i] = [3]int{p, p, p}
		p++
	}
	return out, p
}

// mropeAxis is the axis (0 T, 1 H, 2 W) each of the half rotary frequencies turns with (interleaved MRoPE).
func mropeAxis(half int, sections [3]int) []int {
	out := make([]int, half)
	for j := range out {
		switch {
		case j%3 == 1 && j < 3*sections[1]:
			out[j] = 1
		case j%3 == 2 && j < 3*sections[2]:
			out[j] = 2
		}
	}
	return out
}

// mropeTables are cos and sin of every token's rotary angles ([L, ropeDims/2], row by row) for positions pos:
// frequency j = theta^(−2j/ropeDims) at the token's position on its axis.
func mropeTables(pos [][3]int, ropeDims int, theta float64, sections [3]int) (cos, sin []float32) {
	half := ropeDims / 2
	axis := mropeAxis(half, sections)
	inv := make([]float64, half)
	for j := range inv {
		inv[j] = 1 / math.Pow(theta, float64(2*j)/float64(ropeDims))
	}
	cos, sin = make([]float32, len(pos)*half), make([]float32, len(pos)*half)
	for i, p := range pos {
		for j := range half {
			// float32 angles, like the reference's freqs (position × inv_freq in float32)
			a := float64(float32(p[axis[j]]) * float32(inv[j]))
			cos[i*half+j], sin[i*half+j] = float32(math.Cos(a)), float32(math.Sin(a))
		}
	}
	return cos, sin
}
