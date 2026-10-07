package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Options steer a generation. The zero value is greedy decoding (the guard's judge wants exactly that); a chat
// takes the model's defaults (Engine.Defaults) and changes what the request sets.
type Options struct {
	MaxTokens   int     `json:"maxTokens"`   // 0: 4096
	Temperature float32 `json:"temperature"` // 0: greedy (the penalties still apply)
	TopK        int     `json:"topK"`        // with Temperature > 0: sample among the k most likely tokens (0: 20)
	TopP        float32 `json:"topP"`        // nucleus: of the top k, keep the most likely whose probabilities reach p (0 or ≥ 1: off)
	MinP        float32 `json:"minP"`        // of the top k, drop tokens less likely than MinP × the most likely one (0: off)
	// PresencePenalty is subtracted from the logit of every token already written in this answer (Qwen's and
	// OpenAI's presence penalty: once present, however often; 0: off). Against thinking in circles.
	PresencePenalty float32 `json:"presencePenalty"`
	// RepetitionPenalty divides a positive logit (multiplies a negative one) of every token already written in this
	// answer (1 or 0: off). Unlike transformers it does not count the prompt's tokens.
	RepetitionPenalty float32 `json:"repetitionPenalty"`
}

// penalised: the sampler keeps the answer's tokens (the presence mask).
func (o Options) penalised() bool {
	return o.PresencePenalty != 0 || (o.RepetitionPenalty > 0 && o.RepetitionPenalty != 1)
}

// Defaults are a model's sampling settings with thinking on and off.
type Defaults struct {
	Think   Options `json:"think"`
	NoThink Options `json:"noThink"`
}

// For is the settings for a request with thinking on or off.
func (d Defaults) For(think bool) Options {
	if think {
		return d.Think
	}
	return d.NoThink
}

// GenerationConfig is the sampling part of a checkpoint's generation_config.json (nil: not set).
type GenerationConfig struct {
	Temperature       *float32 `json:"temperature"`
	TopK              *int     `json:"top_k"`
	TopP              *float32 `json:"top_p"`
	MinP              *float32 `json:"min_p"`
	PresencePenalty   *float32 `json:"presence_penalty"`
	RepetitionPenalty *float32 `json:"repetition_penalty"`
}

// ReadGenerationConfig reads dir/generation_config.json (none: all unset).
func ReadGenerationConfig(dir string) (GenerationConfig, error) {
	var g GenerationConfig
	raw, err := os.ReadFile(filepath.Join(dir, "generation_config.json"))
	if os.IsNotExist(err) {
		return g, nil
	}
	if err != nil {
		return g, err
	}
	err = json.Unmarshal(raw, &g)
	return g, err
}

// Output budgets. Qwen recommends 32 768 tokens of output for thinking: a hard question thinks for 10 000 tokens and
// more, and a cut thought has no answer. A presence penalty keeps the thinking from looping, so the budget is rarely
// used up; when it is, it costs about 5–10 minutes on a 35B-A3B (60–100 tokens/s) and its KV cache (KVBytesPerToken).
// Without thinking an answer is short: 4096 as before.
const (
	ThinkMaxTokens   = 32768
	NoThinkMaxTokens = 4096
	// the thinking budget is cut so its KV cache stays below this (a dense model with many attention layers)
	maxKVBudget = 4 << 30
)

// ModelDefaults are the recommended settings for a model type, completed by its generation_config.json.
//
// Qwen's model cards ("Best Practices"; Qwen/Qwen3.6-35B-A3B, Qwen/Qwen3.5-9B, Qwen/Qwen3-8B) recommend
//   - Qwen3.5 / 3.6, thinking: temperature 1.0, top_p 0.95, top_k 20, min_p 0, presence_penalty 1.5
//   - Qwen3.5 / 3.6, no thinking: temperature 0.7, top_p 0.8, top_k 20, min_p 0, presence_penalty 1.5
//   - Qwen3, thinking: temperature 0.6, top_p 0.95, top_k 20, min_p 0 ("DO NOT use greedy decoding, as it can lead
//     to performance degradation and endless repetitions"; presence_penalty 0–2 "to reduce endless repetitions")
//   - Qwen3, no thinking: temperature 0.7, top_p 0.8, top_k 20, min_p 0
//
// generation_config.json holds the model's own default — what these hybrid models were tuned to think with — so its
// values replace the thinking settings (for Qwen3.6-35B-A3B they are the same: 1.0, 0.95, 20); the settings without
// thinking are the card's.
func ModelDefaults(cfg *Config, g GenerationConfig) Defaults {
	think := Options{MaxTokens: ThinkMaxTokens, Temperature: 1.0, TopK: 20, TopP: 0.95, PresencePenalty: 1.5}
	plain := Options{MaxTokens: NoThinkMaxTokens, Temperature: 0.7, TopK: 20, TopP: 0.8, PresencePenalty: 1.5}
	if cfg != nil && isQwen3(cfg.ModelType) {
		think = Options{MaxTokens: ThinkMaxTokens, Temperature: 0.6, TopK: 20, TopP: 0.95}
		plain = Options{MaxTokens: NoThinkMaxTokens, Temperature: 0.7, TopK: 20, TopP: 0.8}
	}
	if g.Temperature != nil && *g.Temperature > 0 {
		think.Temperature = *g.Temperature
	}
	if g.TopK != nil && *g.TopK > 0 {
		think.TopK = *g.TopK
	}
	if g.TopP != nil && *g.TopP > 0 {
		think.TopP = *g.TopP
	}
	if g.MinP != nil {
		think.MinP = *g.MinP
	}
	if g.PresencePenalty != nil {
		think.PresencePenalty = *g.PresencePenalty
	}
	if g.RepetitionPenalty != nil {
		think.RepetitionPenalty = *g.RepetitionPenalty
	}
	if n := cfg.KVBytesPerToken(); n > 0 && think.MaxTokens*n > maxKVBudget {
		think.MaxTokens = max(8192, maxKVBudget/n/1024*1024)
	}
	return Defaults{Think: think, NoThink: plain}
}

// KVBytesPerToken is what one token adds to the KV cache (bf16 keys and values of the attention layers; the gated
// delta-net layers keep a state of fixed size).
func (c *Config) KVBytesPerToken() int {
	if c == nil {
		return 0
	}
	attn := c.Layers
	if c.Hybrid() {
		attn = c.Layers / c.FullAttnInterval
	}
	return attn * 2 * c.KVHeads * c.HeadDim * 2
}

// DefaultsFor reads the defaults of the model in dir without loading it (config.json for the type,
// generation_config.json). An empty or unreadable dir — a catalogue model not downloaded yet — gets Qwen3.5/3.6's.
func DefaultsFor(dir string) Defaults {
	var cfg *Config
	var g GenerationConfig
	if dir != "" {
		if raw, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
			cfg, _ = ParseConfig(raw)
		}
		g, _ = ReadGenerationConfig(dir)
	}
	if cfg == nil {
		cfg = &Config{ModelType: "qwen3_5"}
	}
	return ModelDefaults(cfg, g)
}

// isQwen3: a Qwen3 model type (not 3.5/3.6).
func isQwen3(t string) bool { return strings.HasPrefix(t, "qwen3") && !strings.HasPrefix(t, "qwen3_5") }
