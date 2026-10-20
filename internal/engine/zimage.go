package engine

import (
	"path/filepath"

	"github.com/philipparndt/ai-connector/internal/models"
)

// Z-Image Turbo (Tongyi-MAI, Apache 2.0): a 6B single-stream DiT that draws in 9 steps without CFG, run by mflux
// (models/z_image, since mflux 0.13). The catalogue takes mflux's own 4-bit export (filipstrand/Z-Image-Turbo-mflux-4bit,
// 5.5 GB): the original repository holds the transformer in fp32 (25 of its 33 GB) — more than a 16 GB Mac can fetch
// and quantise when loading. It is the image generator for the Macs FLUX.2 [klein] 4B (16 GB) does not fit.
//
// Licence: Tongyi-MAI/Z-Image-Turbo is Apache 2.0. The pack's card names the "Tongyi Qianwen License" and links the
// original's LICENSE, which does not exist; the original's licence is taken.

const (
	zImageID   = "z-image-turbo"
	zImageRepo = "filipstrand/Z-Image-Turbo-mflux-4bit"
	zImageRev  = "b3a8f31115a11f2f9e2fa0bfbc8d78dcc3e6568b"
)

// zImageFiles are the files of the pack mflux loads (not the example pictures beside them).
var zImageFiles = []string{"transformer/", "text_encoder/", "vae/", "tokenizer/"}

// zImageTurbo is the catalogue's Z-Image Turbo: pre-quantised (Quantize -1: mflux loads the 4-bit weights as stored).
func zImageTurbo(store *models.Store, hub string) *Model {
	m := &Model{ID: zImageID, Name: "Z-Image Turbo", Kind: Image, Backend: "mflux",
		About: "Z-Image Turbo, 6B, Apache 2.0: text to image in 9 steps, good at text in pictures; mflux's 4-bit pack " +
			"(5.5 GB), for Macs with 16 GB. No editing with reference images",
		Size: 55 * gb / 10, repo: zImageRepo, rev: zImageRev, mflux: zImageID, files: zImageFiles,
		Tasks: []string{"generate"}, Licence: "Apache-2.0",
		Defaults: Request{Width: 1024, Height: 1024, Steps: 9, Quantize: -1}}
	if d := filepath.Join(store.Dir, filepath.FromSlash(zImageRepo), zImageRev); hasAll(d, "transformer/*.safetensors.sha256", "vae/*.safetensors.sha256", "text_encoder/*.safetensors.sha256") {
		m.Here, m.Where, m.Size = true, d, dirSize(d)
	} else if d := hfSnapshot(hub, zImageRepo, zImageRev); d != "" && hasAll(d, "transformer/*.safetensors", "vae/*.safetensors", "text_encoder/*.safetensors") {
		m.Here, m.Where = true, "the Hugging Face cache (linked on first use)"
	}
	return m
}

// RecommendedImage is the image generator to propose for a machine with mem bytes of memory: FLUX.2 [klein] 4B where
// it fits (more than 16 GB, or unknown), else Z-Image Turbo.
func RecommendedImage(mem int64) string {
	if mem <= 0 || mem > 16*gb {
		return "flux2-klein-4b"
	}
	return zImageID
}
