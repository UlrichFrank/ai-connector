package llm

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

// Image is an image attached to a message (a screenshot pasted into vboard's chat, an image_url part of the OpenAI
// API). The message's text names it by a tag "[<id>]" (e.g. "[image-1]") where it belongs; an image whose tag the
// text does not have belongs at its end. In JSON the bytes are base64 ("data"); a job's history keeps them in a file
// of the outputs instead ("file", no "data": internal/jobs).
type Image struct {
	ID   string `json:"id"`
	MIME string `json:"mime"`
	Data []byte `json:"data,omitempty"`
	File string `json:"file,omitempty"`
}

// Limits of the images of one request.
const (
	MaxImageBytes = 5 << 20 // one image
	MaxImages     = 8       // a request's
)

// imageExt are the image types a message may carry, with the file extension each is kept under.
var imageExt = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}

// ImageExt is the file extension of an image type ("" when it is not one a message may carry).
func ImageExt(mime string) string { return imageExt[strings.ToLower(strings.TrimSpace(mime))] }

// DataURL is the image as a data: URL (OpenAI's image_url).
func (im Image) DataURL() string {
	return "data:" + im.MIME + ";base64," + base64.StdEncoding.EncodeToString(im.Data)
}

// ParseDataURL reads a data:<mime>;base64,<data> URL of an image type a message may carry.
func ParseDataURL(u string) (mime string, data []byte, err error) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return "", nil, fmt.Errorf("not a data: URL")
	}
	head, b64, ok := strings.Cut(rest, ",")
	if !ok {
		return "", nil, fmt.Errorf("a data: URL without data")
	}
	mime, enc, _ := strings.Cut(head, ";")
	mime = strings.ToLower(strings.TrimSpace(mime))
	if strings.ToLower(strings.TrimSpace(enc)) != "base64" {
		return "", nil, fmt.Errorf("a data: URL that is not base64")
	}
	if ImageExt(mime) == "" {
		return "", nil, fmt.Errorf("image type %q is not taken (png, jpeg, webp, gif)", mime)
	}
	data, err = base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", nil, fmt.Errorf("the image's base64 does not decode: %v", err)
	}
	return mime, data, nil
}

// CheckImage says whether an image may go with a message: an id, a type a message may carry, data, at most
// MaxImageBytes.
func CheckImage(im Image) error {
	switch {
	case !ValidImageID(im.ID):
		return fmt.Errorf("image id %q: letters, digits, - and _ (e.g. image-1)", im.ID)
	case ImageExt(im.MIME) == "":
		return fmt.Errorf("%s: image type %q is not taken (png, jpeg, webp, gif)", im.ID, im.MIME)
	case len(im.Data) == 0 && im.File == "":
		return fmt.Errorf("%s: no image data", im.ID)
	case len(im.Data) > MaxImageBytes:
		return fmt.Errorf("%s: the image is larger than %d MB", im.ID, MaxImageBytes>>20)
	}
	return nil
}

var imageID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// ValidImageID: an image id is 1–40 letters, digits, - and _ (it goes into file names and tags).
func ValidImageID(id string) bool { return imageID.MatchString(id) }

// Tag is how a message's text names an image: "[<id>]".
func Tag(id string) string { return "[" + id + "]" }

// Part is a piece of a message for a model that takes images: text, or an image (Image set).
type Part struct {
	Text  string
	Image *Image
}

// Parts splits a message's text at its images' tags: the text up to and with the tag, then the image, then the rest
// (the tag stays in the text, so the model can refer to the image by it). An image whose tag is not in the text
// follows the text, after its tag. Empty text parts are left out.
func Parts(text string, images []Image) []Part {
	var out []Part
	addText := func(s string) {
		if s != "" {
			out = append(out, Part{Text: s})
		}
	}
	rest := text
	placed := make([]bool, len(images))
	for {
		// the earliest tag of an image not yet placed
		at, which := -1, -1
		for i, im := range images {
			if placed[i] {
				continue
			}
			if j := strings.Index(rest, Tag(im.ID)); j >= 0 && (at < 0 || j < at) {
				at, which = j, i
			}
		}
		if which < 0 {
			break
		}
		end := at + len(Tag(images[which].ID))
		addText(rest[:end])
		im := images[which]
		out = append(out, Part{Image: &im})
		placed[which] = true
		rest = rest[end:]
	}
	addText(rest)
	for i := range images {
		if !placed[i] {
			if len(out) > 0 && out[len(out)-1].Image == nil {
				out[len(out)-1].Text += " "
			}
			addText(Tag(images[i].ID))
			im := images[i]
			out = append(out, Part{Image: &im})
		}
	}
	return out
}

// HasImages: a message of msgs carries an image.
func HasImages(msgs []Message) bool {
	for _, m := range msgs {
		if len(m.Images) > 0 {
			return true
		}
	}
	return false
}

// OmitImages is the text with the images' tags replaced by "(image omitted)" (for a model without the capability).
func OmitImages(text string, images []Image) string {
	for _, im := range images {
		text = strings.ReplaceAll(text, Tag(im.ID), "(image omitted)")
	}
	return text
}
