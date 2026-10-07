package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Clip is one shot of a movie script.
type Clip struct {
	Prompt string `json:"prompt"`
	Frames int    `json:"frames,omitempty"` // 0: the movie's frame count (or LTX-2.5's own guess)
	// Continue starts the clip from the last frame of the one before (a cut otherwise: a new scene).
	Continue bool `json:"continue,omitempty"`
}

// movie renders a script clip by clip and joins the clips into outDir/name.mp4. The movie's prompt is the shared
// description (characters, look) and goes in front of every clip's own.
//
// Clips are kept in outDir/movies/<model, size, fps, seed>/ with a fingerprint of everything that makes them (and,
// for a continuation, the clip before): a retry — after a crash, a quit, or an edit of the script — reuses every
// clip whose fingerprint still matches and renders only the others.
func (b *LTXBackend) movie(ctx context.Context, p ltxRun, model string, r Request, outDir, name string, emit Emit) (string, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", errors.New("movies need ffmpeg (brew install ffmpeg)")
	}
	base := filepath.Join(outDir, "movies", movieKey(model, r))
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	prev := ""                                                         // the fingerprint of the clip before
	emit(Event{Type: "stats", Data: map[string]any{"clipsDir": base}}) // deleting the movie cleans its clips up
	n := len(r.Script)
	clips := make([]string, n)
	for i, c := range r.Script {
		if strings.TrimSpace(c.Prompt) == "" {
			return "", fmt.Errorf("clip %d has no description", i+1)
		}
		prompt := c.Prompt
		if s := strings.TrimSpace(r.Prompt); s != "" {
			prompt = strings.TrimRight(s, ". ") + ". " + c.Prompt
		}
		frames := frames8k1(orInt(c.Frames, r.Frames))
		var images []string
		if i == 0 {
			images = r.Images // a first frame for the whole movie
		}
		clips[i] = filepath.Join(base, fmt.Sprintf("clip-%02d.mp4", i+1))
		how := "new scene"
		if i > 0 && c.Continue {
			how = "continues"
		}
		cont := ""
		if i > 0 && c.Continue {
			cont = prev
		}
		key := fingerprint(prompt, frames, r.Seed+int64(i), cont, strings.Join(images, "|"))
		if i > 0 && c.Continue {
			images = nil // a continuation's first frame is the last of the clip before (made below when needed)
		}
		prev = key
		keyFile := clips[i] + ".key"
		if k, err := os.ReadFile(keyFile); err == nil && string(k) == key && fileExists(clips[i]) {
			emit.Log(fmt.Sprintf("clip %d/%d (%s): already rendered, reused", i+1, n, how))
		} else {
			if i > 0 && c.Continue {
				last := filepath.Join(base, fmt.Sprintf("clip-%02d-last.png", i))
				if err := lastFrame(ctx, ffmpeg, clips[i-1], last); err != nil {
					return "", fmt.Errorf("clip %d: the last frame of clip %d: %w", i+1, i, err)
				}
				images = []string{last}
			}
			emit.Log(fmt.Sprintf("clip %d/%d (%s): %s", i+1, n, how, c.Prompt))
			clipEmit := Emit(func(ev Event) {
				if ev.Type == "status" {
					ev.Text = fmt.Sprintf("clip %d/%d · %s", i+1, n, ev.Text)
				}
				emit(ev)
			})
			_ = os.Remove(keyFile)
			if err := b.render(ctx, p, r, prompt, frames, images, r.Seed+int64(i), clips[i], clipEmit); err != nil {
				return "", fmt.Errorf("clip %d: %w", i+1, err)
			}
			if err := os.WriteFile(keyFile, []byte(key), 0o644); err != nil {
				return "", err
			}
		}
		emit(Event{Type: "stats", Data: map[string]any{"clipsDone": i + 1, "clips": n}})
		// the film so far, to watch while the rest renders (a new name each time: players cache by URL)
		if i+1 < n {
			preview := filepath.Join(base, fmt.Sprintf("preview-%02d.mp4", i+1))
			if err := join(ctx, ffmpeg, clips[:i+1], r.Script[:i+1], r.FPS, !r.NoAudio, preview); err == nil {
				emit(Event{Type: "preview", File: preview, Step: i + 1, Total: n})
			} else {
				emit.Log("preview: " + err.Error())
			}
		}
	}
	emit.Status(fmt.Sprintf("joining %d clips …", n))
	emit.Progress(0, 0)
	out := filepath.Join(outDir, name+".mp4")
	if err := join(ctx, ffmpeg, clips, r.Script, r.FPS, !r.NoAudio, out); err != nil {
		return "", err
	}
	return out, nil
}

// movieKey names a movie's folder: what makes all of its clips (the HQ models' steps and CFG too; the distilled ones
// have none, so their folders stay as they were).
func movieKey(model string, r Request) string {
	parts := []any{model, r.Width, r.Height, r.FPS, r.Seed, r.NoAudio}
	if r.Steps != 0 || r.Guidance != 0 {
		parts = append(parts, r.Steps, r.Guidance)
	}
	return fingerprint(parts...)[:16]
}

// fingerprint is a hash of the things that make a clip (or a movie's folder).
func fingerprint(parts ...any) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%v\x00", p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// lastFrame writes a video's last frame as a PNG.
func lastFrame(ctx context.Context, ffmpeg, video, png string) error {
	cmd := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-y", "-sseof", "-1", "-i", video, "-update", "1", "-q:v", "1", png)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	if _, err := os.Stat(png); err != nil {
		return errors.New("ffmpeg wrote no frame")
	}
	return nil
}

// join puts the clips one after the other. A clip that continues the one before starts with that one's last frame,
// so its first frame (and as much sound) is dropped: no stutter at the seam.
func join(ctx context.Context, ffmpeg string, clips []string, script []Clip, fps int, audio bool, out string) error {
	var args, parts []string
	var filter strings.Builder
	for i, c := range clips {
		args = append(args, "-i", c)
		v, a := fmt.Sprintf("[%d:v]", i), fmt.Sprintf("[%d:a]", i)
		if i > 0 && script[i].Continue {
			fmt.Fprintf(&filter, "[%d:v]trim=start_frame=1,setpts=PTS-STARTPTS[v%d];", i, i)
			v = fmt.Sprintf("[v%d]", i)
			if audio {
				fmt.Fprintf(&filter, "[%d:a]atrim=start=%s,asetpts=PTS-STARTPTS[a%d];", i, strconv.FormatFloat(1/float64(max(fps, 1)), 'f', 4, 64), i)
				a = fmt.Sprintf("[a%d]", i)
			}
		}
		parts = append(parts, v)
		if audio {
			parts = append(parts, a)
		}
	}
	a := 0
	if audio {
		a = 1
	}
	fmt.Fprintf(&filter, "%sconcat=n=%d:v=1:a=%d[v]", strings.Join(parts, ""), len(clips), a)
	if audio {
		filter.WriteString("[a]")
	}
	args = append(args, "-filter_complex", filter.String(), "-map", "[v]")
	if audio {
		args = append(args, "-map", "[a]", "-c:a", "aac", "-b:a", "192k")
	}
	args = append(args, "-c:v", "libx264", "-crf", "18", "-preset", "medium", "-pix_fmt", "yuv420p", "-movflags", "+faststart", "-y", out)
	cmd := exec.CommandContext(ctx, ffmpeg, append([]string{"-v", "error"}, args...)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("joining the clips: %w: %s", err, strings.TrimSpace(string(b)))
	}
	return nil
}
