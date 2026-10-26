//go:build darwin && arm64

package llm

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/mlx"
)

// Parity with mlx-vlm (TestVisionStages compares with its tower in float32): MLXLAB_LLM_DIR=<Qwen3.5 folder> MLXLAB_VLM_REF=<file> MLXLAB_VLM_IMAGES=<image,…> ({"prompt",
// "ids" (the image tokens widened), "grids", "pixels" of the first image, "feats" of all images from mlx-vlm's pixels,
// "logits" of the last position}; written by mlx-vlm's processor and model for the same prompt and images).
func TestVisionParity(t *testing.T) {
	dir, ref, list := os.Getenv("MLXLAB_LLM_DIR"), os.Getenv("MLXLAB_VLM_REF"), os.Getenv("MLXLAB_VLM_IMAGES")
	if dir == "" || ref == "" || list == "" {
		t.Skip("MLXLAB_LLM_DIR / MLXLAB_VLM_REF / MLXLAB_VLM_IMAGES not set")
	}
	raw, err := os.ReadFile(ref)
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Prompt                string
		IDs                   []int
		Grids                 [][3]int
		Pixels, Feats, Logits []float32
		Argmax                int
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	var images []Image
	for i, p := range strings.Split(list, ",") {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		images = append(images, Image{ID: "image-" + string(rune('1'+i)), MIME: "image/" + strings.TrimPrefix(filepath.Ext(p), "."), Data: data})
	}
	e, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if !e.Images() {
		t.Fatal("no vision tower")
	}
	v := e.Cfg.Vision

	// the preprocessing: the grid and the first image's pixels (PIL rounds differently here and there: a step of 1/255)
	px, err := v.PrepareImage(images[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if px.GridH != r.Grids[0][1] || px.GridW != r.Grids[0][2] {
		t.Fatalf("grid %d×%d, mlx-vlm %d×%d", px.GridH, px.GridW, r.Grids[0][1], r.Grids[0][2])
	}
	// mlx-vlm's patches carry both frames: [C, T, P, P]; ours one frame, [P, P, C]
	P := v.PatchSize
	per := 3 * v.TemporalPatch * P * P
	var maxPx float32
	far := 0
	for i := 0; i < px.GridH*px.GridW; i++ {
		for y := range P {
			for x := range P {
				for c := range 3 {
					got := px.Patches[i*P*P*3+(y*P+x)*3+c]
					want := r.Pixels[i*per+c*v.TemporalPatch*P*P+y*P+x]
					d := abs32(got - want)
					maxPx = max(maxPx, d)
					if d > 2.5/127.5 {
						far++
					}
				}
			}
		}
	}
	t.Logf("pixels: max difference %.4f, %d of %d values more than two steps off", maxPx, far, len(px.Patches))
	if float64(far) > 0.001*float64(len(px.Patches)) {
		t.Fatalf("the pixels differ: %d values more than two steps off", far)
	}

	// the vision tower on mlx-vlm's pixels
	var feats []float32
	err = e.do(func() error {
		mine := &Pixels{GridH: px.GridH, GridW: px.GridW, Patches: make([]float32, len(px.Patches))}
		for i := 0; i < px.GridH*px.GridW; i++ {
			for y := range P {
				for x := range P {
					for c := range 3 {
						mine.Patches[i*P*P*3+(y*P+x)*3+c] = r.Pixels[i*per+c*v.TemporalPatch*P*P+y*P+x]
					}
				}
			}
		}
		feats = mlx.AsType(e.md.vision.encode(mine, mlx.Float32), mlx.Float32).Float32s()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	n := len(feats)
	if n > len(r.Feats) {
		t.Fatalf("%d features, mlx-vlm %d", n, len(r.Feats))
	}
	cos, rel := cosine(feats, r.Feats[:n])
	// mlx-vlm runs the tower in bf16, which is itself ~8 % off float32 (a few tokens far off); ours runs in float32
	t.Logf("vision features: cosine %.6f, relative error %.4f (to mlx-vlm's bf16)", cos, rel)
	if cos < 0.995 {
		t.Fatalf("the vision features differ: cosine %.6f", cos)
	}

	// the whole prompt: tokens, then the logits of the last position (chunked like a chat)
	ids := e.tok.Encode(r.Prompt)
	ids, spans, err := v.expandImages(ids, images)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, r.IDs) {
		t.Fatalf("prompt tokens differ: %d, mlx-vlm %d", len(ids), len(r.IDs))
	}
	var logits []float32
	var st Stats
	err = e.do(func() error {
		c, l, err := e.prefill(context.Background(), ids, spans, &st)
		if err != nil {
			return err
		}
		defer c.free()
		logits = mlx.Reshape(l, l.Size()).Float32s()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cos, rel = cosine(logits, r.Logits)
	arg := slices.Index(logits, slices.Max(logits))
	t.Logf("logits: cosine %.6f, relative error %.4f, top token %d (%q), mlx-vlm %d; %d images in %v",
		cos, rel, arg, e.tok.DecodeBytes([]int{arg}, false), r.Argmax, st.Images, st.Vision)
	if cos < 0.995 || arg != r.Argmax {
		t.Fatalf("the logits differ: cosine %.6f, top token %d (mlx-vlm %d)", cos, arg, r.Argmax)
	}
}

func cosine(a, b []float32) (cos, rel float64) {
	var ab, aa, bb, dd float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		ab, aa, bb, dd = ab+x*y, aa+x*x, bb+y*y, dd+(x-y)*(x-y)
	}
	return ab / math.Sqrt(aa*bb), math.Sqrt(dd / bb)
}

// The tower's stages next to mlx-vlm's: MLXLAB_VLM_STAGES=<file> ({"<stage>": [first values …]}) with the variables
// of TestVisionParity.
func TestVisionStages(t *testing.T) {
	dir, ref, stages := os.Getenv("MLXLAB_LLM_DIR"), os.Getenv("MLXLAB_VLM_REF"), os.Getenv("MLXLAB_VLM_STAGES")
	if dir == "" || ref == "" || stages == "" {
		t.Skip("MLXLAB_LLM_DIR / MLXLAB_VLM_REF / MLXLAB_VLM_STAGES not set")
	}
	var r struct {
		Grids  [][3]int
		Pixels []float32
	}
	raw, _ := os.ReadFile(ref)
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	var want map[string][]float32
	raw, _ = os.ReadFile(stages)
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	e, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	v := e.Cfg.Vision
	P, per := v.PatchSize, 3*v.TemporalPatch*v.PatchSize*v.PatchSize
	px := &Pixels{GridH: r.Grids[0][1], GridW: r.Grids[0][2]}
	px.Patches = make([]float32, px.GridH*px.GridW*P*P*3)
	for i := 0; i < px.GridH*px.GridW; i++ {
		for y := range P {
			for x := range P {
				for c := range 3 {
					px.Patches[i*P*P*3+(y*P+x)*3+c] = r.Pixels[i*per+c*v.TemporalPatch*P*P+y*P+x]
				}
			}
		}
	}
	blocks := 0
	visionTrace = func(name string, a array) {
		if name == "block" {
			name = "block" + strconv.Itoa(blocks)
			blocks++
		}
		w, ok := want[name]
		if !ok {
			return
		}
		got := mlx.Reshape(mlx.AsType(a, mlx.Float32), a.Size()).Float32s()[:len(w)]
		if d := os.Getenv("MLXLAB_VLM_DUMP"); d != "" {
			b, _ := json.Marshal(got)
			_ = os.WriteFile(filepath.Join(d, name+".json"), b, 0o644)
		}
		cos, rel := cosine(got, w)
		t.Logf("%-7s cosine %.6f, relative error %.4f", name, cos, rel)
	}
	defer func() { visionTrace = func(string, array) {} }()
	_ = e.do(func() error { e.md.vision.encode(px, mlx.Float32); return nil })
}

// A large image through the tower alone: MLXLAB_LLM_DIR=<Qwen3.5 folder> MLXLAB_VLM_BIG=<image>.
func TestVisionLargeImage(t *testing.T) {
	dir, p := os.Getenv("MLXLAB_LLM_DIR"), os.Getenv("MLXLAB_VLM_BIG")
	if dir == "" || p == "" {
		t.Skip("MLXLAB_LLM_DIR / MLXLAB_VLM_BIG not set")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	px, err := e.Cfg.Vision.PrepareImage(data)
	if err != nil {
		t.Fatal(err)
	}
	stage := 0
	visionTrace = func(name string, a array) {
		mlx.Eval(a)
		t.Logf("%s %d: %v, active %.2f GB, peak %.2f GB", name, stage, a.Shape(), float64(mlx.ActiveMemory())/(1<<30), float64(mlx.PeakMemory())/(1<<30))
		stage++
	}
	defer func() { visionTrace = func(string, array) {} }()
	err = e.do(func() error { mlx.Eval(e.md.vision.encode(px, mlx.BFloat16)); return nil })
	if err != nil {
		t.Fatal(err)
	}
}

// The prompt cache with images: MLXLAB_LLM_DIR=<Qwen3.5 folder> MLXLAB_VLM_CACHE=<image with "21">,<the same size with
// "47">. The second image has the same tokens as the first: the cache must not answer for the first; a tool round of
// the second's conversation continues from the cache.
func TestVisionPromptCache(t *testing.T) {
	dir, list := os.Getenv("MLXLAB_LLM_DIR"), os.Getenv("MLXLAB_VLM_CACHE")
	if dir == "" || list == "" {
		t.Skip("MLXLAB_LLM_DIR / MLXLAB_VLM_CACHE not set")
	}
	paths := strings.Split(list, ",")
	e, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ask := func(msgs []Message) (string, Stats) {
		var out strings.Builder
		st, err := e.GenerateImages(context.Background(), Render(e.Format, msgs, nil, false), PromptImages(msgs),
			Options{Temperature: 0, MaxTokens: 16}, func(s string) { out.WriteString(s) })
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String()), st
	}
	q := func(p string) Message {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return Message{Role: "user", Content: "[image-1] Which number is on it? Only the number.", Images: []Image{{ID: "image-1", MIME: "image/png", Data: data}}}
	}
	first, st1 := ask([]Message{q(paths[0])})
	second, st2 := ask([]Message{q(paths[1])})
	t.Logf("first %q (%d cached), second %q (%d cached, %d images run)", first, st1.CachedTokens, second, st2.CachedTokens, st2.Images)
	if !strings.Contains(first, "21") || !strings.Contains(second, "47") || st2.Images != 1 {
		t.Fatalf("answers %q, %q; the second ran %d images", first, second, st2.Images)
	}
	// a tool round of the same turn (Qwen3.5's template drops the thought of earlier turns, so a new question starts
	// a new prompt, with or without images)
	msgs := []Message{q(paths[1]), {Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "add_one", Arguments: `{"n": ` + second + `}`}}},
		{Role: "tool", ToolCallID: "c1", Content: "48"}}
	third, st3 := ask(msgs)
	t.Logf("follow-up %q: %d of %d prompt tokens cached, %d images run", third, st3.CachedTokens, st3.PromptTokens, st3.Images)
	if st3.CachedTokens == 0 || st3.Images != 0 {
		t.Fatalf("the follow-up did not continue from the cache: %d cached, %d images run", st3.CachedTokens, st3.Images)
	}
}
