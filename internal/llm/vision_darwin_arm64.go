//go:build darwin && arm64

package llm

// The vision tower on MLX, after mlx-vlm's qwen3_vl/vision.py (MIT; Qwen3.5/3.6 use it unchanged): the patches through
// the patch embedding (a Conv3d whose stride is its kernel: a matrix product, the two identical frames of an image
// summed into the weight), plus learned position embeddings interpolated to the grid, then a ViT — LayerNorm,
// attention with 2D rotary positions (row and column), LayerNorm, GELU MLP — and the merger, which folds 2×2 patches
// into one token of the decoder's width. The tower runs in float32: its residual stream has a few patches with
// activations in the thousands, and in bf16 (the checkpoint's type, as mlx-vlm runs it) some of them come out wrong —
// the merged tokens were 8 % off float32's in mlx-vlm and 13 % in a bf16 port of it, a few tokens entirely. The weights
// are read from disk and widened on the first image (a chat without images does not load them): ~1.3 GB for
// Qwen3.5-4B/9B's tower.

import (
	"math"
	"strconv"
	"strings"

	"github.com/philipparndt/ai-connector/internal/mlx"
)

// dense is a linear layer with a bias (not quantised).
type dense struct{ w, b array }

func (d dense) apply(x array) array {
	return mlx.Add(mlx.Matmul(x, mlx.Transpose(d.w, 1, 0)), d.b)
}

type visionBlock struct {
	norm1w, norm1b, norm2w, norm2b array
	qkv, proj, fc1, fc2            dense
}

type visionTower struct {
	cfg              *VisionConfig
	patch            dense // [hidden, P·P·C]: the patch embedding with its frames summed
	pos              array // [NumPositions, hidden]
	side             int   // the position embeddings' grid side
	blocks           []visionBlock
	mergeNw, mergeNb array
	merge1, merge2   dense
	gelu, geluTanh   *mlx.Compiled
}

// wireVision takes the vision tower's weights (nil when the checkpoint has none).
func (md *model) wireVision() *visionTower {
	v := md.cfg.Vision
	if v == nil {
		return nil
	}
	if _, ok := md.weights["vision_tower.patch_embed.proj.weight"]; !ok {
		return nil
	}
	// float32 copies in place of the checkpoint's arrays (computed on first use; the bf16 data is let go then)
	for k, a := range md.weights {
		if strings.HasPrefix(k, "vision_tower.") {
			f := mlx.Keep(mlx.AsType(a, mlx.Float32))
			mlx.Free(a)
			md.weights[k] = f
		}
	}
	w := md.weights
	d := func(p string) dense { return dense{w[p+".weight"], w[p+".bias"]} }
	for _, k := range []string{"vision_tower.pos_embed.weight", "vision_tower.merger.norm.weight", "vision_tower.merger.linear_fc2.weight",
		"vision_tower.blocks." + strconv.Itoa(v.Depth-1) + ".mlp.linear_fc2.weight"} {
		if _, ok := w[k]; !ok {
			return nil
		}
	}
	t := &visionTower{cfg: v, pos: w["vision_tower.pos_embed.weight"], mergeNw: w["vision_tower.merger.norm.weight"],
		mergeNb: w["vision_tower.merger.norm.bias"], merge1: d("vision_tower.merger.linear_fc1"), merge2: d("vision_tower.merger.linear_fc2")}
	for t.side*t.side < v.NumPositions {
		t.side++
	}
	// [hidden, T, P, P, C] → the frames summed → [hidden, P·P·C] (an image is T identical frames)
	pw := w["vision_tower.patch_embed.proj.weight"]
	P := v.PatchSize
	t.patch = dense{md.keepDerived("vboard.vision.patch", mlx.Reshape(mlx.Sum(pw, 1, false), v.Hidden, P*P*v.InChannels)),
		w["vision_tower.patch_embed.proj.bias"]}
	for i := 0; i < v.Depth; i++ {
		p := "vision_tower.blocks." + strconv.Itoa(i) + "."
		t.blocks = append(t.blocks, visionBlock{norm1w: w[p+"norm1.weight"], norm1b: w[p+"norm1.bias"], norm2w: w[p+"norm2.weight"],
			norm2b: w[p+"norm2.bias"], qkv: d(p + "attn.qkv"), proj: d(p + "attn.proj"), fc1: d(p + "mlp.linear_fc1"), fc2: d(p + "mlp.linear_fc2")})
	}
	// GELU (the merger: erf) and its tanh approximation (the blocks' MLP)
	t.gelu = mlx.Compile(func(in []array) []array {
		x := in[0]
		return []array{mlx.Mul(mlx.Mul(x, mlx.Float(0.5)), mlx.Add(mlx.Float(1), mlx.Erf(mlx.Mul(x, mlx.Float(float32(1/math.Sqrt2))))))}
	}, true)
	t.geluTanh = mlx.Compile(func(in []array) []array {
		// 0.5·x·(1 + tanh(z)) = x·sigmoid(2z), z = √(2/π)·(x + 0.044715·x³)
		x := in[0]
		z := mlx.Mul(mlx.Add(x, mlx.Mul(mlx.Float(0.044715), mlx.Mul(x, mlx.Square(x)))), mlx.Float(float32(2*math.Sqrt(2/math.Pi))))
		return []array{mlx.Mul(x, mlx.Sigmoid(z))}
	}, true)
	return t
}

// visionTrace sees the tower's intermediate results (tests).
var visionTrace = func(string, array) {}

// keepDerived keeps an array computed from the weights with them (evaluated on first use, freed with the model).
func (md *model) keepDerived(name string, a array) array {
	k := mlx.Keep(a)
	md.weights[name] = k
	return k
}

// encode runs one image through the tower: its tokens' features [Tokens, OutHidden] in the decoder's dtype. It
// releases every temporary array (mlx.FreeTemps) after each block: a full-HD image's blocks leave ~1.2 GB each.
func (t *visionTower) encode(px *Pixels, dtype mlx.Dtype) array {
	v := t.cfg
	N := px.GridH * px.GridW
	x := t.patch.apply(mlx.FromFloat32(px.Patches, N, len(px.Patches)/N))
	visionTrace("patch", x)
	x = mlx.Add(x, t.posEmbed(px))
	visionTrace("pos", x)
	cos, sin := t.rotary(px)
	// what outlives a block: the residual stream and the rotary tables
	kept := []array{mlx.Keep(x), mlx.Keep(cos), mlx.Keep(sin)}
	defer func() {
		for _, a := range kept {
			mlx.Free(a)
		}
	}()
	x, cos, sin = kept[0], kept[1], kept[2]
	mlx.Eval(kept...)
	mlx.FreeTemps()
	hd := v.Hidden / v.Heads
	scale := float32(1 / math.Sqrt(float64(hd)))
	for _, b := range t.blocks {
		h := mlx.LayerNorm(x, b.norm1w, b.norm1b, 1e-6)
		qkv := mlx.Reshape(b.qkv.apply(h), N, 3, v.Heads, hd)
		q := rotateHalf(mlx.Squeeze(mlx.SliceAxis(qkv, 1, 0, 1), 1), cos, sin) // [N, heads, hd]
		k := rotateHalf(mlx.Squeeze(mlx.SliceAxis(qkv, 1, 1, 2), 1), cos, sin)
		vv := mlx.Squeeze(mlx.SliceAxis(qkv, 1, 2, 3), 1)
		q = mlx.ExpandDims(mlx.Transpose(q, 1, 0, 2), 0) // [1, heads, N, hd]
		k = mlx.ExpandDims(mlx.Transpose(k, 1, 0, 2), 0)
		vv = mlx.ExpandDims(mlx.Transpose(vv, 1, 0, 2), 0)
		o := mlx.SDPA(q, k, vv, scale, false)
		o = mlx.Reshape(mlx.Transpose(o, 0, 2, 1, 3), N, v.Hidden)
		x = mlx.Add(x, b.proj.apply(o))
		h = mlx.LayerNorm(x, b.norm2w, b.norm2b, 1e-6)
		x = mlx.Add(x, b.fc2.apply(t.geluTanh.Call(b.fc1.apply(h))[0]))
		visionTrace("block", x)
		mlx.Eval(x)
		next := mlx.Keep(x)
		mlx.Free(kept[0])
		kept[0], x = next, next
		mlx.FreeTemps()
	}
	M2 := v.MergeSize * v.MergeSize
	x = mlx.LayerNorm(x, t.mergeNw, t.mergeNb, 1e-6)
	x = mlx.Reshape(x, N/M2, v.Hidden*M2)
	x = t.merge2.apply(t.gelu.Call(t.merge1.apply(x))[0])
	visionTrace("merged", x)
	x = mlx.AsType(x, dtype)
	mlx.Eval(x) // before the kept arrays it reads are let go
	return x
}

// rotateHalf is x·cos + rotate_half(x)·sin over each head's features (cos, sin: [N, 1, hd]).
func rotateHalf(x, cos, sin array) array {
	half := x.Dim(-1) / 2
	x1, x2 := mlx.SliceAxis(x, -1, 0, half), mlx.SliceAxis(x, -1, half, 2*half)
	rot := mlx.Concatenate([]array{mlx.Negative(x2), x1}, -1)
	return mlx.Add(mlx.Mul(x, cos), mlx.Mul(rot, sin))
}

// mergeOrder calls f for each patch in the order of Pixels.Patches with its row and column in the grid.
func mergeOrder(px *Pixels, m int, f func(i, row, col int)) {
	i := 0
	for bh := 0; bh < px.GridH/m; bh++ {
		for bw := 0; bw < px.GridW/m; bw++ {
			for mh := 0; mh < m; mh++ {
				for mw := 0; mw < m; mw++ {
					f(i, bh*m+mh, bw*m+mw)
					i++
				}
			}
		}
	}
}

// posEmbed is the learned position embeddings bilinearly interpolated from their square grid to the image's
// (fast_pos_embed_interpolate), in patch order.
func (t *visionTower) posEmbed(px *Pixels) array {
	N := px.GridH * px.GridW
	idx := make([]int32, 4*N)
	wts := make([]float32, 4*N)
	s := t.side
	lin := func(i, n int) float64 { // linspace(0, s−1, n)[i]
		if n == 1 {
			return 0
		}
		return float64(i) * float64(s-1) / float64(n-1)
	}
	mergeOrder(px, t.cfg.MergeSize, func(i, row, col int) {
		h, w := lin(row, px.GridH), lin(col, px.GridW)
		hf, wf := int(float32(h)), int(float32(w))
		hc, wc := min(hf+1, s-1), min(wf+1, s-1)
		dh, dw := float32(h)-float32(hf), float32(w)-float32(wf)
		for k, c := range [4]struct {
			at int
			wt float32
		}{{hf*s + wf, (1 - dh) * (1 - dw)}, {hf*s + wc, (1 - dh) * dw}, {hc*s + wf, dh * (1 - dw)}, {hc*s + wc, dh * dw}} {
			idx[k*N+i], wts[k*N+i] = int32(c.at), c.wt
		}
	})
	rows := mlx.Take(t.pos, mlx.FromInt32(idx, 4, N), 0) // [4, N, hidden]
	rows = mlx.Mul(rows, mlx.ExpandDims(mlx.FromFloat32(wts, 4, N), -1))
	sum := mlx.SliceAxis(rows, 0, 0, 1)
	for k := 1; k < 4; k++ {
		sum = mlx.Add(sum, mlx.SliceAxis(rows, 0, k, k+1))
	}
	return mlx.Reshape(sum, N, t.cfg.Hidden)
}

// rotary is cos and sin of the patches' 2D rotary angles ([N, 1, hd], float32): half of each head's frequencies turn
// with the patch's row, half with its column, the table repeated over both halves of the features (rot_pos_emb).
func (t *visionTower) rotary(px *Pixels) (cos, sin array) {
	v := t.cfg
	hd := v.Hidden / v.Heads
	dim := hd / 2 // VisionRotaryEmbedding(head_dim / 2)
	nf := dim / 2 // its frequencies
	inv := make([]float32, nf)
	for j := range inv {
		inv[j] = float32(1 / math.Pow(10000, float64(2*j)/float64(dim)))
	}
	N := px.GridH * px.GridW
	c, s := make([]float32, N*hd), make([]float32, N*hd)
	mergeOrder(px, v.MergeSize, func(i, row, col int) {
		for j := range nf {
			for k, p := range [2]int{row, col} {
				a := float64(float32(p) * inv[j])
				cv, sv := float32(math.Cos(a)), float32(math.Sin(a))
				for _, at := range [2]int{k*nf + j, dim + k*nf + j} {
					c[i*hd+at], s[i*hd+at] = cv, sv
				}
			}
		}
	})
	return mlx.FromFloat32(c, N, 1, hd), mlx.FromFloat32(s, N, 1, hd)
}
