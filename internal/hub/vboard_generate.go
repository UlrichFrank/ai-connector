package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"mime/multipart"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
)

// Images and videos on vboard's board (docs/api.md, "vboard: generating images and videos on the board"). The
// connector offers the image and video models of its setting `generators` that a connected worker has on disk
// (hello.generators, {type:"generators"} when only they change); vboard puts a placeholder on the board and sends
// {generate}; the connector runs an image or video job on a worker that has the model, says how it goes
// ({generateStatus}), sends the file in pieces ({generateFile}) and ends with {generateDone}. {generateCancel}
// cancels the job. The generation is a request of its own, not part of a chat turn: it runs as long as it takes.
// Upscales (SeedVR2) and cutouts (BiRefNet: the background made transparent) are generators of their own kinds:
// vboard_upscale.go, vboard_cutout.go; music, sound and speech: vboard_audio.go.

// Limits (docs/api.md).
const (
	maxGenerators     = 20
	genPromptChars    = 4000
	genImageBytes     = 8 << 20   // a reference image (decoded)
	genMaxImages      = 4         // reference images a request
	genPieceBytes     = 1 << 20   // the file's bytes in one {generateFile}
	genMaxVideoFile   = 200 << 20 // and videos of at most 500 MB; the connector sends at most 200
	genFailedPolls    = 3         // the worker did not answer this often in a row: the job is lost
	genMaxFPS         = 60
	genImageMultiple  = 16 // FLUX and Qwen-Image (mflux): sides are rounded down to it (engine/flux.go)
	genImageMax       = 2048
	genVideoMultiple  = 64 // LTX's two-stage pipeline (engine/ltx.go)
	genVideoMax       = 1280
	genVideoMaxSecond = 10
	genUpscaleMax     = 4096      // SeedVR2 (engine.UpscaleMax): the longest side of the result
	genCutoutMax      = 4096      // BiRefNet: the longest side of an image it cuts out (the result has its size)
	genImageRead      = 256 << 20 // an image result read to make it a JPEG (a 4096² PNG is 30–50 MB)
	genJPEGQuality    = 92
)

// genUpscaleScales are the factors an upscaler takes (a request's scale goes down to the nearest).
var genUpscaleScales = []int{2, 4}

// genPoll is how often a generation's job is asked for (its status goes to vboard at most this often).
var genPoll = time.Second

// genMaxImageFile: vboard keeps image assets of at most 15 MB (a larger PNG goes as a JPEG).
var genMaxImageFile int64 = 15 << 20

// genTypes are the reference images a generator takes.
var genTypes = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp"}

// advertisedGenerator is a generator model as the connector offers it now (a connected worker has it on disk).
type advertisedGenerator struct {
	ID, Name, Kind, Worker string
	Model                  string // the catalogue id
	// image: Edit takes reference images, TextToImage makes one without (false: an editing model that needs one);
	// video: FirstFrame may start from an image, Audio has a soundtrack
	Edit, TextToImage, FirstFrame, Audio bool
	Multiple, Max                        int // sides are rounded down to Multiple; the longer side is at most Max
	Width, Height, FPS, Seconds          int // the defaults
	MaxSeconds                           int
	Scales                               []int // upscale: the factors it takes
	NonCommercial                        bool
	// music: it sings lyrics, it leaves the voice out, it takes a tempo (Seconds, MaxSeconds: its lengths);
	// speech: its voices, languages (none: it detects them), instruct, the longest text
	Lyrics, Instrumental, BPM, Instruct bool
	Voices                              []genVoice
	Languages                           []string
	MaxChars                            int
}

// generatorOf is what a catalogue model can do on the board (false: it is no image or video model the connector
// knows how to ask): FLUX and Qwen-Image (mflux) for images, SeedVR2 (mflux, task "upscale") for upscales, BiRefNet
// (task "cutout") for cutouts, LTX for videos.
func generatorOf(m catalogModel) (advertisedGenerator, bool) {
	d := modelDefaults{}
	if m.Defaults != nil {
		d = *m.Defaults
	}
	g := advertisedGenerator{Kind: m.Kind, NonCommercial: m.NonCommercial}
	switch {
	case m.Kind == "image" && m.Backend == "mflux" && slices.Contains(m.Tasks, "upscale"):
		g.Kind, g.Scales, g.Max = "upscale", genUpscaleScales, genUpscaleMax
	case m.Kind == "image" && slices.Contains(m.Tasks, "cutout"):
		g.Kind, g.Max = "cutout", genCutoutMax
	case m.Kind == "image" && m.Backend == "mflux":
		// a model with tasks does only those (Qwen-Image: text to image, Qwen-Image-Edit: editing); FLUX does both
		g.Edit = len(m.Tasks) == 0 || slices.Contains(m.Tasks, "edit")
		g.TextToImage = len(m.Tasks) == 0 || slices.Contains(m.Tasks, "generate")
		g.Multiple, g.Max = genImageMultiple, genImageMax
		g.Width, g.Height = cmpOr(d.Width, 1024), cmpOr(d.Height, 1024)
	case m.Kind == "video" && m.Backend == "ltx":
		g.FirstFrame, g.Audio = true, true
		g.Multiple, g.Max, g.MaxSeconds = genVideoMultiple, genVideoMax, genVideoMaxSecond
		g.Width, g.Height, g.FPS = cmpOr(d.Width, 704), cmpOr(d.Height, 448), cmpOr(d.FPS, 24)
		g.Seconds = 4
		if d.Frames > 1 {
			g.Seconds = max(1, (d.Frames-1)/g.FPS) // 97 frames at 24 fps: 4 s
		}
	case audioKind(m.Kind):
		return audioGenerator(m, g)
	default:
		return g, false
	}
	return g, true
}

func cmpOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// generatorNode is the worker a generator's job goes to: one that has the model on disk (nothing is downloaded for a
// board) and fits it, preferring one with it loaded, then an idle one (choose). nil: none.
func (s *Server) generatorNode(routed []*node, m catalogModel) *node {
	var here []*node
	for _, n := range routed {
		_, _, _, models := n.snapshot()
		for _, x := range models {
			if x.ID == m.ID && x.Here {
				here = append(here, n)
				break
			}
		}
	}
	n, ok := choose(here, s.fleet.primary(routed), m, true, nil)
	if !ok {
		return nil
	}
	return n
}

// generatorCaps are the connector's generators a connected worker has on disk now, in the order of its setting.
// Image and video models are shared like chat models: any worker of the fleet, not only the owner's.
func (s *Server) generatorCaps(routed []*node, conf storedConnector) []advertisedGenerator {
	if len(conf.Generators) == 0 {
		return nil
	}
	models := fleetModels(routed)
	var out []advertisedGenerator
	seen := map[string]bool{}
	var ids []string
	for _, id := range conf.Generators {
		if id != allGenerators {
			ids = append(ids, id)
			continue
		}
		var all []string // "*": every generator model on disk, after those named before it, the defaults first
		for _, fm := range models {
			all = append(all, fm.ID)
		}
		slices.SortStableFunc(all, func(a, b string) int { return genRank(a) - genRank(b) })
		ids = append(ids, all...)
	}
	for _, id := range ids {
		fm, ok := findModel(models, id)
		if !ok || seen[fm.ID] {
			continue
		}
		g, ok := generatorOf(fm.catalogModel)
		if !ok {
			continue
		}
		n := s.generatorNode(routed, fm.catalogModel)
		if n == nil {
			continue
		}
		seen[fm.ID] = true
		name := fm.Name
		if name == "" {
			name = fm.ID
		}
		g.ID, g.Name, g.Worker, g.Model = trimRunes(fm.ID, agentChars), trimRunes(name, agentChars), trimRunes(n.name, agentChars), fm.ID
		out = append(out, g)
	}
	return kindsFirst(out)
}

// kindsFirst puts the first generator of every kind in front (in their order), then the others, and keeps at most
// maxGenerators: vboard reads no more than that, and a long "*" list must not cut a kind off (a Mac with many image
// and video models and one sound model). The order within a kind — the first is its default — stays.
func kindsFirst(gs []advertisedGenerator) []advertisedGenerator {
	var first, rest []advertisedGenerator
	seen := map[string]bool{}
	for _, g := range gs {
		if seen[g.Kind] {
			rest = append(rest, g)
			continue
		}
		seen[g.Kind] = true
		first = append(first, g)
	}
	out := append(first, rest...)
	if len(out) > maxGenerators {
		out = out[:maxGenerators]
	}
	return out
}

// wireGenerators is the list as vboard reads it (hello.generators, {type:"generators"}).
func wireGenerators(gs []advertisedGenerator) []map[string]any {
	out := make([]map[string]any, len(gs))
	for i, g := range gs {
		if audioKind(g.Kind) {
			out[i] = wireAudioGenerator(g)
			continue
		}
		if g.Kind == "upscale" {
			out[i] = map[string]any{"id": g.ID, "name": g.Name, "kind": g.Kind, "worker": g.Worker, "scales": g.Scales,
				"max": g.Max, "nonCommercial": g.NonCommercial}
			continue
		}
		if g.Kind == "cutout" { // one image in, the same size out: no sizes, no factors
			out[i] = map[string]any{"id": g.ID, "name": g.Name, "kind": g.Kind, "worker": g.Worker, "max": g.Max,
				"nonCommercial": g.NonCommercial}
			continue
		}
		w := map[string]any{"id": g.ID, "name": g.Name, "kind": g.Kind, "worker": g.Worker, "multiple": g.Multiple,
			"max": g.Max, "nonCommercial": g.NonCommercial}
		def := map[string]any{"width": g.Width, "height": g.Height}
		if g.Kind == "video" {
			w["firstFrame"], w["audio"], w["maxSeconds"] = g.FirstFrame, g.Audio, g.MaxSeconds
			def["fps"], def["seconds"] = g.FPS, g.Seconds
		} else {
			w["edit"] = g.Edit
			if !g.TextToImage {
				w["textToImage"] = false // it only edits: at least one reference image
			}
		}
		w["default"] = def
		out[i] = w
	}
	return out
}

func generatorIDs(gs []advertisedGenerator) string {
	ids := make([]string, len(gs))
	for i, g := range gs {
		ids[i] = g.ID
	}
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, " ")
}

// allGenerators in the setting stands for every generator model a connected worker has on disk (the default
// of `ai-connector vboard`: a model downloaded from vboard is offered at once).
const allGenerators = "*"

// cleanGenerators checks the setting: catalogue ids (or names as in /v1/*) or "*", unique, at most 20. Whether one
// is an image or video model is seen only when a worker lists it (others are not offered).
func cleanGenerators(in []string) ([]string, error) {
	if len(in) > maxGenerators {
		return nil, &connectorError{"generators", fmt.Sprintf("At most %d generators", maxGenerators)}
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for i, id := range in {
		id = strings.TrimSpace(id)
		if id == "" || len(id) > 200 || strings.ContainsAny(id, " \t\r\n") {
			return nil, &connectorError{"generators", fmt.Sprintf("Generator %d: a model id (an image, video, music, sound or speech model)", i+1)}
		}
		if seen[strings.ToLower(id)] {
			return nil, &connectorError{"generators", fmt.Sprintf("%q is there twice", id)}
		}
		seen[strings.ToLower(id)] = true
		out = append(out, id)
	}
	return out, nil
}

// ---------- on the connection ----------

func (c *bridgeConn) setGenerators(gs []advertisedGenerator) {
	c.cmu.Lock()
	c.caps.generators = gs
	c.cmu.Unlock()
}

// generator is the generator a request names; none named: the first of the kind (image when no kind is given).
func (c *bridgeConn) generator(id, kind string) (advertisedGenerator, bool) {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	if kind == "" {
		kind = "image"
	}
	for _, g := range c.caps.generators {
		if id != "" && g.ID == id || id == "" && g.Kind == kind {
			return g, true
		}
	}
	return advertisedGenerator{}, false
}

// offerGenerators tells vboard the generators now, on the open connection (a vboard before generators drops it).
func (r *vboardRun) offerGenerators(c *bridgeConn, now, was []advertisedGenerator) {
	c.setGenerators(now)
	c.send("", map[string]any{"type": "generators", "generators": wireGenerators(now)})
	r.logf("generators now: %s (was %s)", generatorIDs(now), generatorIDs(was))
}

// ---------- the request ----------

// genRequest is vboard's {generate}.
type genRequest struct {
	Generator string      `json:"generator"`
	Kind      string      `json:"kind"`
	Prompt    string      `json:"prompt"`
	Width     int         `json:"width"`
	Height    int         `json:"height"`
	Seed      int64       `json:"seed"`
	Steps     int         `json:"steps"`
	Seconds   float64     `json:"seconds"`
	FPS       int         `json:"fps"`
	Audio     *bool       `json:"audio"`
	Scale     int         `json:"scale"` // upscale
	Images    []chatImage `json:"images"`
	// music (prompt: the style); speech (text …)
	Lyrics       string `json:"lyrics"`
	Instrumental bool   `json:"instrumental"`
	BPM          int    `json:"bpm"`
	Title        string `json:"title"`
	Text         string `json:"text"`
	Voice        string `json:"voice"`
	Language     string `json:"language"`
	Instruct     string `json:"instruct"`
	Context      *struct {
		BoardID       string `json:"boardId"`
		UserID        string `json:"userId"`
		UserName      string `json:"userName"`
		PlaceholderID string `json:"placeholderId"`
	} `json:"context"`
}

// genPlan is what will be made: the size after rounding (a video's frames and fps) and the images' bytes.
type genPlan struct {
	width, height, frames, fps int
	scale                      int // upscale
	images                     [][]byte
	exts                       []string
	// music, sound, speech: the length to be made (speech: an estimate) and the job's fields
	seconds float64
	job     map[string]any
}

// planGenerate checks a request against its generator and works out what will actually be made: width and height
// (0: the default; the longer side cut to max keeping the aspect ratio, both rounded down to the multiple), a
// video's frames (seconds × fps + 1 rounded down to 8k+1, as LTX takes them), and the reference images.
func planGenerate(g advertisedGenerator, q *genRequest) (genPlan, error) {
	if g.Kind == "upscale" {
		return planUpscale(g, q)
	}
	if g.Kind == "cutout" {
		return planCutout(g, q)
	}
	if audioKind(g.Kind) {
		return planAudio(g, q)
	}
	var p genPlan
	q.Prompt = strings.TrimSpace(q.Prompt)
	switch {
	case q.Prompt == "":
		return p, errors.New("a prompt is needed")
	case len([]rune(q.Prompt)) > genPromptChars:
		return p, fmt.Errorf("the prompt is longer than %d characters", genPromptChars)
	case q.Width < 0 || q.Height < 0 || q.Steps < 0 || q.Seed < 0 || q.Seconds < 0 || q.FPS < 0:
		return p, errors.New("width, height, steps, seed, seconds and fps are 0 (the default) or more")
	}
	w, h := q.Width, q.Height
	switch {
	case w == 0 && h == 0:
		w, h = g.Width, g.Height
	case w == 0:
		w = h * g.Width / g.Height
	case h == 0:
		h = w * g.Height / g.Width
	}
	if long := max(w, h); long > g.Max {
		w, h = w*g.Max/long, h*g.Max/long
	}
	p.width, p.height = max(g.Multiple, w/g.Multiple*g.Multiple), max(g.Multiple, h/g.Multiple*g.Multiple)
	if g.Kind == "video" {
		sec := q.Seconds
		if sec == 0 {
			sec = float64(g.Seconds)
		}
		sec = min(sec, float64(g.MaxSeconds))
		p.fps = min(cmpOr(q.FPS, g.FPS), genMaxFPS)
		p.frames = frames8k1(int(math.Round(sec*float64(p.fps))) + 1)
	}
	if len(q.Images) > genMaxImages {
		return p, fmt.Errorf("at most %d images a request", genMaxImages)
	}
	switch {
	case g.Kind == "image" && len(q.Images) > 0 && !g.Edit:
		return p, fmt.Errorf("%s makes pictures from text only: it takes no reference images", g.Name)
	case g.Kind == "image" && len(q.Images) == 0 && !g.TextToImage:
		return p, fmt.Errorf("%s edits pictures: it needs at least one reference image", g.Name)
	case g.Kind == "video" && len(q.Images) > 0 && !g.FirstFrame:
		return p, fmt.Errorf("%s does not start from an image", g.Name)
	case g.Kind == "video" && len(q.Images) > 1:
		return p, errors.New("a video starts from one image (the first frame)")
	}
	for _, im := range q.Images {
		b, ext, err := genImage(im)
		if err != nil {
			return p, err
		}
		p.images, p.exts = append(p.images, b), append(p.exts, ext)
	}
	return p, nil
}

// frames8k1 rounds a frame count down to 8k+1, at least 9 (as engine/ltx.go does).
func frames8k1(n int) int {
	return max(9, (n-1)/8*8+1)
}

// genImage checks a reference image: png, jpeg or webp, base64 of at most 8 MB.
func genImage(im chatImage) ([]byte, string, error) {
	name := im.ID
	if name == "" {
		name = "an image"
	}
	mime := strings.ToLower(strings.TrimSpace(im.MIME))
	if mime == "image/jpg" {
		mime = "image/jpeg"
	}
	ext, ok := genTypes[mime]
	if !ok {
		return nil, "", fmt.Errorf("%s: image type %q is not taken (png, jpeg, webp)", name, im.MIME)
	}
	if base64.StdEncoding.DecodedLen(len(im.Data)) > genImageBytes+3 {
		return nil, "", fmt.Errorf("%s: the image is larger than %d MB", name, genImageBytes>>20)
	}
	b, err := base64.StdEncoding.DecodeString(im.Data)
	switch {
	case err != nil:
		return nil, "", fmt.Errorf("%s: the image's base64 does not decode", name)
	case len(b) == 0:
		return nil, "", fmt.Errorf("%s: no image data", name)
	case len(b) > genImageBytes:
		return nil, "", fmt.Errorf("%s: the image is larger than %d MB", name, genImageBytes>>20)
	}
	return b, ext, nil
}

// genJob is the part of a worker's generation job the connector reads.
type genJob struct {
	State    string         `json:"state"`
	Status   string         `json:"status"`
	Error    string         `json:"error"`
	Step     int            `json:"step"`
	Total    int            `json:"total"`
	Files    []string       `json:"files"`
	Started  time.Time      `json:"started"`
	Finished time.Time      `json:"finished"`
	Stats    map[string]any `json:"stats"`
	Result   map[string]any `json:"result"` // speech: what was said (text, voice, language, words)
}

func (j *genJob) stat(k string) (float64, bool) {
	v, ok := j.Stats[k].(float64)
	return v, ok
}

// generate answers a {generate}: the job on a worker that has the model, {generateStatus} while it waits and runs,
// the file in {generateFile} pieces, {generateDone} — or {error}. Nothing is sent once it is canceled
// ({generateCancel}, the connection or the connector stopped): the job is canceled on the worker.
func (r *vboardRun) generate(ctx context.Context, c *bridgeConn, id string, raw []byte) {
	last := time.Now()
	send := func(v map[string]any) {
		v["id"] = id
		c.send(id, v)
		last = time.Now()
	}
	fail := func(msg string) {
		if ctx.Err() != nil {
			r.logf("generate %s stopped (canceled)", id)
			return
		}
		msg = trimRunes(msg, bridgeErrorChars)
		r.logf("generate %s failed: %s", id, msg)
		send(map[string]any{"type": "error", "message": msg})
	}
	var q genRequest
	if err := json.Unmarshal(raw, &q); err != nil {
		fail("a generate request: " + err.Error())
		return
	}
	g, ok := c.generator(q.Generator, q.Kind)
	if !ok {
		if q.Generator != "" {
			fail(fmt.Sprintf("%q is not available now: no connected worker has it", q.Generator))
		} else {
			kind := cmpStr(q.Kind, "image")
			what := kind + "s"
			if kind == "music" || kind == "speech" {
				what = kind
			}
			fail(fmt.Sprintf("this connector cannot make %s now: no %s generator is offered", what, kind))
		}
		return
	}
	if g.Kind == "speech" { // the language decides which one reads it
		var err error
		if g, err = c.pickSpeech(g, &q); err != nil {
			fail(err.Error())
			return
		}
	}
	plan, err := planGenerate(g, &q)
	if err != nil {
		fail(err.Error())
		return
	}
	routed, _ := r.s.fleet.pick(ctx)
	fm, ok := findModel(fleetModels(routed), g.Model)
	var n *node
	if ok {
		n = r.s.generatorNode(routed, fm.catalogModel)
	}
	if n == nil {
		fail(fmt.Sprintf("%q is not available now: no connected worker has it", g.ID))
		return
	}
	job := map[string]any{"model": g.Model, "prompt": q.Prompt, "width": plan.width, "height": plan.height,
		"client": "vboard: " + c.conf.Name}
	if g.Kind == "upscale" {
		// one image and the factor: the worker works out the size itself (as planUpscale did)
		job = map[string]any{"model": g.Model, "scale": plan.scale, "client": "vboard: " + c.conf.Name}
	}
	if g.Kind == "cutout" { // the one image (below), nothing else
		job = map[string]any{"model": g.Model, "client": "vboard: " + c.conf.Name}
	}
	if audioKind(g.Kind) { // music, sound, speech: their own fields (planAudio)
		job = map[string]any{"model": g.Model, "client": "vboard: " + c.conf.Name}
		maps.Copy(job, plan.job)
	}
	if q.Seed > 0 && g.Kind != "cutout" {
		job["seed"] = q.Seed
	}
	if q.Steps > 0 && g.Kind != "upscale" && g.Kind != "cutout" && g.Kind != "sound" && g.Kind != "speech" { // SeedVR2 takes one step
		job["steps"] = q.Steps
	}
	if g.Kind == "video" {
		job["frames"], job["fps"] = plan.frames, plan.fps
		if q.Audio != nil && !*q.Audio {
			job["noAudio"] = true
		}
	}
	// the references go to the worker first: a job names its images by their /files/ urls there
	var urls []string
	for i, b := range plan.images {
		u, err := r.s.uploadFile(ctx, n, fmt.Sprintf("vboard-%d%s", i+1, plan.exts[i]), b)
		if err != nil {
			fail(fmt.Sprintf("the image to %s: %v", n.name, err))
			return
		}
		urls = append(urls, u)
	}
	if len(urls) > 0 {
		job["images"] = urls
	}
	_, _, st0, _ := n.snapshot()
	run := &chatRun{s: r.s, n: n, model: g.Model}
	jid, state, e := r.s.startChat(ctx, n, job)
	if e != nil {
		fail(e.message)
		return
	}
	run.id = jid
	who := ""
	if q.Context != nil && q.Context.UserName != "" {
		who = " for " + q.Context.UserName
	}
	// the status: queued (with the jobs before it) → loading (the model is read: no steps yet) → running (steps)
	st := map[string]any{"type": "generateStatus", "worker": n.name, "width": plan.width, "height": plan.height}
	if g.Kind == "video" {
		st["frames"], st["fps"] = plan.frames, plan.fps
	}
	if audioKind(g.Kind) {
		// the length instead of a size; speech names the generator that reads it (the language may have chosen it)
		st = map[string]any{"type": "generateStatus", "worker": n.name, "seconds": plan.seconds}
		if g.Kind == "speech" {
			st["generator"], st["name"] = g.ID, g.Name
		}
		r.logf("generate %s%s: %s on %s (%g s)", id, who, g.ID, n.name, plan.seconds)
	} else {
		r.logf("generate %s%s: %s on %s (%d×%d)", id, who, g.ID, n.name, plan.width, plan.height)
	}
	ahead, running := st0.load(), ""
	if st0.Running != nil {
		running = *st0.Running
	}
	var sent string
	var firstStep int
	var firstAt time.Time
	status := func(j *genJob) {
		s := map[string]any{}
		for k, v := range st {
			s[k] = v
		}
		switch {
		case j.State == "queued":
			// a job ahead is done when another starts on the worker
			if _, _, now, _ := n.snapshot(); now.Running != nil && *now.Running != running {
				if running != "" && *now.Running != jid {
					ahead--
				}
				running = *now.Running
			}
			s["state"], s["position"] = "queued", max(0, ahead)
		case j.Total <= 0:
			s["state"] = "loading"
		default:
			s["state"], s["step"], s["total"] = "running", j.Step, j.Total
			if firstAt.IsZero() {
				firstStep, firstAt = j.Step, time.Now()
			} else if j.Step > firstStep && j.Step <= j.Total {
				per := time.Since(firstAt).Seconds() / float64(j.Step-firstStep)
				s["eta"] = int(math.Ceil(per * float64(j.Total-j.Step)))
			}
		}
		key, _ := json.Marshal(s)
		if string(key) != sent || time.Since(last) >= bridgeKeepAlive {
			sent = string(key)
			send(s)
		}
	}
	status(&genJob{State: state})

	poll := time.NewTicker(genPoll)
	defer poll.Stop()
	var j *genJob
	misses := 0
	for j == nil {
		select {
		case <-ctx.Done():
			run.cancel()
			r.logf("generate %s canceled", id)
			return
		case <-poll.C:
		}
		var x genJob
		gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := r.s.fleet.getJSON(gctx, n, "/api/jobs/"+jid, &x)
		cancel()
		switch {
		case ctx.Err() != nil:
			continue // canceled: said above
		case err != nil:
			if misses++; misses >= genFailedPolls {
				fail(fmt.Sprintf("%s does not answer (%v): the generation is lost", n.name, err))
				return
			}
		case finished(x.State):
			j = &x
		default:
			misses = 0
			status(&x)
		}
	}
	switch j.State {
	case "failed":
		fail(cmpStr(j.Error, "the job failed"))
		return
	case "canceled":
		fail("the job was canceled on " + n.name)
		return
	}
	if audioKind(g.Kind) {
		seconds, peaks, ok := r.sendAudio(ctx, c, id, n, g, j, fail)
		if !ok {
			return
		}
		send(audioDone(g, &q, plan, j, seconds, peaks))
		r.logf("generate %s: done in %.1f s on %s", id, j.Finished.Sub(j.Started).Seconds(), n.name)
		return
	}
	if !r.sendFile(ctx, c, id, n, g, j, fail) {
		return
	}
	done := map[string]any{"type": "generateDone", "width": plan.width, "height": plan.height, "generator": g.ID,
		"seed": q.Seed, "took": math.Round(j.Finished.Sub(j.Started).Seconds()*10) / 10}
	if v, ok := j.stat("width"); ok && v > 0 {
		done["width"] = int(v)
	}
	if v, ok := j.stat("height"); ok && v > 0 {
		done["height"] = int(v)
	}
	if v, ok := j.stat("seed"); ok {
		done["seed"] = int64(v)
	}
	if g.Kind == "video" {
		frames, fps := float64(plan.frames), float64(plan.fps)
		if v, ok := j.stat("frames"); ok && v > 0 {
			frames = v
		}
		if v, ok := j.stat("fps"); ok && v > 0 {
			fps = v
		}
		done["seconds"] = math.Round((frames-1)/fps*100) / 100
	}
	send(done)
	r.logf("generate %s: done in %.1f s on %s", id, j.Finished.Sub(j.Started).Seconds(), n.name)
}

func cmpStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// sendFile fetches the job's file from its worker and sends it in pieces: base64 of at most 1 MiB each, in order
// (seq), the last one says so. false: it failed (said) or was canceled.
func (r *vboardRun) sendFile(ctx context.Context, c *bridgeConn, id string, n *node, g advertisedGenerator, j *genJob, fail func(string)) bool {
	file, mime := "", ""
	for _, f := range j.Files {
		switch strings.ToLower(path.Ext(f)) {
		case ".png":
			file, mime = f, "image/png"
		case ".webp": // a cutout too large as a PNG (engine.CutoutMaxPNG): a WebP keeps its transparency
			file, mime = f, "image/webp"
		case ".mp4":
			file, mime = f, "video/mp4"
		}
	}
	limit, what := genMaxImageFile, "image"
	if mime == "video/mp4" {
		limit, what = genMaxVideoFile, "video"
	}
	if file == "" || (g.Kind == "video") != (mime == "video/mp4") {
		fail(fmt.Sprintf("the job made no %s file", g.Kind))
		return false
	}
	res, err := r.s.fleet.request(ctx, n, http.MethodGet, file, nil, "")
	if err != nil {
		fail(fmt.Sprintf("the file from %s: %v", n.name, err))
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		fail(fmt.Sprintf("the file from %s: %s", n.name, res.Status))
		return false
	}
	size := res.ContentLength
	body := io.Reader(res.Body)
	read := limit // not said: read it whole (up to the limit) to know; an image larger than vboard takes as well
	if mime == "image/png" {
		read = genImageRead
	}
	// a PNG is always read whole: its metadata goes (pngWithoutMeta), whatever its size
	if size < 0 || size > limit && size <= read || mime == "image/png" && size <= read {
		b, err := io.ReadAll(io.LimitReader(res.Body, read+1))
		if err != nil {
			fail(fmt.Sprintf("the file from %s: %v", n.name, err))
			return false
		}
		if mime == "image/png" {
			b = pngWithoutMeta(b)
		}
		size, body = int64(len(b)), bytes.NewReader(b)
		// a PNG larger than vboard takes (a 4096² upscale) goes as a JPEG of quality 92 — not a cutout: a JPEG has no
		// transparency (its worker writes a WebP instead of a PNG that large)
		if mime == "image/png" && size > limit && size <= read && g.Kind != "cutout" {
			jb, err := asJPEG(b)
			if err != nil {
				fail(fmt.Sprintf("the image is %.1f MB, more than vboard takes (%d MB), and could not be made a JPEG: %v",
					float64(size)/(1<<20), limit>>20, err))
				return false
			}
			r.logf("generate %s: the PNG is %.1f MB, sent as a JPEG of %.1f MB", id, float64(size)/(1<<20), float64(len(jb))/(1<<20))
			mime, size, body, what = "image/jpeg", int64(len(jb)), bytes.NewReader(jb), "image (as JPEG)"
		}
	}
	if size > limit {
		fail(fmt.Sprintf("the %s is %.1f MB, more than vboard takes (%d MB)", what, float64(size)/(1<<20), limit>>20))
		return false
	}
	return r.sendPieces(ctx, c, id, n, mime, size, body, fail)
}

// sendPieces sends a file of size bytes in pieces: base64 of at most 1 MiB each, in order (seq), the last one says
// so. false: it failed (said) or was canceled.
func (r *vboardRun) sendPieces(ctx context.Context, c *bridgeConn, id string, n *node, mime string, size int64, body io.Reader, fail func(string)) bool {
	buf := make([]byte, genPieceBytes)
	var sent int64
	for seq := 0; sent < size || seq == 0; seq++ {
		k, err := io.ReadFull(body, buf[:min(int64(genPieceBytes), size-sent)])
		if err != nil && !(size == 0 && errors.Is(err, io.EOF)) {
			fail(fmt.Sprintf("the file from %s: %v", n.name, err))
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		sent += int64(k)
		c.send(id, map[string]any{"type": "generateFile", "id": id, "mime": mime, "size": size, "seq": seq,
			"data": base64.StdEncoding.EncodeToString(buf[:k]), "last": sent >= size})
	}
	return true
}

// uploadFile puts a file into a worker's uploads (POST /api/upload) and returns its /files/ url there.
func (s *Server) uploadFile(ctx context.Context, n *node, name string, data []byte) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", name)
	if err == nil {
		_, err = part.Write(data)
	}
	if err == nil {
		err = mw.Close()
	}
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := s.fleet.request(ctx, n, http.MethodPost, "/api/upload", &body, mw.FormDataContentType())
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct {
		URL string `json:"url"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&out) != nil || out.URL == "" {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return "", fmt.Errorf("%s %s", res.Status, strings.TrimFunc(string(msg), unicode.IsSpace))
	}
	return out.URL, nil
}
