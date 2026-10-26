//go:build darwin && arm64

package llm

// The decoder on MLX, after mlx-lm's qwen3.py, qwen3_moe.py, qwen3_next.py, qwen3_5.py and gated_delta.py (MIT):
// RMSNorm → attention (full, with a KV cache; Qwen3.5: with a sigmoid output gate and RoPE on a quarter of the
// features) or a gated delta-net (a causal depthwise convolution, then a recurrent state per head updated by a
// custom Metal kernel) → RMSNorm → SwiGLU MLP or Mixture of Experts (top-k experts via gather_qmm, plus Qwen3.5's
// gated shared expert). Every matrix is 4-bit (8-bit for the routers) affine-quantised, as in the checkpoint.

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/philipparndt/ai-connector/internal/mlx"
)

type array = mlx.Array

// linear is a (quantised) linear layer.
type linear struct {
	w, scales, biases array
	gs, bits          int
}

func (l linear) apply(x array) array {
	if !l.scales.Valid() {
		return mlx.Matmul(x, mlx.Transpose(l.w, 1, 0))
	}
	return mlx.QuantizedMatmul(x, l.w, l.scales, l.biases, true, l.gs, l.bits)
}

type mlp struct{ gate, up, down linear }

func (m mlp) apply(x array) array {
	return m.down.apply(swiglu(m.gate.apply(x), m.up.apply(x)))
}

type moe struct {
	router             linear
	gate, up, down     linear // [experts, out, in] quantised
	shared             *mlp
	sharedGate         linear
	topK, experts      int
	normTopK           bool
	hasShared          bool
	sharedGateQuantize bool
}

type attention struct {
	q, k, v, o   linear
	qNorm, kNorm array
	gated        bool
}

type deltaNet struct {
	qkv, z, b, a, out linear
	conv              array // [convDim, K, 1]
	aLog, dtBias      array
	negA              array // −exp(A_log), float32 (computed once)
	norm              array
}

type layer struct {
	inNorm, postNorm array
	attn             *attention
	delta            *deltaNet
	mlp              *mlp
	moe              *moe
}

// model is a loaded checkpoint.
type model struct {
	cfg       *Config
	embed     linear // quantised embedding rows
	head      linear
	norm      array
	layers    []layer
	weights   map[string]array
	gdKernel  *mlx.Kernel
	convDim   int
	keyDim    int
	valueDim  int
	ropeDims  int
	attnScale float32

	qScale, kScale array // the gated delta-net's q/k scales as RMSNorm weights

	vision *visionTower // nil: text only
}

// loadModel maps the checkpoint's safetensors files (MLX loads them lazily) and wires the layers.
func loadModel(dir string, cfg *Config) (*model, error) {
	files, err := WeightFiles(dir)
	if err != nil {
		return nil, err
	}
	w := map[string]array{}
	for _, f := range files {
		m, err := mlx.LoadSafetensors(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		for k, v := range m {
			drop := strings.HasPrefix(k, "model.visual") || strings.Contains(k, "mtp.")
			if strings.HasPrefix(k, "vision_tower") && cfg.Vision == nil {
				drop = true
			}
			if drop {
				mlx.Free(v)
				continue
			}
			w[k] = v
		}
	}
	md := &model{cfg: cfg, weights: w}
	if err := md.wire(); err != nil {
		md.free()
		return nil, err
	}
	return md, nil
}

func (md *model) free() {
	for _, a := range md.weights {
		mlx.Free(a)
	}
	md.weights = nil
}

func (md *model) wire() (err error) {
	c := md.cfg
	prefix, head := "model.", "lm_head"
	if _, ok := md.weights["language_model.model.embed_tokens.weight"]; ok {
		prefix, head = "language_model.model.", "language_model.lm_head"
	}
	missing := []string{}
	get := func(name string) array {
		a, ok := md.weights[name]
		if !ok {
			missing = append(missing, name)
		}
		return a
	}
	lin := func(path string) linear {
		l := linear{w: get(path + ".weight")}
		if s, ok := md.weights[path+".scales"]; ok {
			l.scales, l.biases = s, md.weights[path+".biases"]
			l.gs, l.bits = c.Quant(path)
		}
		return l
	}
	md.embed = lin(prefix + "embed_tokens")
	if c.TieEmbeddings {
		md.head = md.embed
	} else {
		md.head = lin(head)
	}
	md.norm = get(prefix + "norm.weight")
	md.keyDim = c.LinearKDim * c.LinearKHeads
	md.valueDim = c.LinearVDim * c.LinearVHeads
	md.convDim = 2*md.keyDim + md.valueDim
	md.ropeDims = int(float32(c.HeadDim) * c.PartialRotary)
	md.attnScale = float32(1 / math.Sqrt(float64(c.HeadDim)))
	for i := 0; i < c.Layers; i++ {
		p := prefix + "layers." + strconv.Itoa(i) + "."
		l := layer{inNorm: get(p + "input_layernorm.weight"), postNorm: get(p + "post_attention_layernorm.weight")}
		if c.IsLinear(i) {
			q := p + "linear_attn."
			l.delta = &deltaNet{qkv: lin(q + "in_proj_qkv"), z: lin(q + "in_proj_z"), b: lin(q + "in_proj_b"), a: lin(q + "in_proj_a"),
				out: lin(q + "out_proj"), conv: get(q + "conv1d.weight"), aLog: get(q + "A_log"), dtBias: get(q + "dt_bias"), norm: get(q + "norm.weight")}
		} else {
			q := p + "self_attn."
			l.attn = &attention{q: lin(q + "q_proj"), k: lin(q + "k_proj"), v: lin(q + "v_proj"), o: lin(q + "o_proj"),
				qNorm: get(q + "q_norm.weight"), kNorm: get(q + "k_norm.weight"), gated: c.AttnGate}
		}
		if c.IsMoE(i) {
			q := p + "mlp."
			m := &moe{router: lin(q + "gate"), gate: lin(q + "switch_mlp.gate_proj"), up: lin(q + "switch_mlp.up_proj"),
				down: lin(q + "switch_mlp.down_proj"), topK: c.ExpertsPerTok, experts: c.Experts, normTopK: c.NormTopK}
			if _, ok := md.weights[q+"shared_expert.gate_proj.weight"]; ok {
				m.shared = &mlp{gate: lin(q + "shared_expert.gate_proj"), up: lin(q + "shared_expert.up_proj"), down: lin(q + "shared_expert.down_proj")}
				m.sharedGate = lin(q + "shared_expert_gate")
				m.hasShared = true
			}
			l.moe = m
		} else {
			q := p + "mlp."
			l.mlp = &mlp{gate: lin(q + "gate_proj"), up: lin(q + "up_proj"), down: lin(q + "down_proj")}
		}
		md.layers = append(md.layers, l)
	}
	if len(missing) > 0 {
		if len(missing) > 5 {
			missing = append(missing[:5], "…")
		}
		return fmt.Errorf("the checkpoint lacks %s", strings.Join(missing, ", "))
	}
	if fused == nil {
		fused = newFused()
	}
	md.vision = md.wireVision()
	if c.Hybrid() {
		// derived constants, kept with the weights (evaluated by materialize, freed with the model)
		dt := md.embed.scales.Dtype()
		keep := func(name string, a array) array {
			k := mlx.Keep(a)
			md.weights[name] = k
			return k
		}
		inv := 1 / math.Sqrt(float64(c.LinearKDim))
		qs, ks := make([]float32, c.LinearKDim), make([]float32, c.LinearKDim)
		for i := range qs {
			qs[i], ks[i] = float32(inv*inv), float32(inv)
		}
		md.qScale = keep("vboard.q_scale", mlx.AsType(mlx.FromFloat32(qs, len(qs)), dt))
		md.kScale = keep("vboard.k_scale", mlx.AsType(mlx.FromFloat32(ks, len(ks)), dt))
		for i := range md.layers {
			if d := md.layers[i].delta; d != nil {
				d.negA = keep(fmt.Sprintf("vboard.layers.%d.neg_a", i), mlx.Negative(mlx.Exp(mlx.AsType(d.aLog, mlx.Float32))))
			}
		}
		md.gdKernel = mlx.NewKernel("gated_delta_step", []string{"q", "k", "v", "g", "beta", "state_in", "T"},
			[]string{"y", "state_out"}, gatedDeltaSource)
	}
	return nil
}

// materialize loads every weight into memory (so the first answer does not wait for the disk) — the vision tower's
// only with the first image.
func (md *model) materialize() {
	all := make([]array, 0, len(md.weights))
	for k, a := range md.weights {
		if strings.HasPrefix(k, "vision_tower.") || strings.HasPrefix(k, "vboard.vision.") {
			continue
		}
		all = append(all, a)
		if len(all) == 256 {
			mlx.Eval(all...)
			all = all[:0]
		}
	}
	if len(all) > 0 {
		mlx.Eval(all...)
	}
}

// ---------- caches ----------

// layerCache is one layer's state: keys and values of the attention (grown in steps), or the gated delta-net's
// convolution window and recurrent state.
type layerCache struct {
	keys, values array // [1, kvHeads, capacity, headDim]
	conv, state  array // [1, K−1, convDim], [1, Hv, Dv, Dk] (float32)
}

// cache is the whole model's state after offset tokens; pos is the rotary position of the next token (offset, unless
// the prompt had images: an image's tokens advance it by its larger side only).
type cache struct {
	layers []layerCache
	offset int
	pos    int
}

const kvStep = 256

func (c *cache) free() {
	for _, l := range c.layers {
		mlx.Free(l.keys)
		mlx.Free(l.values)
		mlx.Free(l.conv)
		mlx.Free(l.state)
	}
	c.layers = nil
	c.offset, c.pos = 0, 0
}

// keep takes the step's new arrays out of the temporaries (releasing the ones they replace).
func (c *cache) keep(next []layerCache) {
	for i := range next {
		old := c.layers[i]
		n := next[i]
		for _, pair := range [][2]*array{{&old.keys, &n.keys}, {&old.values, &n.values}, {&old.conv, &n.conv}, {&old.state, &n.state}} {
			if pair[1].Valid() && *pair[0] != *pair[1] {
				k := mlx.Keep(*pair[1])
				mlx.Free(*pair[0])
				*pair[1] = k
			} else if !pair[1].Valid() {
				*pair[1] = *pair[0]
			}
		}
		c.layers[i] = n
	}
}

// snapshot copies the state (the prompt cache: a later conversation that starts with the same tokens continues from
// here). Arrays are immutable, so a copy is a second handle.
func (c *cache) snapshot() *cache {
	s := &cache{offset: c.offset, pos: c.pos, layers: make([]layerCache, len(c.layers))}
	for i, l := range c.layers {
		for _, pair := range [][2]*array{{&l.keys, &s.layers[i].keys}, {&l.values, &s.layers[i].values}, {&l.conv, &s.layers[i].conv}, {&l.state, &s.layers[i].state}} {
			if pair[0].Valid() {
				*pair[1] = mlx.Keep(*pair[0])
			}
		}
	}
	return s
}

// ---------- forward ----------

// multimodal is what a forward pass takes besides its tokens when the prompt has images: image features in place of
// the image tokens' embeddings, and every token's MRoPE position.
type multimodal struct {
	feats []placed
	pos   [][3]int // one per token of the step
	next  int      // the position after the step
}

// placed: rows from…to of an image's features replace the embeddings of the step's tokens at…
type placed struct {
	feats        array
	at, from, to int
}

// rope is how a step's queries and keys turn: by MLX's RoPE from offset on, or by per-token angles (cos, sin:
// [L, ropeDims/2], float32) when the step has image tokens.
type rope struct {
	offset   int
	cos, sin array
}

// forward runs tokens (after the cache's offset) through the model and returns the logits of the last position
// ([1, vocab], float32), updating the cache.
func (md *model) forward(ids array, c *cache) array { return md.forwardMM(ids, c, nil) }

// forwardMM is forward with images (mm may be nil).
func (md *model) forwardMM(ids array, c *cache, mm *multimodal) array {
	L := ids.Dim(1)
	e := md.embed
	h := mlx.Dequantize(mlx.Take(e.w, ids, 0), mlx.Take(e.scales, ids, 0), mlx.Take(e.biases, ids, 0), e.gs, e.bits)
	rp := rope{offset: c.pos}
	next := c.pos + L
	if mm != nil {
		for _, f := range mm.feats {
			rows := mlx.ExpandDims(mlx.AsType(mlx.SliceAxis(f.feats, 0, f.from, f.to), h.Dtype()), 0)
			h = mlx.SliceUpdate(h, rows, []int{0, f.at, 0}, []int{1, f.at + f.to - f.from, md.cfg.Hidden})
		}
		if p0, ok := textRun(mm.pos); ok {
			rp.offset = p0
		} else {
			cos, sin := mropeTables(mm.pos, md.ropeDims, float64(md.cfg.RopeTheta), md.cfg.MRoPESections)
			rp.cos, rp.sin = mlx.FromFloat32(cos, L, md.ropeDims/2), mlx.FromFloat32(sin, L, md.ropeDims/2)
		}
		next = mm.next
	}
	if len(c.layers) == 0 {
		c.layers = make([]layerCache, len(md.layers))
	}
	nextCache := make([]layerCache, len(md.layers))
	eps := md.cfg.RMSEps
	for i, l := range md.layers {
		x := mlx.RMSNorm(h, l.inNorm, eps)
		var r array
		if l.delta != nil {
			r = md.deltaNet(l.delta, x, c.layers[i], &nextCache[i])
		} else {
			r = md.attention(l.attn, x, c.layers[i], c.offset, rp, &nextCache[i])
		}
		h = mlx.Add(h, r)
		x = mlx.RMSNorm(h, l.postNorm, eps)
		if l.moe != nil {
			h = mlx.Add(h, md.moe(l.moe, x))
		} else {
			h = mlx.Add(h, l.mlp.apply(x))
		}
	}
	c.keep(nextCache)
	c.offset += L
	c.pos = next
	last := mlx.SliceAxis(h, 1, L-1, L)
	last = mlx.RMSNorm(last, md.norm, eps)
	logits := md.head.apply(last) // [1, 1, vocab]
	return mlx.AsType(mlx.Reshape(logits, 1, md.cfg.Vocab), mlx.Float32)
}

func (md *model) attention(a *attention, x array, lc layerCache, offset int, rp rope, out *layerCache) array {
	c := md.cfg
	L := x.Dim(1)
	D := c.HeadDim
	qp := a.q.apply(x)
	var q, gate array
	if a.gated {
		qp = mlx.Reshape(qp, 1, L, c.Heads, 2*D)
		q = mlx.SliceAxis(qp, 3, 0, D)
		gate = mlx.Reshape(mlx.SliceAxis(qp, 3, D, 2*D), 1, L, c.Heads*D)
	} else {
		q = mlx.Reshape(qp, 1, L, c.Heads, D)
	}
	k := mlx.Reshape(a.k.apply(x), 1, L, c.KVHeads, D)
	v := mlx.Reshape(a.v.apply(x), 1, L, c.KVHeads, D)
	q = mlx.Transpose(mlx.RMSNorm(q, a.qNorm, c.RMSEps), 0, 2, 1, 3)
	k = mlx.Transpose(mlx.RMSNorm(k, a.kNorm, c.RMSEps), 0, 2, 1, 3)
	v = mlx.Transpose(v, 0, 2, 1, 3)
	q, k = md.rotate(q, rp), md.rotate(k, rp)

	// the cache grows in steps of kvStep positions
	keys, values := lc.keys, lc.values
	need := offset + L
	if !keys.Valid() || keys.Dim(2) < need {
		capacity := (need + kvStep - 1) / kvStep * kvStep
		nk := mlx.Zeros(k.Dtype(), 1, c.KVHeads, capacity, D)
		nv := mlx.Zeros(v.Dtype(), 1, c.KVHeads, capacity, D)
		if keys.Valid() && offset > 0 {
			nk = mlx.SliceUpdate(nk, mlx.SliceAxis(keys, 2, 0, offset), []int{0, 0, 0, 0}, []int{1, c.KVHeads, offset, D})
			nv = mlx.SliceUpdate(nv, mlx.SliceAxis(values, 2, 0, offset), []int{0, 0, 0, 0}, []int{1, c.KVHeads, offset, D})
		}
		keys, values = nk, nv
	}
	keys = mlx.SliceUpdate(keys, k, []int{0, 0, offset, 0}, []int{1, c.KVHeads, need, D})
	values = mlx.SliceUpdate(values, v, []int{0, 0, offset, 0}, []int{1, c.KVHeads, need, D})
	out.keys, out.values = keys, values

	o := mlx.SDPA(q, mlx.SliceAxis(keys, 2, 0, need), mlx.SliceAxis(values, 2, 0, need), md.attnScale, L > 1)
	o = mlx.Reshape(mlx.Transpose(o, 0, 2, 1, 3), 1, L, c.Heads*D)
	if a.gated {
		o = fused.sigmoidMul.Call(o, gate)[0]
	}
	return a.o.apply(o)
}

// rotate turns the first ropeDims features of x ([1, heads, L, D]) by the step's positions; the angles of per-token
// positions rotate feature i with i + ropeDims/2, as MLX's RoPE does (not traditional).
func (md *model) rotate(x array, rp rope) array {
	if !rp.cos.Valid() {
		return mlx.RoPE(x, md.ropeDims, false, md.cfg.RopeTheta, 1, rp.offset)
	}
	dt := x.Dtype()
	half := md.ropeDims / 2
	xf := mlx.AsType(mlx.SliceAxis(x, -1, 0, md.ropeDims), mlx.Float32)
	x1, x2 := mlx.SliceAxis(xf, -1, 0, half), mlx.SliceAxis(xf, -1, half, md.ropeDims)
	r1 := mlx.Sub(mlx.Mul(x1, rp.cos), mlx.Mul(x2, rp.sin))
	r2 := mlx.Add(mlx.Mul(x2, rp.cos), mlx.Mul(x1, rp.sin))
	parts := []array{mlx.AsType(r1, dt), mlx.AsType(r2, dt)}
	if D := x.Dim(-1); D > md.ropeDims {
		parts = append(parts, mlx.SliceAxis(x, -1, md.ropeDims, D))
	}
	return mlx.Concatenate(parts, -1)
}

// textRun: the positions are text's — the same on all three axes and consecutive — and the first one.
func textRun(pos [][3]int) (int, bool) {
	for i, p := range pos {
		if p[0] != p[1] || p[0] != p[2] || p[0] != pos[0][0]+i {
			return 0, false
		}
	}
	return pos[0][0], true
}

func (md *model) deltaNet(d *deltaNet, x array, lc layerCache, out *layerCache) array {
	c := md.cfg
	S := x.Dim(1)
	K := c.ConvKernel
	qkv := d.qkv.apply(x)
	z := mlx.Reshape(d.z.apply(x), 1, S, c.LinearVHeads, c.LinearVDim)
	b := d.b.apply(x)
	a := d.a.apply(x)

	convState := lc.conv
	if !convState.Valid() {
		convState = mlx.Zeros(x.Dtype(), 1, K-1, md.convDim)
	}
	convIn := mlx.Concatenate([]array{convState, qkv}, 1)
	out.conv = mlx.Contiguous(mlx.SliceAxis(convIn, 1, S, S+K-1))
	conv := fused.silu.Call(mlx.Conv1d(convIn, d.conv, 1, 0, 1, md.convDim))[0]

	q := mlx.Reshape(mlx.SliceAxis(conv, 2, 0, md.keyDim), 1, S, c.LinearKHeads, c.LinearKDim)
	k := mlx.Reshape(mlx.SliceAxis(conv, 2, md.keyDim, 2*md.keyDim), 1, S, c.LinearKHeads, c.LinearKDim)
	v := mlx.Reshape(mlx.SliceAxis(conv, 2, 2*md.keyDim, md.convDim), 1, S, c.LinearVHeads, c.LinearVDim)
	// RMSNorm without weight, scaled by 1/Dk (q) and 1/√Dk (k): the scale as the norm's weight, one kernel each
	q = mlx.RMSNorm(q, md.qScale, 1e-6)
	k = mlx.RMSNorm(k, md.kScale, 1e-6)

	gb := fused.gates.Call(a, b, d.dtBias, d.negA)
	g, beta := gb[0], gb[1]
	state := lc.state
	if !state.Valid() {
		state = mlx.Zeros(mlx.Float32, 1, c.LinearVHeads, c.LinearVDim, c.LinearKDim)
	}
	res := md.gdKernel.Apply(mlx.KernelCall{
		Inputs: []array{q, k, v, g, beta, state, mlx.Int(S)},
		Template: []mlx.TemplateArg{{Name: "InT", Dtype: x.Dtype()}, {Name: "StT", Dtype: mlx.Float32},
			{Name: "Dk", Int: c.LinearKDim, IsInt: true}, {Name: "Dv", Int: c.LinearVDim, IsInt: true},
			{Name: "Hk", Int: c.LinearKHeads, IsInt: true}, {Name: "Hv", Int: c.LinearVHeads, IsInt: true}},
		Grid:      [3]int{32, c.LinearVDim, c.LinearVHeads},
		Group:     [3]int{32, 4, 1},
		OutShapes: [][]int{{1, S, c.LinearVHeads, c.LinearVDim}, {1, c.LinearVHeads, c.LinearVDim, c.LinearKDim}},
		OutDtypes: []mlx.Dtype{x.Dtype(), mlx.Float32},
	})
	y, st := res[0], res[1]
	out.state = st

	// RMSNorm gated by SiLU(z), in float32
	n := mlx.RMSNorm(y, d.norm, c.RMSEps)
	o := swiglu(z, n)
	return d.out.apply(mlx.Reshape(o, 1, S, md.valueDim))
}

func (md *model) moe(m *moe, x array) array {
	L := x.Dim(1)
	D := x.Dim(2)
	gates := mlx.Softmax(m.router.apply(x), -1, true)
	inds := mlx.SliceAxis(mlx.Argpartition(gates, m.experts-m.topK, -1), 2, m.experts-m.topK, m.experts) // [1, L, k]
	scores := mlx.TakeAlongAxis(gates, inds, -1)
	if m.normTopK {
		scores = mlx.Div(scores, mlx.Sum(scores, -1, true))
	}
	y := md.switchGLU(m, x, inds, L, D)                                                   // [1, L, k, D]
	y = mlx.Sum(mlx.Mul(y, mlx.ExpandDims(mlx.AsType(scores, y.Dtype()), -1)), -2, false) // [1, L, D]
	if m.hasShared {
		sh := m.shared.apply(x)
		sh = mlx.Mul(mlx.Sigmoid(m.sharedGate.apply(x)), sh)
		y = mlx.Add(y, sh)
	}
	return y
}

// switchGLU runs every token through its top-k experts' SwiGLU (gather_qmm picks the experts' matrices); many
// tokens (the prompt) are sorted by expert first, so each expert's matrix is read once.
func (md *model) switchGLU(m *moe, x, inds array, L, D int) array {
	k := m.topK
	xe := mlx.Reshape(x, 1, L, 1, 1, D)
	var none array
	if L*k < 64 {
		up := mlx.GatherQMM(xe, m.up.w, m.up.scales, m.up.biases, none, inds, true, m.up.gs, m.up.bits, false)
		gt := mlx.GatherQMM(xe, m.gate.w, m.gate.scales, m.gate.biases, none, inds, true, m.gate.gs, m.gate.bits, false)
		h := swiglu(gt, up)
		dn := mlx.GatherQMM(h, m.down.w, m.down.scales, m.down.biases, none, inds, true, m.down.gs, m.down.bits, false)
		return mlx.Reshape(dn, 1, L, k, D)
	}
	flat := mlx.Reshape(inds, L*k)
	order := mlx.Argsort(flat, 0)
	inv := mlx.Argsort(order, 0)
	xs := mlx.Take(mlx.Reshape(x, L, 1, D), mlx.FloorDiv(order, mlx.AsType(mlx.Int(k), order.Dtype())), 0) // [L·k, 1, D]
	idx := mlx.Take(flat, order, 0)
	up := mlx.GatherQMM(xs, m.up.w, m.up.scales, m.up.biases, none, idx, true, m.up.gs, m.up.bits, true)
	gt := mlx.GatherQMM(xs, m.gate.w, m.gate.scales, m.gate.biases, none, idx, true, m.gate.gs, m.gate.bits, true)
	h := swiglu(gt, up)
	dn := mlx.GatherQMM(h, m.down.w, m.down.scales, m.down.biases, none, idx, true, m.down.gs, m.down.bits, true) // [L·k, 1, D]
	dn = mlx.Take(dn, inv, 0)
	return mlx.Reshape(dn, 1, L, k, D)
}

// The element-wise chains, compiled (mx.compile, shapeless): one fused kernel each instead of up to six. Decoding a
// token launches a few thousand kernels, so this is a third of the time per token (mlx-lm compiles the same ones).
type fusedOps struct {
	swiglu, silu, sigmoidMul, gates *mlx.Compiled
}

var fused *fusedOps // made on the engine's thread (wire)

func newFused() *fusedOps {
	one := func(f func(in []array) array) *mlx.Compiled {
		return mlx.Compile(func(in []array) []array { return []array{f(in)} }, true)
	}
	return &fusedOps{
		// SiLU(gate)·up in float32, rounded once
		swiglu: one(func(in []array) array {
			return mlx.AsType(mlx.Mul(mlx.SiLU(mlx.AsType(in[0], mlx.Float32)), mlx.AsType(in[1], mlx.Float32)), in[0].Dtype())
		}),
		silu:       one(func(in []array) array { return mlx.SiLU(in[0]) }),
		sigmoidMul: one(func(in []array) array { return mlx.Mul(in[0], mlx.Sigmoid(in[1])) }),
		// the gated delta-net's decay g = exp(−exp(A_log)·softplus(a + dt_bias)) (negA = −exp(A_log), float32) and
		// write strength beta = sigmoid(b)
		gates: mlx.Compile(func(in []array) []array {
			a, b, dtBias, negA := in[0], in[1], in[2], in[3]
			x := mlx.Add(a, dtBias)
			sp := mlx.Logaddexp(x, mlx.AsType(mlx.Float(0), x.Dtype()))
			return []array{mlx.Exp(mlx.Mul(negA, sp)), mlx.Sigmoid(b)}
		}, true),
	}
}

func swiglu(gate, up array) array { return fused.swiglu.Call(gate, up)[0] }

// gatedDeltaSource is mlx-lm's gated delta-net step kernel (models/gated_delta.py, no mask, one gate per head):
// for every head and time step, decay the state, read what it holds for k, correct it towards v, read it out with q.
const gatedDeltaSource = `
        auto n = thread_position_in_grid.z;
        auto b_idx = n / Hv;
        auto hv_idx = n % Hv;
        auto hk_idx = hv_idx / (Hv / Hk);
        constexpr int n_per_t = Dk / 32;

        // q, k: [B, T, Hk, Dk]
        auto q_ = q + b_idx * T * Hk * Dk + hk_idx * Dk;
        auto k_ = k + b_idx * T * Hk * Dk + hk_idx * Dk;

        // v, y: [B, T, Hv, Dv]
        auto v_ = v + b_idx * T * Hv * Dv + hv_idx * Dv;
        y += b_idx * T * Hv * Dv + hv_idx * Dv;

        auto dk_idx = thread_position_in_threadgroup.x;
        auto dv_idx = thread_position_in_grid.y;

        // state_in, state_out: [B, Hv, Dv, Dk]
        auto i_state = state_in + (n * Dv + dv_idx) * Dk;
        auto o_state = state_out + (n * Dv + dv_idx) * Dk;

        float state[n_per_t];
        for (int i = 0; i < n_per_t; ++i) {
          auto s_idx = n_per_t * dk_idx + i;
          state[i] = static_cast<float>(i_state[s_idx]);
        }

        // g: [B, T, Hv]
        auto g_ = g + b_idx * T * Hv;
        auto beta_ = beta + b_idx * T * Hv;

        for (int t = 0; t < T; ++t) {
          {
            float kv_mem = 0.0f;
            for (int i = 0; i < n_per_t; ++i) {
              auto s_idx = n_per_t * dk_idx + i;
              state[i] = state[i] * g_[hv_idx];
              kv_mem += state[i] * k_[s_idx];
            }
            kv_mem = simd_sum(kv_mem);

            auto delta = (v_[dv_idx] - kv_mem) * beta_[hv_idx];

            float out = 0.0f;
            for (int i = 0; i < n_per_t; ++i) {
              auto s_idx = n_per_t * dk_idx + i;
              state[i] = state[i] + k_[s_idx] * delta;
              out += state[i] * q_[s_idx];
            }
            out = simd_sum(out);
            if (thread_index_in_simdgroup == 0) {
              y[dv_idx] = static_cast<InT>(out);
            }
          }
          // Increment data pointers to next time step
          q_ += Hk * Dk;
          k_ += Hk * Dk;
          v_ += Hv * Dv;
          y += Hv * Dv;
          g_ += Hv;
          beta_ += Hv;
        }
        for (int i = 0; i < n_per_t; ++i) {
          auto s_idx = n_per_t * dk_idx + i;
          o_state[s_idx] = static_cast<StT>(state[i]);
        }
`
