package laya

import (
	"errors"
	"math"
	"strings"

	"github.com/philipparndt/ai-connector/internal/tensor"
)

// Option is one answer of a choice question: its label and an optional description.
type Option struct {
	Label, Description string
}

// Answer is the answer to a choice question for one state, as the bridge protocol carries it (laya's own keys).
type Answer struct {
	Choice           string             `json:"choice"`
	Probabilities    map[string]float64 `json:"probabilities"`
	AnswerConfidence float64            `json:"answer_confidence"`
}

const qtypeChoice = 0

// Choose asks one choice question (instructions, options in their order) about every state — each state the text
// Laya reads (vboard sends JSON objects; the caller serialises them as Python's json.dumps would). batch caps the
// states per forward pass.
func (m *Model) Choose(states []string, instructions string, options []Option, batch int) ([]Answer, error) {
	if len(options) == 0 {
		return nil, errors.New("a choice question needs at least one option")
	}
	if batch <= 0 {
		batch = 32
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	head, markers := m.question(instructions, options)
	if len(markers) != len(options) {
		return nil, errors.New("the options do not fit into the question")
	}
	out := make([]Answer, len(states))
	tensor.Batches(len(states), batch, func(lo, hi int) {
		seqs := make([]seq, hi-lo)
		for i, st := range states[lo:hi] {
			seqs[i] = m.sequence(head, markers, st)
		}
		for i, logits := range m.forward(seqs) {
			out[lo+i] = m.answer(logits, options)
		}
	})
	return out, nil
}

// question builds the state-independent start of a row (laya/common.py build_sequence):
// [CLS] "choice question: <instructions>" [SEP] [MASK] option … [MASK] option … [SEP]
func (m *Model) question(instructions string, options []Option) ([]int, []int) {
	headIDs := m.tok.Encode("choice question: " + strings.ReplaceAll(instructions, "[MASK]", " "))
	var optIDs [][]int
	for _, o := range options {
		text := o.Label
		if o.Description != "" {
			text = o.Label + ": " + o.Description
		}
		ids := m.tok.Encode(" " + strings.ReplaceAll(text, "[MASK]", " "))
		if len(ids) > 48 {
			ids = ids[:48]
		}
		optIDs = append(optIDs, append([]int{m.mask}, ids...))
	}
	hm := m.cfg.HeadMaxLen
	sum := func() int {
		n := 0
		for _, o := range optIDs {
			n += len(o)
		}
		return n
	}
	budget := hm - sum()
	if budget < 16 {
		per := max(4, (hm-16)/max(1, len(optIDs)))
		for i, o := range optIDs {
			optIDs[i] = o[:min(len(o), per)]
		}
		budget = hm - sum()
	}
	headIDs = headIDs[:min(len(headIDs), max(8, budget))]
	ids := append([]int{m.cls}, headIDs...)
	ids = append(ids, m.sep)
	var markers []int
	for _, o := range optIDs {
		markers = append(markers, len(ids))
		ids = append(ids, o...)
	}
	ids = append(ids, m.sep)
	var kept []int
	for _, p := range markers {
		if p < m.cfg.MaxLen {
			kept = append(kept, p)
		}
	}
	return ids, kept
}

// sequence appends a state to the question: … [SEP] state [SEP], cut to max_len (the state loses its end).
func (m *Model) sequence(head, markers []int, state string) seq {
	st := m.tok.Encode(strings.ReplaceAll(state, "[MASK]", " "))
	room := max(0, m.cfg.MaxLen-len(head)-1)
	st = st[:min(len(st), room)]
	ids := make([]int, 0, len(head)+len(st)+1)
	ids = append(ids, head...)
	ids = append(ids, st...)
	ids = append(ids, m.sep)
	if len(ids) > m.cfg.MaxLen {
		ids = ids[:m.cfg.MaxLen]
	}
	return seq{ids: ids, markers: markers, qtype: qtypeChoice}
}

// answer turns the option logits into probabilities (temperature-scaled softmax) and the calibrated confidence.
func (m *Model) answer(logits []float32, options []Option) Answer {
	k := len(logits)
	t := m.temperature(qtypeChoice, k)
	z := make([]float64, k)
	mx := math.Inf(-1)
	for i, v := range logits {
		z[i] = float64(v) / t
		mx = max(mx, z[i])
	}
	var s float64
	for i := range z {
		z[i] = math.Exp(z[i] - mx)
		s += z[i]
	}
	a := Answer{Probabilities: map[string]float64{}}
	best := 0
	for i := range z {
		z[i] /= s
		a.Probabilities[options[i].Label] = round4(z[i])
		if z[i] > z[best] {
			best = i
		}
	}
	a.Choice = options[best].Label
	a.AnswerConfidence = round4(min(1, max(0, z[best])))
	return a
}

func round4(x float64) float64 { return math.Round(x*1e4) / 1e4 }
