package hub

import (
	"errors"
	"fmt"
)

// Cutouts on vboard's board (docs/api.md, "Removing the background"): a generator of kind "cutout" (BiRefNet) takes
// one image and makes its background transparent — the same size, an RGBA PNG (a WebP when the PNG would be larger
// than vboard takes; never a JPEG, which has no alpha). No prompt, no seed, no steps.

// planCutout checks a cutout request: exactly one image, at most genCutoutMax pixels on its longer side. The result
// has the image's size.
func planCutout(g advertisedGenerator, q *genRequest) (genPlan, error) {
	var p genPlan
	if len(q.Images) != 1 {
		return p, fmt.Errorf("%s cuts out one image: send exactly one (not %d)", g.Name, len(q.Images))
	}
	b, ext, err := genImage(q.Images[0])
	if err != nil {
		return p, err
	}
	w, h, err := imageSize(b)
	if err != nil {
		return p, fmt.Errorf("the image: %v", err)
	}
	if w <= 0 || h <= 0 {
		return p, errors.New("the image has no size")
	}
	if max(w, h) > g.Max {
		return p, fmt.Errorf("the image (%d×%d) is larger than %s cuts out: at most %d pixels on the longer side", w, h, g.Name, g.Max)
	}
	p.width, p.height = w, h
	p.images, p.exts = [][]byte{b}, []string{ext}
	return p, nil
}
