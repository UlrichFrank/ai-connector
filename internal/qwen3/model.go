// Package qwen3 runs a Qwen3 text embedding model (Qwen3-Embedding-0.6B) in Go from its own files (config.json,
// model.safetensors, tokenizer.json): the decoder with causal attention, the hidden state of the last token (the
// <|endoftext|> the tokenizer appends), normalised to length 1 — what tools/embed-server computes with transformers.
package qwen3

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/philipparndt/ai-connector/internal/tensor"
	"github.com/philipparndt/ai-connector/internal/tokenizer"
)

// MaxTokens is where a text is cut (embed_server.py: max_length=512, the end-of-text token included).
const MaxTokens = 512

type config struct {
	ModelType    string  `json:"model_type"`
	Hidden       int     `json:"hidden_size"`
	Intermediate int     `json:"intermediate_size"`
	Heads        int     `json:"num_attention_heads"`
	KVHeads      int     `json:"num_key_value_heads"`
	HeadDim      int     `json:"head_dim"`
	Layers       int     `json:"num_hidden_layers"`
	Vocab        int     `json:"vocab_size"`
	Eps          float32 `json:"rms_norm_eps"`
	Theta        float64 `json:"rope_theta"`
	Act          string  `json:"hidden_act"`
	AttnBias     bool    `json:"attention_bias"`
	RopeScaling  any     `json:"rope_scaling"`
}

type layer struct {
	inNorm, postNorm []float32
	q, k, v, o       []float32
	qNorm, kNorm     []float32
	gate, up, down   []float32
}

// Model is a loaded Qwen3 embedding model.
type Model struct {
	cfg    config
	tok    *tokenizer.Tokenizer
	emb    tensor.Half
	layers []layer
	norm   []float32
	rope   *tensor.Rope
	mu     sync.Mutex
}

// Load reads a model directory.
func Load(dir string) (*Model, error) {
	m := &Model{}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &m.cfg); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	c := &m.cfg
	switch {
	case c.ModelType != "qwen3":
		return nil, fmt.Errorf("embedding model %q not supported (Qwen3 only)", c.ModelType)
	case c.AttnBias || c.RopeScaling != nil || (c.Act != "" && c.Act != "silu"):
		return nil, fmt.Errorf("qwen3: this variant (bias, RoPE scaling or activation) is not supported")
	}
	if c.HeadDim == 0 {
		c.HeadDim = c.Hidden / c.Heads
	}
	if c.KVHeads == 0 {
		c.KVHeads = c.Heads
	}
	if m.tok, err = tokenizer.Load(filepath.Join(dir, "tokenizer.json")); err != nil {
		return nil, err
	}
	w, err := tensor.OpenSafetensors(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		return nil, err
	}
	defer w.Close()
	// checkpoints name the tensors with or without the "model." prefix
	p := ""
	if w.Has("model.embed_tokens.weight") {
		p = "model."
	}
	d, qd, kvd := c.Hidden, c.Heads*c.HeadDim, c.KVHeads*c.HeadDim
	f := func(name string, shape ...int) []float32 {
		if err != nil {
			return nil
		}
		var v []float32
		v, err = w.F32(p+name, shape...)
		return v
	}
	if m.emb, err = w.Half(p+"embed_tokens.weight", c.Vocab, d); err != nil {
		return nil, fmt.Errorf("qwen3: %w", err)
	}
	for i := 0; i < c.Layers; i++ {
		l := fmt.Sprintf("layers.%d.", i)
		m.layers = append(m.layers, layer{
			inNorm: f(l+"input_layernorm.weight", d), postNorm: f(l+"post_attention_layernorm.weight", d),
			q: f(l+"self_attn.q_proj.weight", qd, d), k: f(l+"self_attn.k_proj.weight", kvd, d),
			v: f(l+"self_attn.v_proj.weight", kvd, d), o: f(l+"self_attn.o_proj.weight", d, qd),
			qNorm: f(l+"self_attn.q_norm.weight", c.HeadDim), kNorm: f(l+"self_attn.k_norm.weight", c.HeadDim),
			gate: f(l+"mlp.gate_proj.weight", c.Intermediate, d), up: f(l+"mlp.up_proj.weight", c.Intermediate, d),
			down: f(l+"mlp.down_proj.weight", d, c.Intermediate),
		})
	}
	m.norm = f("norm.weight", d)
	if err != nil {
		return nil, fmt.Errorf("qwen3: %w", err)
	}
	m.rope = tensor.NewRope(c.HeadDim, c.Theta, MaxTokens)
	return m, nil
}

// Embed returns one unit vector per text, batch texts per forward pass.
func (m *Model) Embed(texts []string, batch int) [][]float32 {
	if batch <= 0 {
		batch = 32
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]float32, len(texts))
	tensor.Batches(len(texts), batch, func(lo, hi int) {
		var seqs [][]int
		for _, t := range texts[lo:hi] {
			seqs = append(seqs, m.tok.EncodeSpecial(t, MaxTokens))
		}
		copy(out[lo:hi], m.forward(seqs))
	})
	return out
}

// Dim is the length of the vectors.
func (m *Model) Dim() int { return m.cfg.Hidden }

func (m *Model) forward(seqs [][]int) [][]float32 {
	c := m.cfg
	d, hd := c.Hidden, c.HeadDim
	qd, kvd := c.Heads*hd, c.KVHeads*hd
	total := 0
	for _, s := range seqs {
		total += len(s)
	}
	x := make([]float32, total*d)
	o := 0
	for _, s := range seqs {
		for _, id := range s {
			m.emb.Row(id, d, x[o*d:(o+1)*d])
			o++
		}
	}
	causal := func(i, j int) bool { return j <= i }
	// the activations the matrix products read and write: scratch memory (the GPU's, when there is one), per layer
	var keep tensor.Scratch
	defer keep.Free()
	h := keep.Alloc(total * d)
	for _, l := range m.layers {
		var s tensor.Scratch
		copy(h, x)
		tensor.RMSNorm(h, d, l.inNorm, c.Eps)
		qkv := s.LinearMany(h, total, d, tensor.Proj{W: l.q, Out: qd}, tensor.Proj{W: l.k, Out: kvd}, tensor.Proj{W: l.v, Out: kvd})
		q, k, v := qkv[0], qkv[1], qkv[2]
		tensor.RMSNorm(q, hd, l.qNorm, c.Eps) // per head
		tensor.RMSNorm(k, hd, l.kNorm, c.Eps)
		att := s.Alloc(total * qd) // every row is written below
		off := 0
		for _, s := range seqs {
			n := len(s)
			tensor.Parallel(n, 16, func(lo, hi int) {
				for i := lo; i < hi; i++ {
					for hh := 0; hh < c.Heads; hh++ {
						m.rope.Apply(q[(off+i)*qd+hh*hd:(off+i)*qd+(hh+1)*hd], i)
					}
					for hh := 0; hh < c.KVHeads; hh++ {
						m.rope.Apply(k[(off+i)*kvd+hh*hd:(off+i)*kvd+(hh+1)*hd], i)
					}
				}
			})
			a := tensor.Attention(q[off*qd:], qd, k[off*kvd:], kvd, v[off*kvd:], kvd, n, c.Heads, c.KVHeads, hd, causal)
			copy(att[off*qd:], a)
			off += n
		}
		tensor.Add(x, s.Linear(att, total, qd, l.o, nil, d))

		copy(h, x)
		tensor.RMSNorm(h, d, l.postNorm, c.Eps)
		gu := s.LinearMany(h, total, d, tensor.Proj{W: l.gate, Out: c.Intermediate}, tensor.Proj{W: l.up, Out: c.Intermediate})
		g, u := gu[0], gu[1]
		tensor.Parallel(len(g), 1<<14, func(lo, hi int) {
			for i := lo; i < hi; i++ {
				g[i] = tensor.SiLU(g[i]) * u[i]
			}
		})
		tensor.Add(x, s.Linear(g, total, c.Intermediate, l.down, nil, d))
		s.Free()
	}
	out := make([][]float32, len(seqs))
	off := 0
	for i, s := range seqs {
		off += len(s)
		last := append([]float32(nil), x[(off-1)*d:off*d]...)
		tensor.RMSNorm(last, d, m.norm, c.Eps)
		tensor.Normalize(last)
		out[i] = last
	}
	return out
}
