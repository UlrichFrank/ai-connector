// Package laya runs the Laya decision model (github.com/NandhaKishorM/laya, Apache-2.0) in Go: a ModernBERT encoder
// plus Laya's decision head, read from the checkpoint's own files (model.safetensors, encoder/config.json,
// tokenizer/tokenizer.json, rl_agent_config.json) — the same files the Python package downloads, no conversion.
//
// The forward pass follows laya/common.py (DecisionModel, build_sequence) and transformers' ModernBERT: the tokens of
// all sequences are stacked into one matrix for the linear layers (no padding), attention runs per sequence. Numbers
// are float32 throughout, like the Python package on the CPU.
package laya

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/philipparndt/ai-connector/internal/tensor"
	"github.com/philipparndt/ai-connector/internal/tokenizer"
)

// encoderConfig is the part of encoder/config.json (ModernBERT) the forward pass needs.
type encoderConfig struct {
	ModelType    string   `json:"model_type"`
	Hidden       int      `json:"hidden_size"`
	Intermediate int      `json:"intermediate_size"`
	Heads        int      `json:"num_attention_heads"`
	Layers       int      `json:"num_hidden_layers"`
	Vocab        int      `json:"vocab_size"`
	LocalWindow  int      `json:"local_attention"`
	GlobalEvery  int      `json:"global_attn_every_n_layers"`
	LayerTypes   []string `json:"layer_types"`
	NormEps      float32  `json:"norm_eps"`
	NormBias     bool     `json:"norm_bias"`
	AttnBias     bool     `json:"attention_bias"`
	MLPBias      bool     `json:"mlp_bias"`
	Activation   string   `json:"hidden_activation"`
	GlobalTheta  float64  `json:"global_rope_theta"`
	LocalTheta   float64  `json:"local_rope_theta"`
	RopeParams   map[string]struct {
		Theta float64 `json:"rope_theta"`
	} `json:"rope_parameters"`
}

// agentConfig is rl_agent_config.json.
type agentConfig struct {
	ModelName     string             `json:"model_name"`
	HeadLayers    int                `json:"head_layers"`
	MaxLen        int                `json:"max_len"`
	HeadMaxLen    int                `json:"head_max_len"`
	Temperature   []float64          `json:"temperature"`
	TempByOptions map[string]float64 `json:"temperature_by_options"`
}

type encLayer struct {
	global   bool
	attnNorm []float32 // nil in layer 0 (Identity)
	wqkv, wo []float32
	mlpNorm  []float32
	wi, wo2  []float32
	rope     *tensor.Rope
}

type headLayer struct {
	norm1W, norm1B, inW, inB, outW, outB []float32
	norm2W, norm2B, l1W, l1B, l2W, l2B   []float32
}

// Model is a loaded Laya checkpoint.
type Model struct {
	Name string
	enc  encoderConfig
	cfg  agentConfig
	tok  *tokenizer.Tokenizer

	cls, sep, mask, pad int

	emb       tensor.Half
	embNorm   []float32
	layers    []encLayer
	finalNorm []float32
	head      []headLayer
	typeEmb   []float32
	scLnW     []float32
	scLnB     []float32
	sc1W      []float32
	sc1B      []float32
	sc2W      []float32
	sc2B      []float32

	mu sync.Mutex // one forward pass at a time (each already uses every core)
}

// Load reads a checkpoint directory (the subfolder, e.g. …/typed-decisions).
func Load(dir string) (*Model, error) {
	m := &Model{}
	if err := readJSON(filepath.Join(dir, "rl_agent_config.json"), &m.cfg); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(dir, "encoder", "config.json"), &m.enc); err != nil {
		return nil, err
	}
	e := &m.enc
	switch {
	case e.ModelType != "modernbert":
		return nil, fmt.Errorf("laya: encoder %q not supported (ModernBERT only)", e.ModelType)
	case e.NormBias || e.AttnBias || e.MLPBias:
		return nil, fmt.Errorf("laya: encoder with biases not supported")
	case e.Activation != "" && e.Activation != "gelu":
		return nil, fmt.Errorf("laya: encoder activation %q not supported", e.Activation)
	case e.Hidden%e.Heads != 0:
		return nil, fmt.Errorf("laya: hidden size %d not divisible by %d heads", e.Hidden, e.Heads)
	}
	if e.NormEps == 0 {
		e.NormEps = 1e-5
	}
	// transformers 5 stores the RoPE bases per layer type (laya/common.py _apply_rope_config)
	if p, ok := e.RopeParams["full_attention"]; ok && p.Theta > 0 {
		e.GlobalTheta = p.Theta
	}
	if p, ok := e.RopeParams["sliding_attention"]; ok && p.Theta > 0 {
		e.LocalTheta = p.Theta
	}
	if e.GlobalTheta == 0 {
		e.GlobalTheta = 160000
	}
	if e.LocalTheta == 0 {
		e.LocalTheta = 10000
	}
	if m.cfg.MaxLen == 0 {
		m.cfg.MaxLen = 512
	}
	if m.cfg.HeadMaxLen == 0 {
		m.cfg.HeadMaxLen = 192
	}
	m.Name = m.cfg.ModelName

	tok, err := tokenizer.Load(filepath.Join(dir, "tokenizer", "tokenizer.json"))
	if err != nil {
		return nil, err
	}
	m.tok = tok
	for name, dst := range map[string]*int{"[CLS]": &m.cls, "[SEP]": &m.sep, "[MASK]": &m.mask, "[PAD]": &m.pad} {
		id, ok := tok.ID(name)
		if !ok {
			return nil, fmt.Errorf("laya: tokenizer has no %s", name)
		}
		*dst = id
	}

	w, err := tensor.OpenSafetensors(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		return nil, err
	}
	defer w.Close()
	if err := m.loadWeights(w); err != nil {
		return nil, fmt.Errorf("laya: %w", err)
	}
	return m, nil
}

func (m *Model) loadWeights(w *tensor.Weights) error {
	d, in, e := m.enc.Hidden, m.enc.Intermediate, &m.enc
	var err error
	f := func(name string, shape ...int) []float32 {
		if err != nil {
			return nil
		}
		var v []float32
		v, err = w.F32(name, shape...)
		return v
	}
	if m.emb, err = w.Half("encoder.embeddings.tok_embeddings.weight", e.Vocab, d); err != nil {
		return err
	}
	m.embNorm = f("encoder.embeddings.norm.weight", d)
	globalRope := tensor.NewRope(d/e.Heads, e.GlobalTheta, m.cfg.MaxLen)
	localRope := tensor.NewRope(d/e.Heads, e.LocalTheta, m.cfg.MaxLen)
	for i := 0; i < e.Layers; i++ {
		p := fmt.Sprintf("encoder.layers.%d.", i)
		l := encLayer{global: m.globalLayer(i)}
		if i > 0 {
			l.attnNorm = f(p+"attn_norm.weight", d)
		}
		l.wqkv = f(p+"attn.Wqkv.weight", 3*d, d)
		l.wo = f(p+"attn.Wo.weight", d, d)
		l.mlpNorm = f(p+"mlp_norm.weight", d)
		l.wi = f(p+"mlp.Wi.weight", 2*in, d)
		l.wo2 = f(p+"mlp.Wo.weight", d, in)
		l.rope = localRope
		if l.global {
			l.rope = globalRope
		}
		m.layers = append(m.layers, l)
	}
	m.finalNorm = f("encoder.final_norm.weight", d)
	for i := 0; i < m.cfg.HeadLayers; i++ {
		p := fmt.Sprintf("head.layers.%d.", i)
		m.head = append(m.head, headLayer{
			norm1W: f(p+"norm1.weight", d), norm1B: f(p+"norm1.bias", d),
			inW: f(p+"self_attn.in_proj_weight", 3*d, d), inB: f(p+"self_attn.in_proj_bias", 3*d),
			outW: f(p+"self_attn.out_proj.weight", d, d), outB: f(p+"self_attn.out_proj.bias", d),
			norm2W: f(p+"norm2.weight", d), norm2B: f(p+"norm2.bias", d),
			l1W: f(p+"linear1.weight", 4*d, d), l1B: f(p+"linear1.bias", 4*d),
			l2W: f(p+"linear2.weight", d, 4*d), l2B: f(p+"linear2.bias", d),
		})
	}
	m.typeEmb = f("type_emb.weight", 3, d)
	m.scLnW, m.scLnB = f("scorer.0.weight", d), f("scorer.0.bias", d)
	m.sc1W, m.sc1B = f("scorer.1.weight", d, d), f("scorer.1.bias", d)
	m.sc2W, m.sc2B = f("scorer.3.weight", 1, d), f("scorer.3.bias", 1)
	return err
}

// globalLayer: full attention every GlobalEvery layers (or as layer_types says), else the sliding window.
func (m *Model) globalLayer(i int) bool {
	if i < len(m.enc.LayerTypes) {
		return m.enc.LayerTypes[i] == "full_attention"
	}
	every := m.enc.GlobalEvery
	if every <= 0 {
		every = 3
	}
	return i%every == 0
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// seq is one encoded question row: token ids, the option markers' positions and the question type.
type seq struct {
	ids     []int
	markers []int
	qtype   int
}

// forward runs the model on several sequences and returns every sequence's marker logits (one per option).
func (m *Model) forward(seqs []seq) [][]float32 {
	d, heads := m.enc.Hidden, m.enc.Heads
	hd := d / heads
	total := 0
	for _, s := range seqs {
		total += len(s.ids)
	}
	x := make([]float32, total*d)
	o := 0
	for _, s := range seqs {
		for _, id := range s.ids {
			m.emb.Row(id, d, x[o*d:(o+1)*d])
			o++
		}
	}
	tensor.LayerNorm(x, d, m.embNorm, nil, m.enc.NormEps)
	half := m.enc.LocalWindow / 2
	local := func(i, j int) bool { return j-i <= half && i-j <= half }
	// the activations the matrix products read and write: scratch memory (the GPU's, when there is one), per layer
	var keep tensor.Scratch
	defer keep.Free()
	h := keep.Alloc(total * d)
	for _, l := range m.layers {
		var s tensor.Scratch
		copy(h, x)
		if l.attnNorm != nil {
			tensor.LayerNorm(h, d, l.attnNorm, nil, m.enc.NormEps)
		}
		qkv := s.Linear(h, total, d, l.wqkv, nil, 3*d)
		att := s.Alloc(total * d) // every row is written below
		allow := local
		if l.global {
			allow = nil
		}
		m.perSeq(seqs, func(off, n int) {
			base := qkv[off*3*d:]
			tensor.Parallel(n, 16, func(lo, hi int) {
				for i := lo; i < hi; i++ {
					for hh := 0; hh < heads; hh++ {
						l.rope.Apply(base[i*3*d+hh*hd:i*3*d+(hh+1)*hd], i)
						l.rope.Apply(base[i*3*d+d+hh*hd:i*3*d+d+(hh+1)*hd], i)
					}
				}
			})
			a := tensor.Attention(base, 3*d, base[d:], 3*d, base[2*d:], 3*d, n, heads, heads, hd, allow)
			copy(att[off*d:], a)
		})
		tensor.Add(x, s.Linear(att, total, d, l.wo, nil, d))

		copy(h, x)
		tensor.LayerNorm(h, d, l.mlpNorm, nil, m.enc.NormEps)
		in := m.enc.Intermediate
		wi := s.Linear(h, total, d, l.wi, nil, 2*in)
		act := s.Alloc(total * in) // written in full below
		tensor.Parallel(total, 4, func(lo, hi int) {
			for r := lo; r < hi; r++ {
				row := wi[r*2*in : (r+1)*2*in]
				out := act[r*in : (r+1)*in]
				for j := 0; j < in; j++ {
					out[j] = tensor.GELU(row[j]) * row[in+j]
				}
			}
		})
		tensor.Add(x, s.Linear(act, total, in, l.wo2, nil, d))
		s.Free()
	}
	tensor.LayerNorm(x, d, m.finalNorm, nil, m.enc.NormEps)

	// Laya's decision head: the question type's embedding, then post-norm-first transformer layers (PyTorch's
	// nn.TransformerEncoderLayer, ReLU), then the scorer on the option markers
	o = 0
	for _, s := range seqs {
		te := m.typeEmb[s.qtype*d : (s.qtype+1)*d]
		for range s.ids {
			tensor.Add(x[o*d:(o+1)*d], te)
			o++
		}
	}
	for _, l := range m.head {
		var s tensor.Scratch
		copy(h, x)
		tensor.LayerNorm(h, d, l.norm1W, l.norm1B, 1e-5)
		qkv := s.Linear(h, total, d, l.inW, l.inB, 3*d)
		att := s.Alloc(total * d)
		m.perSeq(seqs, func(off, n int) {
			base := qkv[off*3*d:]
			copy(att[off*d:], tensor.Attention(base, 3*d, base[d:], 3*d, base[2*d:], 3*d, n, heads, heads, hd, nil))
		})
		tensor.Add(x, s.Linear(att, total, d, l.outW, l.outB, d))
		copy(h, x)
		tensor.LayerNorm(h, d, l.norm2W, l.norm2B, 1e-5)
		ff := s.Linear(h, total, d, l.l1W, l.l1B, 4*d)
		tensor.Parallel(len(ff), 1<<16, func(lo, hi int) {
			for i := lo; i < hi; i++ {
				ff[i] = max(ff[i], 0)
			}
		})
		tensor.Add(x, s.Linear(ff, total, 4*d, l.l2W, l.l2B, d))
		s.Free()
	}

	// gather the markers of every sequence and score them together
	var rows []float32
	off := 0
	for _, s := range seqs {
		for _, p := range s.markers {
			rows = append(rows, x[(off+p)*d:(off+p+1)*d]...)
		}
		off += len(s.ids)
	}
	nm := len(rows) / d
	tensor.LayerNorm(rows, d, m.scLnW, m.scLnB, 1e-5)
	hid := tensor.Linear(rows, nm, d, m.sc1W, m.sc1B, d)
	for i, v := range hid {
		hid[i] = tensor.GELU(v)
	}
	sc := tensor.Linear(hid, nm, d, m.sc2W, m.sc2B, 1)
	out := make([][]float32, len(seqs))
	k := 0
	for i, s := range seqs {
		out[i] = sc[k : k+len(s.markers)]
		k += len(s.markers)
	}
	return out
}

// perSeq calls fn for every sequence's token range (offset, length).
func (m *Model) perSeq(seqs []seq, fn func(off, n int)) {
	off := 0
	for _, s := range seqs {
		fn(off, len(s.ids))
		off += len(s.ids)
	}
}

// temperature is the calibration temperature of a question (laya/common.py temp_bucket, clamp_temperature).
func (m *Model) temperature(qtype, k int) float64 {
	size := "11+"
	switch {
	case k <= 2:
		size = "2"
	case k <= 5:
		size = "3-5"
	case k <= 10:
		size = "6-10"
	}
	name := [...]string{"choice", "score", "noul"}[qtype]
	t, ok := m.cfg.TempByOptions[name+":"+size]
	if !ok {
		t = 1
		if qtype < len(m.cfg.Temperature) {
			t = m.cfg.Temperature[qtype]
		}
	}
	if math.IsNaN(t) || math.IsInf(t, 0) {
		return 1
	}
	return min(5, max(0.5, t))
}
