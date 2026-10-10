package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/llm"
)

// Images of a chat's messages (llm.Message.Images): the history keeps the jobs small — a job's images are written to
// <outputs>/chat-images/<job id>/<image id>.<ext> when it is submitted, and its request keeps only references (id,
// mime, file); the bytes are read back for the run.

// ImagesDir is where the chats' images are kept.
func (m *Manager) ImagesDir() string { return filepath.Join(m.OutDir(), "chat-images") }

// checkImages refuses a request's images when its model cannot take them, and checks each (llm.CheckImage, at most
// llm.MaxImages, a reference only to a file kept here).
func (m *Manager) checkImages(md *engine.Model, r *engine.Request) error {
	if !llm.HasImages(r.Messages) {
		return nil
	}
	if md.Kind != engine.Chat || !md.Images {
		return fmt.Errorf("%s does not take images", md.Name)
	}
	n := 0
	for _, msg := range r.Messages {
		for _, im := range msg.Images {
			n++
			if err := llm.CheckImage(im); err != nil {
				return err
			}
			if len(im.Data) == 0 && !m.keptImage(im.File) {
				return fmt.Errorf("%s: unknown image file", im.ID)
			}
		}
	}
	if n > llm.MaxImages {
		return fmt.Errorf("%d images: at most %d a request", n, llm.MaxImages)
	}
	return nil
}

// keptImage: a file of the images directory.
func (m *Manager) keptImage(p string) bool {
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	rel, err := filepath.Rel(m.ImagesDir(), filepath.Clean(p))
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// storeImages writes the request's images (those with data) into the job's folder and leaves references in their
// place. The messages are copied: the caller's request is not changed.
func (m *Manager) storeImages(id string, r *engine.Request) error {
	if !llm.HasImages(r.Messages) {
		return nil
	}
	dir := filepath.Join(m.ImagesDir(), id)
	msgs := make([]llm.Message, len(r.Messages))
	for i, msg := range r.Messages {
		msgs[i] = msg
		if len(msg.Images) == 0 {
			continue
		}
		ims := make([]llm.Image, len(msg.Images))
		for k, im := range msg.Images {
			if len(im.Data) > 0 {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return err
				}
				name := fmt.Sprintf("%d-%s%s", i, im.ID, llm.ImageExt(im.MIME)) // ids repeat from message to message
				p := filepath.Join(dir, name)
				if err := os.WriteFile(p, im.Data, 0o644); err != nil {
					return fmt.Errorf("keeping %s: %w", im.ID, err)
				}
				im.File, im.Data = p, nil
			}
			ims[k] = im
		}
		msgs[i].Images = ims
	}
	r.Messages = msgs
	return nil
}

// withImageData is the request as a backend runs it: the images' bytes read back from their files.
func withImageData(r engine.Request) (engine.Request, error) {
	if !llm.HasImages(r.Messages) {
		return r, nil
	}
	msgs := make([]llm.Message, len(r.Messages))
	for i, msg := range r.Messages {
		msgs[i] = msg
		if len(msg.Images) == 0 {
			continue
		}
		ims := make([]llm.Image, len(msg.Images))
		for k, im := range msg.Images {
			if len(im.Data) == 0 && im.File != "" {
				b, err := os.ReadFile(im.File)
				if err != nil {
					return r, fmt.Errorf("image %s: %w", im.ID, err)
				}
				im.Data = b
			}
			ims[k] = im
		}
		msgs[i].Images = ims
	}
	r.Messages = msgs
	return r, nil
}

// withoutImageData is msgs with references only (a result's history: the bytes are in the files).
func withoutImageData(msgs []llm.Message) []llm.Message {
	if !llm.HasImages(msgs) {
		return msgs
	}
	out := make([]llm.Message, len(msgs))
	for i, msg := range msgs {
		out[i] = msg
		if len(msg.Images) == 0 {
			continue
		}
		ims := make([]llm.Image, len(msg.Images))
		for k, im := range msg.Images {
			im.Data = nil
			ims[k] = im
		}
		out[i].Images = ims
	}
	return out
}

// imageFiles are the files of a request's images.
func imageFiles(r engine.Request) []string {
	var out []string
	for _, msg := range r.Messages {
		for _, im := range msg.Images {
			if im.File != "" {
				out = append(out, im.File)
			}
		}
	}
	return out
}
