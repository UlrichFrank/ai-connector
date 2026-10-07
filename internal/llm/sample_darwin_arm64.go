//go:build darwin && arm64

package llm

import (
	"math"

	"github.com/philipparndt/ai-connector/internal/mlx"
)

// sampler picks the tokens of one answer on the GPU. Everything stays lazy — next only adds to the graph, nothing
// is read back — so Generate can queue the next step before the current token is read. With a penalty it keeps a
// [1, V] mask of the tokens written so far (1 = present), updated in the graph with every token.
type sampler struct {
	o    Options
	seen array // kept; valid only with a penalty
}

func newSampler(o Options) *sampler { return &sampler{o: o} }

// state is what must be evaluated along with the token (the mask).
func (s *sampler) state() []array {
	if s.seen.Valid() {
		return []array{s.seen}
	}
	return nil
}

func (s *sampler) free() {
	mlx.Free(s.seen)
	s.seen = array{}
}

// next picks the next token from logits [1, V]: an int32 array [1].
func (s *sampler) next(logits array) array {
	pen := s.o.penalised()
	if pen && !s.seen.Valid() {
		s.seen = mlx.Keep(mlx.Zeros(mlx.Float32, 1, logits.Dim(-1)))
	}
	if pen {
		logits = s.penalise(logits)
	}
	var tok array
	if s.o.Temperature <= 0 {
		tok = mlx.AsType(mlx.Argmax(logits, -1, false), mlx.Int32)
	} else {
		vals, idx := s.filter(logits)
		choice := mlx.Reshape(mlx.Categorical(vals, -1), 1, 1)
		tok = mlx.AsType(mlx.Reshape(mlx.TakeAlongAxis(idx, mlx.AsType(choice, idx.Dtype()), -1), 1), mlx.Int32)
	}
	if pen {
		seen := mlx.Keep(mlx.PutAlongAxis(s.seen, mlx.Reshape(tok, 1, 1), mlx.Float(1), -1))
		mlx.Free(s.seen)
		s.seen = seen
	}
	return tok
}

// penalise lowers the logits of the tokens already written: presence (minus a constant) and repetition (a factor).
func (s *sampler) penalise(logits array) array {
	logits = mlx.AsType(logits, mlx.Float32)
	if rp := s.o.RepetitionPenalty; rp > 0 && rp != 1 {
		// x/rp for x ≥ 0, x·rp below; blended in where seen = 1
		pen := mlx.Where(mlx.Less(logits, mlx.Float(0)), mlx.Mul(logits, mlx.Float(rp)), mlx.Div(logits, mlx.Float(rp)))
		logits = mlx.Add(logits, mlx.Mul(s.seen, mlx.Sub(pen, logits)))
	}
	if pp := s.o.PresencePenalty; pp != 0 {
		logits = mlx.Sub(logits, mlx.Mul(s.seen, mlx.Float(pp)))
	}
	return logits
}

// filter is the top-k candidates' logits (scaled by the temperature, float32) and their token ids, both [1, k];
// with top-p or min-p the dropped candidates' logits are −∞. Top-p and min-p work on the probabilities among the
// top k (transformers' order: temperature, top-k, top-p, min-p).
func (s *sampler) filter(logits array) (vals, idx array) {
	V := logits.Dim(-1)
	k := min(max(s.o.TopK, 1), V)
	idx = mlx.SliceAxis(mlx.Argpartition(logits, V-k, -1), 1, V-k, V) // [1, k], unordered
	vals = mlx.Div(mlx.AsType(mlx.TakeAlongAxis(logits, idx, -1), mlx.Float32), mlx.Float(s.o.Temperature))
	topP := s.o.TopP > 0 && s.o.TopP < 1
	minP := s.o.MinP > 0
	if !topP && !minP {
		return vals, idx
	}
	// ascending: the most likely last
	order := mlx.Argsort(vals, -1)
	vals = mlx.TakeAlongAxis(vals, order, -1)
	idx = mlx.TakeAlongAxis(idx, order, -1)
	probs := mlx.Softmax(vals, -1, true)
	ninf := mlx.Float(float32(math.Inf(-1)))
	if topP {
		// keep a token while the tokens more likely than it hold less than p (the most likely is always kept)
		above := mlx.Cumsum(probs, -1, true, false)
		vals = mlx.Where(mlx.Less(above, mlx.Float(s.o.TopP)), vals, ninf)
	}
	if minP {
		best := mlx.SliceAxis(probs, 1, k-1, k)
		vals = mlx.Where(mlx.GreaterEqual(probs, mlx.Mul(best, mlx.Float(s.o.MinP))), vals, ninf)
	}
	return vals, idx
}
