package engine

// Z-Image Turbo (Tongyi-MAI, Apache 2.0): a 6B single-stream DiT that draws in 9 steps without CFG, run by mflux
// (models/z_image, since mflux 0.13). The catalogue takes mflux's own 4-bit export (filipstrand/Z-Image-Turbo-mflux-4bit,
// 5.5 GB): the original repository holds the transformer in fp32 (25 of its 33 GB) — more than a 16 GB Mac can fetch
// and quantise when loading. It is the image generator for the Macs FLUX.2 [klein] 4B (16 GB) does not fit.
//
// Licence: Tongyi-MAI/Z-Image-Turbo is Apache 2.0. The pack's card names the "Tongyi Qianwen License" and links the
// original's LICENSE, which does not exist; the original's licence is taken.
//
// The model itself is catalog/z-image-turbo.json.

// zImageID is Z-Image Turbo's id (catalog/z-image-turbo.json).
const zImageID = "z-image-turbo"

// RecommendedImage is the image generator to propose for a machine with mem bytes of memory: FLUX.2 [klein] 4B where
// it fits (more than 16 GB, or unknown), else Z-Image Turbo.
func RecommendedImage(mem int64) string {
	if mem <= 0 || mem > 16*gb {
		return "flux2-klein-4b"
	}
	return zImageID
}
