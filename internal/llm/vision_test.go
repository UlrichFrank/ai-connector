package llm

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"slices"
	"strings"
	"testing"
)

// Qwen3.5-4B's config.json, the parts the vision engine reads.
const visionConfigJSON = `{"model_type": "qwen3_5", "image_token_id": 248056, "vision_start_token_id": 248053, "vision_end_token_id": 248054,
 "quantization": {"group_size": 64, "bits": 4, "mode": "affine"},
 "text_config": {"hidden_size": 2560, "num_hidden_layers": 32, "num_attention_heads": 16, "num_key_value_heads": 4, "head_dim": 256,
  "vocab_size": 248320, "linear_num_key_heads": 16, "linear_num_value_heads": 32, "linear_key_head_dim": 128, "linear_value_head_dim": 128,
  "linear_conv_kernel_dim": 4, "rope_parameters": {"mrope_interleaved": true, "mrope_section": [11, 11, 10], "rope_type": "default",
  "rope_theta": 10000000, "partial_rotary_factor": 0.25}},
 "vision_config": {"deepstack_visual_indexes": [], "depth": 24, "hidden_act": "gelu_pytorch_tanh", "hidden_size": 1024, "in_channels": 3,
  "intermediate_size": 4096, "num_heads": 16, "num_position_embeddings": 2304, "out_hidden_size": 2560, "patch_size": 16,
  "spatial_merge_size": 2, "temporal_patch_size": 2}}`

func visionConfig(t *testing.T) *VisionConfig {
	t.Helper()
	c, err := ParseConfig([]byte(visionConfigJSON))
	if err != nil {
		t.Fatal(err)
	}
	if c.Vision == nil {
		t.Fatal("no vision config")
	}
	if c.MRoPESections != [3]int{11, 11, 10} {
		t.Fatalf("mrope sections %v", c.MRoPESections)
	}
	return c.Vision
}

func TestParseVisionConfig(t *testing.T) {
	v := visionConfig(t)
	if v.Depth != 24 || v.Hidden != 1024 || v.OutHidden != 2560 || v.Factor() != 32 || v.ImageToken != 248056 {
		t.Fatalf("%+v", v)
	}
	// Qwen3-VL's deepstack features are not run: the model is text only
	deep := strings.Replace(visionConfigJSON, `"deepstack_visual_indexes": []`, `"deepstack_visual_indexes": [5, 11]`, 1)
	if c, err := ParseConfig([]byte(deep)); err != nil || c.Vision != nil {
		t.Fatalf("deepstack: %v, %v", c.Vision, err)
	}
	// no image token ids: an error, not a model that would mix up images
	noIDs := strings.Replace(visionConfigJSON, `"image_token_id": 248056, `, "", 1)
	if _, err := ParseConfig([]byte(noIDs)); err == nil {
		t.Fatal("a vision config without image_token_id was taken")
	}
}

func TestSmartResize(t *testing.T) {
	// the sizes: mlx-vlm's _smart_resize_image with the same bounds
	for _, c := range []struct{ h, w, wantH, wantW int }{
		{463, 357, 448, 352},     // mlx-vlm's grid for this size: 28×22 patches
		{520, 900, 512, 896},     // 32×56
		{1080, 1920, 1088, 1920}, // full HD stays (2.09 MP ≤ 2 MiP)
		{150, 60, 416, 192},      // a small crop grows to 64k pixels
		{3000, 4000, 1248, 1664}, // a photo shrinks under the maximum
	} {
		h, w, err := smartResize(c.h, c.w, 32, MinImagePixels, MaxImagePixels)
		if err != nil || h != c.wantH || w != c.wantW {
			t.Errorf("%d×%d: %d×%d %v, want %d×%d", c.h, c.w, h, w, err, c.wantH, c.wantW)
		}
		if h*w > MaxImagePixels || h%32 != 0 || w%32 != 0 {
			t.Errorf("%d×%d: %d×%d is not a valid size", c.h, c.w, h, w)
		}
	}
	if _, _, err := smartResize(10, 3000, 32, MinImagePixels, MaxImagePixels); err == nil {
		t.Error("a ratio above 200 was taken")
	}
}

func pngOf(t *testing.T, w, h int, at func(x, y int) color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, at(x, y))
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestPrepareImage(t *testing.T) {
	v := visionConfig(t)
	// 512×256 needs no scaling: the patches are the pixels, in merge order, R G B per pixel
	data := pngOf(t, 512, 256, func(x, y int) color.Color {
		if x >= 32 && x < 48 && y < 16 { // the third patch of the first row: patch (0, 2)
			return color.NRGBA{255, 0, 0, 255}
		}
		return color.NRGBA{0, 0, 0, 0} // transparent: laid over white
	})
	px, err := v.PrepareImage(data)
	if err != nil {
		t.Fatal(err)
	}
	if px.GridH != 16 || px.GridW != 32 || len(px.Patches) != 16*32*16*16*3 {
		t.Fatalf("grid %d×%d, %d values", px.GridH, px.GridW, len(px.Patches))
	}
	// merge order: block (0,0) holds patches (0,0) (0,1) (1,0) (1,1); patch (0,2) is the first of block (0,1): the 5th
	per := 16 * 16 * 3
	red := px.Patches[4*per : 5*per]
	for i := 0; i < len(red); i += 3 {
		if red[i] != 1 || red[i+1] != -1 || red[i+2] != -1 {
			t.Fatalf("patch (0,2) pixel %d: %v", i/3, red[i:i+3])
		}
	}
	if w := px.Patches[0:3]; !slices.Equal(w, []float32{1, 1, 1}) {
		t.Fatalf("a transparent pixel: %v, want white", w)
	}
	if v.Tokens(px.GridH, px.GridW) != 128 {
		t.Fatalf("%d tokens", v.Tokens(px.GridH, px.GridW))
	}
	gh, gw, err := v.ImageGrid(data)
	if err != nil || gh != 16 || gw != 32 {
		t.Fatalf("ImageGrid %d×%d %v", gh, gw, err)
	}
}

func TestResizeBicubic(t *testing.T) {
	// a flat colour stays flat whichever way it is scaled
	src := make([]uint8, 7*5*3)
	for i := range src {
		src[i] = []uint8{200, 100, 50}[i%3]
	}
	for _, sz := range [][2]int{{14, 10}, {3, 2}, {7, 9}} {
		out := resizeBicubic(src, 7, 5, sz[0], sz[1])
		if len(out) != sz[0]*sz[1]*3 {
			t.Fatalf("%v: %d bytes", sz, len(out))
		}
		for i, c := range out {
			if want := []uint8{200, 100, 50}[i%3]; c != want {
				t.Fatalf("%v: byte %d is %d, want %d", sz, i, c, want)
			}
		}
	}
}

func TestPromptImages(t *testing.T) {
	a, b := Image{ID: "image-1", MIME: "image/png", Data: []byte{1}}, Image{ID: "image-2", MIME: "image/png", Data: []byte{2}}
	msgs := []Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "compare [image-2] with [image-1]", Images: []Image{a, b}},
		{Role: "assistant", Content: "they differ"},
		{Role: "user", Content: "and this?", Images: []Image{a}},
	}
	got := PromptImages(msgs)
	if len(got) != 3 || got[0].ID != "image-2" || got[1].ID != "image-1" || got[2].ID != "image-1" {
		t.Fatalf("%v", got)
	}
	p := Render("xml", msgs, nil, false)
	want := "<|im_start|>user\ncompare [image-2]" + imagePlaceholder + " with [image-1]" + imagePlaceholder + "<|im_end|>"
	if !strings.Contains(p, want) {
		t.Fatalf("prompt:\n%s", p)
	}
	if !strings.Contains(p, "<|im_start|>user\nand this? [image-1]"+imagePlaceholder+"<|im_end|>") {
		t.Fatalf("an image without its tag in the text:\n%s", p)
	}
	if strings.Count(p, "<|image_pad|>") != 3 {
		t.Fatalf("%d placeholders", strings.Count(p, "<|image_pad|>"))
	}
}

func TestExpandImagesAndPositions(t *testing.T) {
	v := visionConfig(t)
	img := Image{ID: "image-1", MIME: "image/png", Data: pngOf(t, 256, 320, func(x, y int) color.Color { return color.Gray{128} })}
	pad := v.ImageToken
	ids := []int{1, 2, v.VisionStart, pad, v.VisionEnd, 3}
	out, spans, err := v.expandImages(ids, []Image{img})
	if err != nil {
		t.Fatal(err)
	}
	// 320×256 → 20×16 patches → 10×8 tokens
	if len(spans) != 1 || spans[0].from != 3 || spans[0].n != 80 || len(out) != 5+80 {
		t.Fatalf("%d tokens, spans %+v", len(out), spans)
	}
	if _, _, err := v.expandImages(ids, nil); err == nil {
		t.Fatal("an image place without an image was taken")
	}
	pos, next := v.ropePositions(len(out), spans)
	// text 0, 1, 2 (vision_start); the image from 3: row 0…9 on H, column 0…7 on W; then 3 + max(10, 8) = 13
	if pos[2] != [3]int{2, 2, 2} || pos[3] != [3]int{3, 3, 3} || pos[3+9] != [3]int{3, 4, 4} || pos[3+79] != [3]int{3, 12, 10} {
		t.Fatalf("image positions %v %v %v", pos[3], pos[12], pos[82])
	}
	if pos[83] != [3]int{13, 13, 13} || pos[84] != [3]int{14, 14, 14} || next != 15 {
		t.Fatalf("after the image: %v %v, next %d", pos[83], pos[84], next)
	}
}

func TestMRoPEAxis(t *testing.T) {
	ax := mropeAxis(32, [3]int{11, 11, 10})
	// H on 1, 4, … 31; W on 2, 5, … 29; T on the rest
	h, w := 0, 0
	for j, a := range ax {
		switch a {
		case 1:
			h++
			if j%3 != 1 {
				t.Fatalf("H at %d", j)
			}
		case 2:
			w++
			if j%3 != 2 {
				t.Fatalf("W at %d", j)
			}
		}
	}
	if h != 11 || w != 10 || ax[0] != 0 || ax[30] != 0 || ax[31] != 1 {
		t.Fatalf("H %d, W %d: %v", h, w, ax)
	}
	// text positions turn every frequency alike: the tables are ordinary RoPE's
	cos, _ := mropeTables([][3]int{{5, 5, 5}}, 64, 1e7, [3]int{11, 11, 10})
	plain, _ := mropeTables([][3]int{{5, 0, 0}}, 64, 1e7, [3]int{})
	if !slices.Equal(cos, plain) {
		t.Fatal("text positions differ from ordinary RoPE")
	}
}

func TestImageLimits(t *testing.T) {
	v := visionConfig(t)
	// a PNG whose header claims 100000×100000 pixels: refused from the header, the pixels are never decoded
	data := pngOf(t, 8, 8, func(x, y int) color.Color { return color.White })
	ihdr := data[8+8 : 8+8+13] // signature, length + "IHDR", then width, height, …
	binary.BigEndian.PutUint32(ihdr[0:], 100000)
	binary.BigEndian.PutUint32(ihdr[4:], 100000)
	binary.BigEndian.PutUint32(data[8+8+13:], crc32.ChecksumIEEE(data[8+4:8+8+13]))
	if _, _, err := v.ImageGrid(data); err == nil || !strings.Contains(err.Error(), "megapixels") {
		t.Fatalf("ImageGrid: %v", err)
	}
	if _, err := v.PrepareImage(data); err == nil || !strings.Contains(err.Error(), "megapixels") {
		t.Fatalf("PrepareImage: %v", err)
	}

	// a GIF whose first frame is smaller than its canvas: the pixels have the header's grid
	pal := color.Palette{color.Black, color.White}
	frame := image.NewPaletted(image.Rect(0, 0, 40, 30), pal)
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, &gif.GIF{Image: []*image.Paletted{frame}, Delay: []int{0},
		Config: image.Config{ColorModel: pal, Width: 600, Height: 400}}); err != nil {
		t.Fatal(err)
	}
	gh, gw, err := v.ImageGrid(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	px, err := v.PrepareImage(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if px.GridH != gh || px.GridW != gw {
		t.Fatalf("header grid %d×%d, pixels %d×%d", gh, gw, px.GridH, px.GridW)
	}
}
