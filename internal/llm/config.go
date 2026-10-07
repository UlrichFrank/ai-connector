// Package llm is vboard-ai's built-in chat model on Apple silicon: a Qwen3 / Qwen3.5 / Qwen3.6 decoder (dense or
// Mixture of Experts, with Qwen3.5's gated delta-net layers) on MLX (internal/mlx), loaded from an MLX-format
// quantised checkpoint (mlx-community), with the model's chat template and tool calls. The platform-neutral parts —
// the configuration, the catalogue and the choice by memory, local copies (LM Studio, the Hugging Face cache), the
// prompt, tool-call parsing and sampling — are here and tested everywhere; the forward pass needs MLX
// (engine_darwin_arm64.go).
package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Config is the part of a checkpoint's config.json the decoder needs.
type Config struct {
	ModelType     string // qwen3, qwen3_moe, qwen3_5, qwen3_5_moe
	Hidden        int
	Layers        int
	Heads         int
	KVHeads       int
	HeadDim       int
	Intermediate  int
	Vocab         int
	RMSEps        float32
	RopeTheta     float32
	PartialRotary float32 // the share of each head's features RoPE rotates (Qwen3.5: 0.25)
	TieEmbeddings bool

	// Qwen3.5's gated delta-net (linear attention) layers; every FullAttnInterval-th layer is full attention
	LinearKHeads, LinearVHeads, LinearKDim, LinearVDim, ConvKernel, FullAttnInterval int
	AttnGate                                                                         bool // q_proj also yields a sigmoid gate on the attention output (Qwen3.5)

	// Mixture of Experts
	Experts, ExpertsPerTok, MoEIntermediate, SharedIntermediate, SparseStep int
	NormTopK                                                                bool
	MLPOnlyLayers                                                           []int

	// quantisation (affine): default group size and bits, per-layer exceptions by weight path
	GroupSize, Bits int
	QuantOverride   map[string][2]int

	EOS []int
}

// Hybrid: the model has gated delta-net layers.
func (c *Config) Hybrid() bool { return c.FullAttnInterval > 0 && c.LinearVHeads > 0 }

// IsLinear: layer i is a gated delta-net layer.
func (c *Config) IsLinear(i int) bool { return c.Hybrid() && (i+1)%c.FullAttnInterval != 0 }

// IsMoE: layer i has a Mixture of Experts block.
func (c *Config) IsMoE(i int) bool {
	if c.Experts == 0 {
		return false
	}
	for _, l := range c.MLPOnlyLayers {
		if l == i {
			return false
		}
	}
	step := max(1, c.SparseStep)
	return (i+1)%step == 0
}

// Quant is the group size and bits of the weight at path (e.g. "language_model.model.layers.0.mlp.gate").
func (c *Config) Quant(path string) (groupSize, bits int) {
	if q, ok := c.QuantOverride[path]; ok {
		return q[0], q[1]
	}
	return c.GroupSize, c.Bits
}

// Supported model types.
var supportedTypes = map[string]bool{"qwen3": true, "qwen3_moe": true, "qwen3_5": true, "qwen3_5_moe": true}

// ParseConfig reads config.json (a text model's, or a vision-language model's with text_config: its vision tower is
// not used).
func ParseConfig(raw []byte) (*Config, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	text := top
	if tc, ok := top["text_config"]; ok {
		text = map[string]json.RawMessage{} // not into top's map
		if err := json.Unmarshal(tc, &text); err != nil {
			return nil, fmt.Errorf("config.json text_config: %w", err)
		}
	}
	get := func(key string, v any) bool {
		r, ok := text[key]
		if !ok {
			r, ok = top[key]
		}
		if !ok || string(r) == "null" {
			return false
		}
		return json.Unmarshal(r, v) == nil
	}
	var modelType string
	if r, ok := top["model_type"]; ok {
		_ = json.Unmarshal(r, &modelType)
	}
	if !supportedTypes[modelType] {
		return nil, fmt.Errorf("config.json: model type %q is not supported (the built-in chat runs qwen3, qwen3_moe, qwen3_5, qwen3_5_moe)", modelType)
	}
	c := &Config{ModelType: modelType, RMSEps: 1e-6, RopeTheta: 1e6, PartialRotary: 1, NormTopK: modelType == "qwen3_5_moe"}
	for key, dst := range map[string]*int{"hidden_size": &c.Hidden, "num_hidden_layers": &c.Layers, "num_attention_heads": &c.Heads,
		"num_key_value_heads": &c.KVHeads, "head_dim": &c.HeadDim, "intermediate_size": &c.Intermediate, "vocab_size": &c.Vocab,
		"linear_num_key_heads": &c.LinearKHeads, "linear_num_value_heads": &c.LinearVHeads, "linear_key_head_dim": &c.LinearKDim,
		"linear_value_head_dim": &c.LinearVDim, "linear_conv_kernel_dim": &c.ConvKernel, "full_attention_interval": &c.FullAttnInterval,
		"num_experts": &c.Experts, "num_experts_per_tok": &c.ExpertsPerTok, "moe_intermediate_size": &c.MoEIntermediate,
		"shared_expert_intermediate_size": &c.SharedIntermediate, "decoder_sparse_step": &c.SparseStep} {
		get(key, dst)
	}
	var f float64
	if get("rms_norm_eps", &f) {
		c.RMSEps = float32(f)
	}
	if get("rope_theta", &f) {
		c.RopeTheta = float32(f)
	}
	if get("partial_rotary_factor", &f) {
		c.PartialRotary = float32(f)
	}
	var rope struct {
		RopeTheta     *float64 `json:"rope_theta"`
		PartialRotary *float64 `json:"partial_rotary_factor"`
		Type          string   `json:"type"`
		RopeType      string   `json:"rope_type"`
	}
	if get("rope_parameters", &rope) || get("rope_scaling", &rope) {
		if rope.RopeTheta != nil {
			c.RopeTheta = float32(*rope.RopeTheta)
		}
		if rope.PartialRotary != nil {
			c.PartialRotary = float32(*rope.PartialRotary)
		}
		if t := rope.Type + rope.RopeType; t != "" && t != "default" {
			return nil, fmt.Errorf("config.json: RoPE scaling %q is not supported", t)
		}
	}
	get("tie_word_embeddings", &c.TieEmbeddings)
	get("norm_topk_prob", &c.NormTopK)
	get("mlp_only_layers", &c.MLPOnlyLayers)
	get("attn_output_gate", &c.AttnGate)
	if strings.HasPrefix(modelType, "qwen3_5") {
		c.AttnGate = true // Qwen3.5's attention is always gated (mlx-lm's Qwen3NextAttention)
		if c.FullAttnInterval == 0 {
			c.FullAttnInterval = 4
		}
		if c.PartialRotary == 1 {
			if _, ok := text["rope_parameters"]; !ok {
				c.PartialRotary = 0.25
			}
		}
	} else {
		c.FullAttnInterval, c.LinearVHeads = 0, 0
	}
	if c.HeadDim == 0 && c.Heads > 0 {
		c.HeadDim = c.Hidden / c.Heads
	}
	if c.KVHeads == 0 {
		c.KVHeads = c.Heads
	}
	var eos any
	if get("eos_token_id", &eos) {
		switch v := eos.(type) {
		case float64:
			c.EOS = []int{int(v)}
		case []any:
			for _, x := range v {
				if n, ok := x.(float64); ok {
					c.EOS = append(c.EOS, int(n))
				}
			}
		}
	}
	// quantisation: {"group_size": 64, "bits": 4, "mode": "affine", "<path>": {"group_size": 64, "bits": 8}, …}
	qr, ok := top["quantization"]
	if !ok {
		qr, ok = top["quantization_config"]
	}
	if !ok {
		return nil, fmt.Errorf("config.json: not a quantised MLX checkpoint (no \"quantization\"): take an mlx-community 4-bit model")
	}
	var q map[string]json.RawMessage
	if err := json.Unmarshal(qr, &q); err != nil {
		return nil, fmt.Errorf("config.json quantization: %w", err)
	}
	c.QuantOverride = map[string][2]int{}
	for k, v := range q {
		switch k {
		case "group_size":
			_ = json.Unmarshal(v, &c.GroupSize)
		case "bits":
			_ = json.Unmarshal(v, &c.Bits)
		case "mode":
			var mode string
			_ = json.Unmarshal(v, &mode)
			if mode != "" && mode != "affine" {
				return nil, fmt.Errorf("config.json: quantisation mode %q is not supported (affine only)", mode)
			}
		default:
			var o struct {
				GroupSize int `json:"group_size"`
				Bits      int `json:"bits"`
			}
			if json.Unmarshal(v, &o) == nil && o.Bits > 0 {
				c.QuantOverride[k] = [2]int{o.GroupSize, o.Bits}
			}
		}
	}
	if c.GroupSize == 0 || c.Bits == 0 {
		return nil, fmt.Errorf("config.json: quantization without group_size/bits")
	}
	switch {
	case c.Hidden == 0 || c.Layers == 0 || c.Heads == 0 || c.Vocab == 0:
		return nil, fmt.Errorf("config.json: hidden_size, num_hidden_layers, num_attention_heads or vocab_size missing")
	case c.Hybrid() && (c.LinearKHeads == 0 || c.LinearKDim == 0 || c.LinearVDim == 0 || c.ConvKernel == 0 || c.LinearVHeads%c.LinearKHeads != 0):
		return nil, fmt.Errorf("config.json: incomplete linear-attention settings")
	case c.Experts > 0 && (c.ExpertsPerTok == 0 || c.MoEIntermediate == 0):
		return nil, fmt.Errorf("config.json: incomplete Mixture-of-Experts settings")
	case c.Heads%c.KVHeads != 0:
		return nil, fmt.Errorf("config.json: %d heads, %d key/value heads", c.Heads, c.KVHeads)
	}
	return c, nil
}

// ToolFormat is how the model writes tool calls: "json" (<tool_call>{"name":…,"arguments":…}</tool_call>, Qwen3) or
// "xml" (<tool_call><function=…><parameter=…>…</parameter></function></tool_call>, Qwen3.5/3.6).
func (c *Config) ToolFormat() string {
	if strings.HasPrefix(c.ModelType, "qwen3_5") {
		return "xml"
	}
	return "json"
}
