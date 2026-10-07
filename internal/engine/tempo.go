package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/philipparndt/ai-connector/internal/pyenv"
)

// Tempo lock and best of N for songs (docs/music.md, "Sticking to the tempo").
//
// A song model is asked for a tempo (Request.BPM, or "NNN bpm" in the style) but does not always keep it: ACE-Step's
// planner picked 130 for a 138 bpm trance style before the tempo was passed explicitly, and any model can land a few
// percent off. After a new song is made, mlxlab measures its tempo near the one asked for (workers/tempo.py: the
// strongest pulse within ±12 % of the target, or of half / double the target, from the onset envelope's
// autocorrelation, with librosa) and
//
//   - best of N (Request.Takes 2–3): makes that many takes (seeds seed, seed+1, …) and keeps the one closest to the
//     tempo; the other takes are deleted (their tempos stay in the stats);
//   - tempo lock (Request.LockTempo, on by default when there is a tempo): a song off by at most 12 % is
//     time-stretched to the exact tempo with its pitch kept (ffmpeg's rubberband filter when the installed ffmpeg has
//     it, else atempo); the song as the model made it is kept as the job's second file. A song further off (or with
//     no clear pulse) is left as it is and the stats say "tempo missed".

const (
	tempoWindow  = 0.12  // the most a song is stretched (and how far from the target a pulse is searched)
	tempoExactly = 0.006 // closer than this (0.8 bpm at 138, about the measurement's noise) is not stretched
	maxTakes     = 3
)

// tempoEnv is the tempo helper's runtime: librosa, pinned.
var tempoEnv = pyenv.Env{Name: "tempo", Python: "3.12", Packages: []string{
	"librosa==1.0.0", "soundfile==0.14.0", "numpy==2.5.3", "numba==0.68.0", "scipy==1.18.1",
}}

type tempoSettings struct {
	measure, lock bool
	takes         int
}

// tempoPlan is what happens after a song is made: only new songs with a tempo are measured.
func tempoPlan(r Request, task string, bpm int) tempoSettings {
	if bpm <= 0 || task != "generate" {
		return tempoSettings{takes: 1}
	}
	s := tempoSettings{lock: r.LockTempo == nil || *r.LockTempo, takes: min(max(r.Takes, 1), maxTakes)}
	s.measure = s.lock || s.takes > 1
	return s
}

type takeResult struct {
	file string
	seed int64
	ev   workerEvent
	t    tempoReading
}

// tempoReading is one song's tempo as workers/tempo.py measured it.
type tempoReading struct {
	File     string  `json:"file"`
	Tracked  float64 `json:"tracked"`  // librosa's beat tracker
	Measured float64 `json:"measured"` // the pulse near the target (0: none)
	Salience float64 `json:"salience"`
	OK       bool    `json:"ok"` // a clear pulse near the target
}

// off is how far a reading is from bpm (relative); no clear pulse counts as infinitely far.
func (t tempoReading) off(bpm int) float64 {
	if !t.OK || t.Measured <= 0 {
		return math.Inf(1)
	}
	return math.Abs(t.Measured/float64(bpm) - 1)
}

// got is the tempo to report for a reading.
func (t tempoReading) got() float64 {
	if t.OK {
		return round1(t.Measured)
	}
	return round1(t.Tracked)
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }

// settleTempo measures the takes, keeps the closest one (as out) and locks its tempo; it returns the kept song's
// event and the tempo stats (with "original": the unstretched file, when there is one).
func (b *MusicBackend) settleTempo(ctx context.Context, takes []takeResult, bpm int, lock bool, out string, emit Emit) (workerEvent, map[string]any, error) {
	emit.Status("measuring the tempo …")
	files := make([]string, len(takes))
	var seconds float64
	for i, t := range takes {
		files[i] = t.file
		seconds += t.ev.Seconds
	}
	readings, err := measureTempo(ctx, b.Root, bpm, files, emit)
	if err != nil {
		// the song is there: a failed measurement must not lose it
		emit.Log("tempo: " + err.Error())
		return takes[0].ev, map[string]any{"tempoError": err.Error()}, nil
	}
	best := 0
	for i := range takes {
		takes[i].t = readings[i]
		if readings[i].off(bpm) < readings[best].off(bpm) {
			best = i
		}
	}
	keep := takes[best]
	stats := map[string]any{"bpmMeasured": keep.t.got(), "bpmFinal": keep.t.got(), "stretched": false}
	if len(takes) > 1 {
		var got []float64
		for i, t := range takes {
			got = append(got, t.t.got())
			if i != best {
				_ = os.Remove(t.file)
			}
		}
		if err := os.Rename(keep.file, out); err != nil {
			return keep.ev, nil, err
		}
		keep.ev.File = out
		stats["takes"], stats["takeBPMs"], stats["seed"] = len(takes), got, keep.seed
		emit.Log(fmt.Sprintf("best of %d: takes at %v bpm, kept seed %d", len(takes), got, keep.seed))
	}
	ev := keep.ev
	ev.Seconds = seconds
	off := keep.t.off(bpm)
	switch {
	case off > tempoWindow:
		msg := fmt.Sprintf("tempo missed: got %g bpm", keep.t.got())
		if !keep.t.OK {
			msg += " (no clear pulse near the tempo asked for)"
		}
		stats["tempoMissed"] = msg
		emit.Log(msg + fmt.Sprintf(", asked for %d", bpm))
	case !lock || off < tempoExactly:
	default:
		factor := float64(bpm) / keep.t.Measured
		orig := strings.TrimSuffix(ev.File, ".wav") + "-original.wav"
		if err := os.Rename(ev.File, orig); err != nil {
			return ev, nil, err
		}
		emit.Status(fmt.Sprintf("locking the tempo: %.1f → %d bpm …", keep.t.Measured, bpm))
		how, err := stretchTempo(ctx, orig, ev.File, factor)
		if err != nil {
			_ = os.Rename(orig, ev.File)
			emit.Log("tempo lock failed: " + err.Error())
			stats["tempoError"] = err.Error()
			return ev, stats, nil
		}
		stats["stretched"], stats["stretchFactor"], stats["stretchedWith"], stats["original"] = true, math.Round(factor*10000)/10000, how, orig
		if ev.Duration > 0 {
			ev.Duration /= factor
		}
		if r, err := measureTempo(ctx, b.Root, bpm, []string{ev.File}, emit); err == nil && r[0].OK {
			stats["bpmFinal"] = round1(r[0].Measured)
		} else {
			stats["bpmFinal"] = float64(bpm)
		}
		emit.Log(fmt.Sprintf("tempo lock: %.1f bpm stretched ×%.4f (%s) to %v bpm; the original is kept", keep.t.Measured, factor, how, stats["bpmFinal"]))
	}
	return ev, stats, nil
}

// measureTempo runs workers/tempo.py on the files (installing its runtime the first time).
func measureTempo(ctx context.Context, root string, bpm int, files []string, emit Emit) ([]tempoReading, error) {
	if err := tempoEnv.Ensure(ctx, root, emit.Log); err != nil {
		return nil, err
	}
	src, err := workerFiles.ReadFile("workers/tempo.py")
	if err != nil {
		return nil, err
	}
	script := filepath.Join(tempoEnv.Dir(root), "mlxlab_tempo.py")
	if err := os.WriteFile(script, src, 0o644); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, tempoEnv.Bin(root, "python"), append([]string{script, strconv.Itoa(bpm)}, files...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("measuring the tempo: %w: %s", err, lastLine(stderr.String()))
	}
	var rs []tempoReading
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		var r tempoReading
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.File != "" {
			rs = append(rs, r)
		}
	}
	if len(rs) != len(files) {
		return nil, fmt.Errorf("measuring the tempo: %d results for %d songs", len(rs), len(files))
	}
	return rs, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

var (
	rubberbandOnce sync.Once
	hasRubberband  bool
)

// stretchTempo writes in sped up by factor (> 1: faster) to out, pitch kept; it says which filter it used.
func stretchTempo(ctx context.Context, in, out string, factor float64) (string, error) {
	rubberbandOnce.Do(func() {
		b, err := exec.Command("ffmpeg", "-hide_banner", "-filters").Output()
		hasRubberband = err == nil && bytes.Contains(b, []byte(" rubberband "))
	})
	filter, how := fmt.Sprintf("atempo=%.6f", factor), "atempo"
	if hasRubberband {
		filter, how = fmt.Sprintf("rubberband=tempo=%.6f:pitchq=quality", factor), "rubberband"
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", in, "-af", filter, "-c:a", "pcm_s16le", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg %s: %w: %s", how, err, lastLine(string(b)))
	}
	return how, nil
}
