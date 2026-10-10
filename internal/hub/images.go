package hub

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

// Images in chats (docs/api.md): a user message may carry images, named in its text by tags ("[image-1]"). A chat
// job takes them as messages[].images [{id, mime, data (base64)}] (llm.Message.Images on the worker, which keeps them
// in files, not in its history). Only a model with the capability ("images" in the workers' model lists) gets them:
// /v1/chat/completions refuses images for another, and a connector replaces their tags with "(image omitted)".
// The hub does not import the worker's packages, so the checks are here once more (llm.CheckImage).

const (
	maxImageBytes = 5 << 20 // one image (decoded)
	maxImages     = 8       // a request's
)

// imageTypes are the image types a message may carry.
var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}

var imageIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// chatImage is an image of a message as a job takes it.
type chatImage struct {
	ID   string `json:"id"`
	MIME string `json:"mime"`
	Data string `json:"data"` // base64
}

// check: an id, a type a message may carry, base64 data of at most maxImageBytes.
func (im *chatImage) check() error {
	im.MIME = strings.ToLower(strings.TrimSpace(im.MIME))
	if im.MIME == "image/jpg" {
		im.MIME = "image/jpeg"
	}
	switch {
	case !imageIDPattern.MatchString(im.ID):
		return fmt.Errorf("image id %q: letters, digits, - and _ (e.g. image-1)", im.ID)
	case !imageTypes[im.MIME]:
		return fmt.Errorf("%s: image type %q is not taken (png, jpeg, webp, gif)", im.ID, im.MIME)
	case im.Data == "":
		return fmt.Errorf("%s: no image data", im.ID)
	case base64.StdEncoding.DecodedLen(len(im.Data)) > maxImageBytes+3:
		return fmt.Errorf("%s: the image is larger than %d MB", im.ID, maxImageBytes>>20)
	}
	b, err := base64.StdEncoding.DecodeString(im.Data)
	if err != nil {
		return fmt.Errorf("%s: the image's base64 does not decode", im.ID)
	}
	if len(b) > maxImageBytes {
		return fmt.Errorf("%s: the image is larger than %d MB", im.ID, maxImageBytes>>20)
	}
	return nil
}

// dataURLImage reads an image_url of the OpenAI API: a data: URL of an image type (http(s) URLs are not fetched).
func dataURLImage(u, id string) (chatImage, error) {
	u = strings.TrimSpace(u)
	lower := strings.ToLower(u)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return chatImage{}, fmt.Errorf("image URLs are not supported: send the image as a data: URL (data:image/png;base64,…)")
	}
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return chatImage{}, fmt.Errorf("an image_url is a data: URL (data:image/png;base64,…)")
	}
	head, data, ok := strings.Cut(rest, ",")
	mime, enc, _ := strings.Cut(head, ";")
	if !ok || !strings.EqualFold(strings.TrimSpace(enc), "base64") {
		return chatImage{}, fmt.Errorf("an image_url is a base64 data: URL (data:image/png;base64,…)")
	}
	im := chatImage{ID: id, MIME: mime, Data: strings.TrimSpace(data)}
	return im, im.check()
}

// omitImageTags replaces the tags of a message's images with "(image omitted)" (a model without the capability).
func omitImageTags(text string, images []chatImage) string {
	for _, im := range images {
		text = strings.ReplaceAll(text, "["+im.ID+"]", "(image omitted)")
	}
	return text
}

// jobHasImages: a message of the chat job carries images.
func jobHasImages(job map[string]any) bool {
	msgs, _ := job["messages"].([]map[string]any)
	for _, m := range msgs {
		if _, ok := m["images"]; ok {
			return true
		}
	}
	return false
}

// jobImages are images as a job's message carries them.
func jobImages(images []chatImage) []map[string]string {
	out := make([]map[string]string, len(images))
	for i, im := range images {
		out[i] = map[string]string{"id": im.ID, "mime": im.MIME, "data": im.Data}
	}
	return out
}
