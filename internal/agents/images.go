package agents

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Images in a turn (docs/api.md "Agents (Claude Code)", Images). The turn's text names each image by its tag
// ("[image-1]"); the json driver sends the user message as content blocks — the text and the images interleaved where
// the tags are (the tags stay in the text, so Claude can refer to them; checked with Claude Code 2.1.288's
// stream-json input) — and the tty driver, which can only type, writes the images into the session's folder
// (attachments/image-1.png …, where the session may Read) and names the file at the tag.

// Image is an image of a turn.
type Image struct {
	ID   string // its tag in the text is "[<ID>]"
	MIME string // image/png, image/jpeg, image/webp, image/gif
	Data []byte
}

func (im Image) tag() string { return "[" + im.ID + "]" }

// turnImages are the images of what a turn sends: the Opening's when the text is the Opening, else the turn's.
func turnImages(o Options, text string) []Image {
	if o.Opening != "" && text == o.Opening {
		return o.OpeningImages
	}
	return o.Images
}

// imageTurner is a driver that takes images with a turn.
type imageTurner interface {
	turnImages(ctx context.Context, text string, images []Image, emit func(Event)) (Result, error)
}

// turnWith runs a turn, with its images when there are any.
func turnWith(ctx context.Context, drv driver, text string, images []Image, emit func(Event)) (Result, error) {
	if it, ok := drv.(imageTurner); ok && len(images) > 0 {
		return it.turnImages(ctx, text, images, emit)
	}
	return drv.turn(ctx, text, emit)
}

// contentBlocks is a user message as Claude's content blocks: text up to and with an image's tag, the image, the
// rest; an image whose tag the text lacks follows at the end, after its tag.
func contentBlocks(text string, images []Image) []map[string]any {
	var out []map[string]any
	addText := func(s string) {
		if s != "" {
			out = append(out, map[string]any{"type": "text", "text": s})
		}
	}
	addImage := func(im Image) {
		out = append(out, map[string]any{"type": "image", "source": map[string]any{"type": "base64",
			"media_type": im.MIME, "data": base64.StdEncoding.EncodeToString(im.Data)}})
	}
	rest := text
	placed := make([]bool, len(images))
	for {
		at, which := -1, -1
		for i, im := range images {
			if j := strings.Index(rest, im.tag()); !placed[i] && j >= 0 && (at < 0 || j < at) {
				at, which = j, i
			}
		}
		if which < 0 {
			break
		}
		end := at + len(images[which].tag())
		addText(rest[:end])
		addImage(images[which])
		placed[which] = true
		rest = rest[end:]
	}
	addText(rest)
	for i, im := range images {
		if !placed[i] {
			addText(im.tag())
			addImage(im)
		}
	}
	return out
}

func (d *jsonDriver) turnImages(ctx context.Context, text string, images []Image, emit func(Event)) (Result, error) {
	return d.turnContent(ctx, contentBlocks(text, images), emit)
}

// imageExts are the file extensions of the image types.
var imageExts = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}

// attachmentsDir is where a tty session's images go, inside its folder.
const attachmentsDir = "attachments"

// attach writes the images into <dir>/attachments (image-1.png; a name that is taken gets -2, -3 …: an earlier
// turn's image keeps its file) and returns the text with each tag followed by the file's path relative to dir
// ("[image-1: attachments/image-1.png]"; an image whose tag the text lacks is named at its end).
func attach(dir, text string, images []Image) (string, error) {
	at := filepath.Join(dir, attachmentsDir)
	if err := os.MkdirAll(at, 0o755); err != nil {
		return "", err
	}
	for _, im := range images {
		ext := imageExts[strings.ToLower(im.MIME)]
		if ext == "" {
			return "", fmt.Errorf("%s: image type %q is not taken", im.ID, im.MIME)
		}
		if strings.ContainsAny(im.ID, `/\.`) || im.ID == "" {
			return "", fmt.Errorf("image id %q is not a file name", im.ID)
		}
		name := im.ID + ext
		for n := 2; ; n++ {
			if _, err := os.Lstat(filepath.Join(at, name)); os.IsNotExist(err) {
				break
			}
			name = fmt.Sprintf("%s-%d%s", im.ID, n, ext)
		}
		if err := os.WriteFile(filepath.Join(at, name), im.Data, 0o644); err != nil {
			return "", err
		}
		ref := "[" + im.ID + ": " + attachmentsDir + "/" + name + "]"
		if strings.Contains(text, im.tag()) {
			text = strings.ReplaceAll(text, im.tag(), ref)
		} else {
			text += " " + ref
		}
	}
	return text, nil
}

func (d *ttyDriver) turnImages(ctx context.Context, text string, images []Image, emit func(Event)) (Result, error) {
	text, err := attach(d.s.cwd, text, images)
	if err != nil {
		return Result{}, err
	}
	return d.turn(ctx, text, emit)
}
