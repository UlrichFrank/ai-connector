package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/agent"
	"github.com/philipparndt/ai-connector/internal/guard"
	"github.com/philipparndt/ai-connector/internal/llm"
)

// ChatBackend runs the chat models on MLX, in this process (vboard-ai's engine: Qwen3, Qwen3.5/3.6 dense and MoE).
type ChatBackend struct {
	Finder *llm.Finder
	// Agent runs chats with tools (Request.Zone != ""); nil: no chat with tools on this Mac.
	Agent *agent.Config

	mu     sync.Mutex
	eng    *llm.Engine
	loaded string
	state  status
}

func (b *ChatBackend) Name() string { return "mlx-go" }
func (b *ChatBackend) Heavy() bool  { return true }

func (b *ChatBackend) Loaded() string { return b.state.get() }

func (b *ChatBackend) Unload() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.eng != nil {
		b.eng.Close()
		b.eng, b.loaded = nil, ""
		b.state.set("")
	}
}

func (b *ChatBackend) load(ctx context.Context, m *Model, emit Emit) error {
	if b.loaded == m.ID && b.eng != nil {
		return nil
	}
	if b.eng != nil {
		b.eng.Close()
		b.eng, b.loaded = nil, ""
		b.state.set("")
	}
	dir := m.dir
	if m.chat != nil {
		// the store and the checksum check report into the job
		store := *b.Finder.Store
		store.Progress = func(format string, a ...any) { emit.Status(strings.TrimSpace(fmt.Sprintf(format, a...))) }
		finder := *b.Finder
		finder.Store = &store
		finder.Verifier = &llm.Verifier{Path: b.Finder.Verifier.Path, Progress: store.Progress}
		c, err := finder.Resolve(m.chat.Short, false)
		if err != nil {
			return err
		}
		if c.Dir == "" && c.Chat != nil {
			if c.Dir, err = fetch(ctx, &store, c.Chat.Model, emit); err != nil {
				return err
			}
			c.Where = "the models directory"
		} else if c, err = finder.Ensure(ctx, c); err != nil {
			return err
		}
		dir = c.Dir
	}
	emit.Status(fmt.Sprintf("loading %s into memory …", m.Name))
	start := time.Now()
	eng, err := llm.Load(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", m.Name, err)
	}
	act, _ := eng.Memory()
	emit.Log(fmt.Sprintf("loaded %s from %s in %.1f s, %.1f GB", m.Name, dir, time.Since(start).Seconds(), float64(act)/(1<<30)))
	b.eng, b.loaded = eng, m.ID
	b.state.set(m.ID)
	return nil
}

func (b *ChatBackend) Run(ctx context.Context, m *Model, r Request, outDir string, emit Emit) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(r.Messages) == 0 {
		return fmt.Errorf("no messages")
	}
	if r.Zone != "" && b.Agent == nil {
		return fmt.Errorf("a %s chat is not available on this Mac yet", r.Zone)
	}
	if err := b.load(ctx, m, emit); err != nil {
		return err
	}
	if r.Zone != "" {
		return b.runAgent(ctx, r, outDir, emit)
	}
	prompt := llm.Render(b.eng.Format, withNow(r.Messages, time.Now()), nil, r.Think)
	if r.Think {
		emit(Event{Type: "text", Text: "<think>"}) // the prompt opened the thought
	}
	emit.Status("answering …")
	o := chatOptions(b.eng.Defaults(), r)
	st, err := b.eng.Generate(ctx, prompt, o, func(s string) { emit(Event{Type: "text", Text: s}) })
	b.stats(st, o, emit)
	return err
}

// runAgent answers with tools (internal/agent): the model may call the zone's tools, round after round.
func (b *ChatBackend) runAgent(ctx context.Context, r Request, outDir string, emit Emit) error {
	g := &engineGen{eng: b.eng, opts: chatOptions(b.eng.Defaults(), r)}
	cfg := *b.Agent
	if cfg.Guard == nil {
		// the guard's default: the chat's own model as the judge (already loaded, no extra memory). The Laya
		// classifier alone missed most injections it was not tuned on (internal/guard, the eval set).
		cfg.Guard = guard.NewJudged(guard.ChatJudge{Complete: b.complete}, guard.Config{})
	}
	_, err := agent.Run(ctx, g, cfg, agent.Request{Zone: agent.Zone(r.Zone), Messages: withNow(r.Messages, time.Now()), Think: r.Think, OutDir: outDir},
		func(ev agent.Event) { emit(Event{Type: ev.Type, Text: ev.Text, File: ev.File, Data: ev.Data}) })
	if g.rounds > 0 {
		b.stats(g.sum, g.opts, emit)
	}
	return err
}

// complete asks the loaded model one question without thinking (the guard's judge: a label at once when screening,
// a few sentences and a number when grading — it stops at the end of its answer, the limit is only a bound).
func (b *ChatBackend) complete(ctx context.Context, system, user string) (string, error) {
	prompt := llm.Render(b.eng.Format, []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: user}}, nil, false)
	var out strings.Builder
	_, err := b.eng.Generate(ctx, prompt, llm.Options{Temperature: 0, MaxTokens: 320}, func(s string) { out.WriteString(s) })
	return out.String(), err
}

// chatOptions are the model's settings for thinking on or off with what the request sets (a chat and every round
// of a chat with tools). Thinking at a low temperature without a penalty loops (Qwen: "DO NOT use greedy decoding
// … endless repetitions"), so an unset field takes the model's recommendation, not a fixed number.
func chatOptions(d llm.Defaults, r Request) llm.Options {
	o := d.For(r.Think)
	if r.Temperature > 0 {
		o.Temperature = r.Temperature
	}
	if r.Greedy {
		o.Temperature = 0
	}
	if r.TopP > 0 {
		o.TopP = r.TopP
	}
	if r.TopK > 0 {
		o.TopK = r.TopK
	}
	if r.PresencePenalty != nil {
		o.PresencePenalty = *r.PresencePenalty
	}
	if r.MaxTokens > 0 {
		o.MaxTokens = r.MaxTokens
	}
	return o
}

// engineGen is the loaded model as the agent's generator; it adds up the rounds' statistics (the prompt and speed
// of the last round, the tokens of all).
type engineGen struct {
	eng    *llm.Engine
	opts   llm.Options
	sum    llm.Stats
	rounds int
}

func (g *engineGen) Format() string { return g.eng.Format }

func (g *engineGen) Generate(ctx context.Context, prompt string, onText func(string)) error {
	st, err := g.eng.Generate(ctx, prompt, g.opts, onText)
	g.rounds++
	tokens := g.sum.Tokens + st.Tokens
	g.sum = st
	g.sum.Tokens = tokens
	return err
}

// stats reports a generation (or a chat's rounds).
func (b *ChatBackend) stats(st llm.Stats, o llm.Options, emit Emit) {
	active, peak := b.eng.Memory()
	emit(Event{Type: "stats", Data: map[string]any{
		// what the answer ran with (a request that left them unset got the model's defaults)
		"maxTokens": o.MaxTokens, "temperature": o.Temperature,
		"promptTokens": st.PromptTokens, "cachedTokens": st.CachedTokens, "tokens": st.Tokens,
		"prefillSeconds": st.Prefill.Seconds(), "prefillPerSecond": st.PrefillPerSecond(),
		"firstTokenSeconds": st.FirstToken.Seconds(), "tokensPerSecond": st.TokensPerSecond(),
		"activeGB": float64(active) / (1 << 30), "peakGB": float64(peak) / (1 << 30),
	}})
	emit.Log(fmt.Sprintf("%d prompt tokens (%d cached) in %.1f s, %d tokens at %.1f tokens/s",
		st.PromptTokens, st.CachedTokens, st.Prefill.Seconds(), st.Tokens, st.TokensPerSecond()))
}

// nowPrefix starts the line that tells the model the date and time.
const nowPrefix = "Current date and time: "

// withNow tells the model when "now" is — it has no clock: a line with the date, weekday, time and time zone of
// this Mac at the end of the system prompt (a system message of its own if there is none). A line from an earlier
// turn is replaced, so a long chat never carries an old date.
func withNow(msgs []llm.Message, now time.Time) []llm.Message {
	zone, _ := now.Zone()
	if name := now.Location().String(); name != "Local" && name != "" {
		zone = name
	} else if tz := os.Getenv("TZ"); tz != "" {
		zone = tz
	} else if link, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(link, "zoneinfo/"); i >= 0 {
			zone = link[i+len("zoneinfo/"):] // macOS: /var/db/timezone/zoneinfo/Europe/Berlin
		}
	}
	line := nowPrefix + now.Format("Monday, 2 January 2006, 15:04") + " (" + zone + ", UTC" + now.Format("-07:00") + ")."
	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	for i, m := range out {
		if m.Role != "system" {
			continue
		}
		var keep []string
		for _, l := range strings.Split(m.Content, "\n") {
			if !strings.HasPrefix(l, nowPrefix) {
				keep = append(keep, l)
			}
		}
		m.Content = strings.TrimRight(strings.Join(keep, "\n"), "\n") + "\n\n" + line
		out[i] = m
		return out
	}
	return append([]llm.Message{{Role: "system", Content: line}}, out...)
}
