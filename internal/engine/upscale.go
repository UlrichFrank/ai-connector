package engine

import (
	"context"
	"fmt"
	"image/png"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
)

// SeedVR2 (ByteDance, Apache-2.0): diffusion super-resolution in one step, run by mflux like FLUX. One image in, no
// prompt; the result is Scale times as large. Its weights are single files of numz/SeedVR2_comfyUI (the 3B and the
// 7B share the VAE and the folder).

// TaskUpscale: an image model that only makes a picture larger (SeedVR2).
const TaskUpscale = "upscale"

// UpscaleMax is the longest side an upscale makes (a larger scale is cut to it, the aspect ratio kept).
const UpscaleMax = 4096

// upscale runs SeedVR2 on one image. mflux takes the shorter side's new length; the worker script reads the image's
// size and works it out as UpscaleSize does. mflux tiles the VAE (encode and decode in 512 px tiles) by default, so
// a 4096² result does not need the memory of a 4096² decode.
func (b *FluxBackend) upscale(ctx context.Context, m *Model, r Request, outDir string, emit Emit) error {
	call, err := upscaleCall(m, r)
	if err != nil {
		return err
	}
	hm, err := hubModel(ctx, b.Store, m, func(p string) bool { return models.Matches(p, m.files...) })
	if err != nil {
		return err
	}
	dir, err := fetch(ctx, b.Store, hm, emit)
	if err != nil {
		return err
	}
	m.Here, m.Where = true, dir
	if b.w == nil {
		b.w = &pyWorker{env: MfluxEnv, script: "flux_worker.py"}
	}
	if err := b.w.start(ctx, b.Root, emit); err != nil {
		return err
	}
	call["path"] = dir
	call["output"] = filepath.Join(outDir, fmt.Sprintf("%s-%d-x%d.png", time.Now().Format("20060102-150405"), call["seed"], call["scale"]))
	emit(Event{Type: "stats", Data: map[string]any{"seed": call["seed"], "scale": call["scale"], "quantize": call["quantize"]}})
	b.state.set(m.ID) // loading counts as loaded: it takes the memory
	ev, err := b.w.call(ctx, call, emit)
	if err != nil {
		if !b.w.running() {
			b.state.set("") // canceled or crashed: the worker and its model are gone
		}
		return err
	}
	b.state.set(m.ID)
	st := map[string]any{"seconds": ev.Seconds, "peakGB": ev.PeakGB}
	w, h := pngSize(ev.File)
	if w > 0 {
		st["width"], st["height"] = w, h // what it made
	}
	emit(Event{Type: "file", File: ev.File})
	emit(Event{Type: "stats", Data: st})
	emit.Log(fmt.Sprintf("upscaled ×%d to %dx%d in %.1f s, peak %.1f GB", call["scale"], w, h, ev.Seconds, ev.PeakGB))
	return nil
}

// upscaleCall is the worker's request for an upscale, checked and with the defaults (without the weights' folder
// and the output file).
func upscaleCall(m *Model, r Request) (map[string]any, error) {
	switch {
	case len(r.Images) != 1 || r.Images[0] == "":
		return nil, fmt.Errorf("%s upscales one image: give exactly one", m.Name)
	case r.Scale < 0 || r.Scale == 1 || r.Scale > 4:
		return nil, fmt.Errorf("%s: scale %d (2 to 4; 0 is 2)", m.Name, r.Scale)
	}
	q := orInt(r.Quantize, m.Defaults.Quantize)
	if q < 0 {
		q = 0
	}
	seed := r.Seed
	if seed == 0 {
		seed = rand.Int64N(1_000_000_000)
	}
	return map[string]any{"model": m.mflux, "upscale": true, "image": r.Images[0], "scale": orInt(r.Scale, 2),
		"maxSide": UpscaleMax, "seed": seed, "quantize": q}, nil
}

// UpscaleSize is what an upscale of a w×h image makes, worked out as mflux does: the shorter side scale times as
// long (less when the longer side would pass maxSide), the other in proportion, both rounded down to even. The
// worker script picks the same shorter side (upscale_resolution).
func UpscaleSize(w, h, scale, maxSide int) (int, int) {
	short, long := min(w, h), max(w, h)
	if short <= 0 || scale <= 0 {
		return 0, 0
	}
	res := short * scale
	if long*scale > maxSide {
		res = short * maxSide / long
	}
	f := float64(res) / float64(short) // mflux: scale = target_res / min(w, h); int(w * scale) // 2 * 2
	return int(float64(w)*f) / 2 * 2, int(float64(h)*f) / 2 * 2
}

// pngSize reads a PNG's size from its header (0: not one).
func pngSize(p string) (int, int) {
	f, err := os.Open(p)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	c, err := png.DecodeConfig(f)
	if err != nil {
		return 0, 0
	}
	return c.Width, c.Height
}
