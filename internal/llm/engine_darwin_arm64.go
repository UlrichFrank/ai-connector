//go:build darwin && arm64

package llm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
	"unicode/utf8"

	"github.com/philipparndt/ai-connector/internal/mlx"
	"github.com/philipparndt/ai-connector/internal/tokenizer"
)

// Engine runs one loaded chat model. All MLX work happens on one goroutine locked to its OS thread (MLX's streams
// and this package's temporaries are per thread); Generate calls are served one after the other.
type Engine struct {
	Cfg    *Config
	Format string // tool-call format: "json" or "xml"
	tok    *tokenizer.Tokenizer
	md     *model
	eos    map[int]bool
	work   chan func()
	wired  int // the GPU's recommended working set: kept resident while answering (like mlx-lm)
	// the sampling settings of config.json's model type and generation_config.json
	defaults Defaults

	// the prompt cache: the state after the last prompt (a follow-up round of the same conversation extends it) and
	// where its images were (their tokens are the same for every image: the checksums tell them apart)
	prompt      []int
	promptSpans []imageSpan
	snap        *cache
}

// Stats describe one generation.
type Stats struct {
	PromptTokens, CachedTokens, Tokens int
	Prefill, FirstToken, Total         time.Duration
	Images                             int           // images run through the vision tower (not taken from the cache)
	Vision                             time.Duration // the time that took (part of Prefill)
}

// TokensPerSecond is the decoding speed (after the first token).
func (s Stats) TokensPerSecond() float64 {
	d := (s.Total - s.FirstToken).Seconds()
	if s.Tokens <= 1 || d <= 0 {
		return 0
	}
	return float64(s.Tokens-1) / d
}

// PrefillPerSecond is the prompt speed (tokens not taken from the cache).
func (s Stats) PrefillPerSecond() float64 {
	if s.Prefill <= 0 {
		return 0
	}
	return float64(s.PromptTokens-s.CachedTokens) / s.Prefill.Seconds()
}

// Available: this build can run the built-in chat model (MLX loads).
func Available() error {
	_, err := mlx.Open()
	return err
}

// Load opens MLX (unpacking it on first use) and loads the model in dir.
func Load(dir string) (*Engine, error) {
	if _, err := mlx.Open(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, err
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	tok, err := tokenizer.Load(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		return nil, err
	}
	gen, err := ReadGenerationConfig(dir)
	if err != nil {
		return nil, fmt.Errorf("generation_config.json: %w", err)
	}
	e := &Engine{Cfg: cfg, Format: cfg.ToolFormat(), tok: tok, eos: map[int]bool{}, work: make(chan func()),
		defaults: ModelDefaults(cfg, gen)}
	for _, id := range cfg.EOS {
		e.eos[id] = true
	}
	for _, s := range []string{"<|im_end|>", "<|endoftext|>"} {
		if id, ok := tok.ID(s); ok {
			e.eos[id] = true
		}
	}
	go e.loop()
	err = e.do(func() error {
		md, err := loadModel(dir, cfg)
		if err != nil {
			return err
		}
		e.md = md
		// weights are small next to the machine's memory by choice (Chat.Fits); keep freed buffers bounded
		mlx.SetCacheLimit(1 << 30)
		md.materialize()
		mlx.Seed(uint64(time.Now().UnixNano()))
		e.wired = mlx.MaxWorkingSet()
		return nil
	})
	if err != nil {
		e.Close()
		return nil, err
	}
	return e, nil
}

func (e *Engine) loop() {
	runtime.LockOSThread()
	for f := range e.work {
		f()
	}
}

// do runs f on the engine's thread (MLX errors come back as errors).
func (e *Engine) do(f func() error) error {
	done := make(chan error, 1)
	e.work <- func() {
		var err error
		mlx.WithAutoreleasePool(func() {
			err = mlx.Guard(f)
		})
		mlx.FreeTemps()
		done <- err
	}
	return <-done
}

// Close releases the model.
func (e *Engine) Close() {
	if e.work == nil {
		return
	}
	_ = e.do(func() error {
		if e.snap != nil {
			e.snap.free()
			e.snap = nil
		}
		if e.md != nil {
			e.md.free()
			e.md = nil
		}
		mlx.ClearCache()
		return nil
	})
	close(e.work)
	e.work = nil
}

// Memory is MLX's active memory and its peak (bytes).
func (e *Engine) Memory() (active, peak int) {
	_ = e.do(func() error { active, peak = mlx.ActiveMemory(), mlx.PeakMemory(); return nil })
	return
}

// Defaults are the model's sampling settings with thinking on and off (ModelDefaults).
func (e *Engine) Defaults() Defaults { return e.defaults }

// ErrPromptTooLong: the conversation does not fit.
var ErrPromptTooLong = errors.New("the conversation is too long for the local model")

var (
	prof                                     = os.Getenv("MLXLAB_LLM_PROF") != ""
	profBuild, profWait, profFree, profGraph time.Duration
)

// ProfReport is the time the decoding loop spent building graphs, waiting for the GPU and releasing arrays
// (MLXLAB_LLM_PROF=1).
func ProfReport() string {
	return fmt.Sprintf("graph %v, graph+async_eval %v, wait %v, free %v", profGraph, profBuild, profWait, profFree)
}

const (
	maxPrompt    = 65536
	prefillChunk = 512
)

// Generate continues prompt (the rendered conversation) and passes the text on as it comes (whole UTF-8 characters).
func (e *Engine) Generate(ctx context.Context, prompt string, o Options, onText func(string)) (Stats, error) {
	return e.GenerateImages(ctx, prompt, nil, o, onText)
}

// Images: the model has a vision tower the engine runs (GenerateImages takes images).
func (e *Engine) Images() bool { return e.md != nil && e.md.vision != nil }

// ErrNoImages: the model sees no images.
var ErrNoImages = errors.New("the model does not take images")

// GenerateImages is Generate for a prompt with images: Render's placeholders, one per image of images in the same
// order (PromptImages).
func (e *Engine) GenerateImages(ctx context.Context, prompt string, images []Image, o Options, onText func(string)) (Stats, error) {
	var st Stats
	start := time.Now()
	ids := e.tok.Encode(prompt)
	var spans []imageSpan
	if len(images) > 0 {
		if !e.Images() {
			return st, ErrNoImages
		}
		var err error
		if ids, spans, err = e.Cfg.Vision.expandImages(ids, images); err != nil {
			return st, err
		}
	}
	st.PromptTokens = len(ids)
	if len(ids) == 0 {
		return st, errors.New("empty prompt")
	}
	if len(ids) > maxPrompt {
		return st, fmt.Errorf("%w (%d tokens)", ErrPromptTooLong, len(ids))
	}
	if o.MaxTokens <= 0 {
		o.MaxTokens = 4096
	}
	if o.TopK <= 0 {
		o.TopK = 20
	}
	var pending []byte
	emit := func(b []byte) {
		pending = append(pending, b...)
		n := validPrefix(pending)
		if n > 0 {
			onText(string(pending[:n]))
			pending = append(pending[:0], pending[n:]...)
		}
	}
	err := e.do(func() error {
		if e.wired > 0 {
			old := mlx.SetWiredLimit(e.wired)
			defer mlx.SetWiredLimit(old)
		}
		c, logits, err := e.prefill(ctx, ids, spans, &st)
		if err != nil {
			return err
		}
		defer c.free()
		s := newSampler(o)
		defer s.free()
		y := mlx.Keep(s.next(logits))
		mlx.Eval(append(append(c.arrays(), y), s.state()...)...)
		st.Prefill = time.Since(start)
		if e.snap != nil {
			e.snap.free()
		}
		e.snap, e.prompt, e.promptSpans = c.snapshot(), ids, withoutData(spans)
		mlx.FreeTemps()
		defer func() { mlx.Free(y) }()
		for i := 0; ; i++ {
			// the next step is queued before this token is read: the GPU works while Go decodes and sends
			var next array
			t0 := time.Now()
			if i+1 < o.MaxTokens {
				next = mlx.Keep(s.next(e.md.forward(mlx.Reshape(y, 1, 1), c)))
				if prof {
					profGraph += time.Since(t0)
				}
				mlx.AsyncEval(append([]array{next}, s.state()...)...)
			}
			t1 := time.Now()
			tok := y.ItemInt()
			t2 := time.Now()
			mlx.Free(y)
			y = next
			mlx.FreeTemps()
			if prof {
				profBuild += t1.Sub(t0)
				profWait += t2.Sub(t1)
				profFree += time.Since(t2)
			}
			if i == 0 {
				st.FirstToken = time.Since(start)
			}
			if e.eos[tok] {
				break
			}
			st.Tokens++
			emit(e.tok.DecodeBytes([]int{tok}, false))
			if !next.Valid() || ctx.Err() != nil {
				break
			}
		}
		return ctx.Err()
	})
	if len(pending) > 0 {
		onText(string(pending))
	}
	st.Total = time.Since(start)
	return st, err
}

// prefill runs the prompt's tokens the cache does not hold, in chunks (bounded memory), each image through the vision
// tower when its first token comes: the cache after the prompt and the logits of its last token.
func (e *Engine) prefill(ctx context.Context, ids []int, spans []imageSpan, st *Stats) (c *cache, logits array, err error) {
	c, rest := e.startFrom(ids, spans)
	st.CachedTokens = len(ids) - len(rest)
	done := false
	defer func() {
		if !done {
			c.free()
		}
	}()
	var pos [][3]int
	nextPos := 0
	if len(spans) > 0 {
		pos, nextPos = e.Cfg.Vision.ropePositions(len(ids), spans)
	}
	feats := map[int]array{} // the images' features (kept while the prompt runs)
	defer func() {
		for _, f := range feats {
			mlx.Free(f)
		}
	}()
	// the prompt, in chunks (bounded memory)
	for len(rest) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, logits, err
		}
		n := min(prefillChunk, len(rest))
		from := len(ids) - len(rest)
		var mm *multimodal
		if pos != nil {
			mm = &multimodal{pos: pos[from : from+n], next: nextPos}
			if from+n < len(ids) {
				mm.next = pos[from+n][0]
			}
			for k, sp := range spans {
				lo, hi := max(sp.from, from), min(sp.from+sp.n, from+n)
				if lo >= hi {
					continue
				}
				f, ok := feats[k]
				if !ok {
					t0 := time.Now()
					px, err := e.Cfg.Vision.PrepareImage(sp.data)
					if err != nil {
						return nil, logits, fmt.Errorf("%s: %w", sp.id, err)
					}
					f = mlx.Keep(e.md.vision.encode(px, e.md.embed.scales.Dtype()))
					feats[k] = f
					mlx.Eval(f)
					mlx.FreeTemps()
					st.Images++
					st.Vision += time.Since(t0)
				}
				mm.feats = append(mm.feats, placed{feats: f, at: lo - from, from: lo - sp.from, to: hi - sp.from})
			}
		}
		logits = e.md.forwardMM(idsArray(rest[:n]), c, mm)
		rest = rest[n:]
		if len(rest) > 0 {
			mlx.Eval(c.arrays()...)
			mlx.FreeTemps()
		}
	}
	done = true
	return c, logits, nil
}

// startFrom picks up the cached state when the prompt continues the last one (a new round of the same
// conversation): the cache and the tokens still to run. A model with only attention layers can also cut the cache
// back to a shorter common prefix; the recurrent state of the gated delta-net layers cannot be cut.
func (e *Engine) startFrom(ids []int, spans []imageSpan) (*cache, []int) {
	if e.snap != nil {
		common := 0
		for common < len(ids) && common < len(e.prompt) && ids[common] == e.prompt[common] {
			common++
		}
		// the same tokens with another image are another prompt from that image on
		for k, sp := range e.promptSpans {
			if sp.from < common && (k >= len(spans) || spans[k].sum != sp.sum || spans[k].from != sp.from) {
				common = sp.from
				break
			}
		}
		switch {
		case common == len(e.prompt) && common < len(ids):
			return e.snap.snapshot(), ids[common:]
		case !e.Cfg.Hybrid() && len(e.promptSpans) == 0 && common > 0 && common < len(ids):
			c := e.snap.snapshot()
			c.offset, c.pos = common, common
			return c, ids[common:]
		}
	}
	return &cache{}, ids
}

// withoutData: the spans without the images' bytes (what the prompt cache keeps).
func withoutData(spans []imageSpan) []imageSpan {
	out := make([]imageSpan, len(spans))
	for i, sp := range spans {
		sp.data = nil
		out[i] = sp
	}
	return out
}

func (c *cache) arrays() []array {
	var out []array
	for _, l := range c.layers {
		for _, a := range []array{l.keys, l.values, l.conv, l.state} {
			if a.Valid() {
				out = append(out, a)
			}
		}
	}
	return out
}

func idsArray(ids []int) array {
	v := make([]int32, len(ids))
	for i, t := range ids {
		v[i] = int32(t)
	}
	return mlx.FromInt32(v, 1, len(ids))
}

// validPrefix is the length of b's longest prefix of whole UTF-8 characters (an incomplete character at the end
// waits for the next token).
func validPrefix(b []byte) int {
	n := len(b)
	for i := 1; i <= utf8.UTFMax-1 && i <= n; i++ {
		c := b[n-i]
		if c < 0x80 { // ASCII: complete
			return n
		}
		if c >= 0xC0 { // the leading byte of the last character: is it complete?
			need := 2
			switch {
			case c >= 0xF0:
				need = 4
			case c >= 0xE0:
				need = 3
			}
			if i < need {
				return n - i
			}
			return n
		}
	}
	return n
}
