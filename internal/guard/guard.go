// Package guard is the second line of defence of a chat with tools (the zones are the first, see
// internal/agent/contract.go): it screens untrusted text — a web page, a search result — for instructions aimed at
// the assistant before the model reads it, and grades the final answer.
//
// It runs the Laya classifier in this process (Classifier: engine.TextBackend.Choose), never through the job queue:
// the chat job that asks holds the queue's slot.
//
// # Screen
//
// The text is cut into overlapping chunks (Chunks), each fits Laya's 1024-token window together with the question.
// Each chunk gets an injection probability p from the classifier, and the cheap heuristics (Markers) of the chunk add
// their weights in log-odds:
//
//	score(chunk) = sigmoid(logit(p) + Sum(markers))        Sum is capped at +5
//
// The page's score is its worst chunk's. At Threshold or above the verdict is "injection". Chunks the classifier
// did not get to (MaxChunks, Budget) are scored by the heuristics alone, from a prior of HeuristicPrior — so the
// heuristics always cover the whole text and a page is never waved through unread. Chunks with markers are
// classified first, then the first and the last, then the rest in order.
//
// A page without an injection is "irrelevant" when no screened chunk is likely to help with the question
// (relevance below IrrelevantBelow), otherwise "ok". Score is the confidence in the label.
//
// # Grade
//
// One classifier question over the question, the answer and (the start of) the sources: good or bad. Between
// GradeBad and GradeGood the verdict is "unsure".
//
// # How well it works (testdata, `mlxlab guard eval`, `go test -tags eval ./internal/guard -run TestEval -v`)
//
// Laya does not separate injections from normal pages: alone (NoHeuristics) it catches 5 of 45 injections of the
// tuning set and 0 of 17 of the holdout. With the heuristics it reaches 0.98 recall / 0.06 false alarms on the
// tuning set — but the heuristics were written against that set: on the holdout (written afterwards, never tuned
// on) it is 0.29 recall / 0.28 false alarms. So: Laya + heuristics is a cheap tripwire (~0.2 s per page), not a
// filter to rely on. A chat model as judge (ChatJudge, NewJudged; Qwen3.6-27B 4-bit, no thinking) got every case
// right — tuning set, holdout and grading — at ~3 s per call (~6 s per page with the relevance question). Use the
// chat's own model, which is loaded anyway, as the judge. Laya's grading: good recall 0.94, bad recall 0.29.
package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/philipparndt/ai-connector/internal/agent"
	"github.com/philipparndt/ai-connector/internal/laya"
)

// Classifier answers a choice question about texts: Laya, in process (engine.TextBackend.Choose or a loaded
// laya.Model through ModelClassifier).
type Classifier interface {
	Choose(ctx context.Context, states []string, instructions string, options []laya.Option) ([]laya.Answer, error)
}

// ModelClassifier adapts a loaded laya.Model.
type ModelClassifier struct{ Model *laya.Model }

func (c ModelClassifier) Choose(ctx context.Context, states []string, instructions string, options []laya.Option) ([]laya.Answer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.Model.Choose(states, instructions, options, 32)
}

// Question is one choice question: the instructions, the options, and the label whose probability is the
// question's score (injection, relevant, good).
type Question struct {
	Instructions string
	Options      []laya.Option
	Positive     string
	// State turns the inputs into the text Laya reads.
	State func(question, text string) string
}

// Config tunes the guard; the zero value of a field means its default.
type Config struct {
	ChunkSize int           // bytes per chunk (default 1800)
	Overlap   int           // bytes the chunks overlap (default 200)
	MaxChunks int           // chunks the classifier reads per text at most (default 24)
	Batch     int           // chunks per classifier call (default 8; the budget is checked between calls)
	Budget    time.Duration // time per Screen for the classifier (default 10 s); the heuristics read everything
	Threshold float64       // injection score at or above which Screen says "injection" (default 0.6)
	// HeuristicPrior is the injection probability assumed for a chunk the classifier did not read (default 0.05).
	HeuristicPrior float64
	// IrrelevantBelow: the page is "irrelevant" when its best chunk's relevance is below this (default 0.35; 0 turns the
	// relevance question off, negative too).
	IrrelevantBelow float64
	RelevanceChunks int     // chunks asked about relevance (the first ones, default 3)
	GradeGood       float64 // P(good) at or above which Grade says "good" (default 0.6)
	GradeBad        float64 // P(good) at or below which Grade says "bad" (default 0.4)
	SourceChars     int     // characters of the sources Grade shows the classifier (default 2400)

	Injection, Relevance, Grading *Question // nil: the defaults (prompts.go)
	// NoHeuristics: the classifier's word alone for the chunks it read (a chat judge: the markers only add false
	// alarms on pages that quote attacks). Chunks it did not read are still scored by the heuristics.
	NoHeuristics bool
}

func (c Config) withDefaults() Config {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&c.ChunkSize, 1800)
	def(&c.Overlap, 200)
	def(&c.MaxChunks, 24)
	def(&c.Batch, 8)
	def(&c.RelevanceChunks, 3)
	def(&c.SourceChars, 2400)
	if c.Budget <= 0 {
		c.Budget = 10 * time.Second
	}
	if c.Threshold <= 0 {
		c.Threshold = 0.6
	}
	if c.HeuristicPrior <= 0 {
		c.HeuristicPrior = 0.05
	}
	if c.IrrelevantBelow == 0 {
		c.IrrelevantBelow = 0.35
	}
	if c.GradeGood <= 0 {
		c.GradeGood = 0.6
	}
	if c.GradeBad <= 0 {
		c.GradeBad = 0.4
	}
	if c.Injection == nil {
		c.Injection = &InjectionQuestion
	}
	if c.Relevance == nil {
		c.Relevance = &RelevanceQuestion
	}
	if c.Grading == nil {
		c.Grading = &GradeQuestion
	}
	return c
}

// Guard implements agent.Guard.
type Guard struct {
	c   Classifier
	cfg Config
	// grader rates answers (nil: c). A chat judge grades with numbered options: answering "good" or "bad" was
	// steered by data that contains those words (a smart home's bathroom is "bad" in German).
	grader Classifier
}

var _ agent.Guard = (*Guard)(nil)

// New makes a guard that asks c.
func New(c Classifier, cfg Config) *Guard {
	return &Guard{c: c, cfg: cfg.withDefaults()}
}

// ChunkReport is what the guard found in one chunk.
type ChunkReport struct {
	Index      int      `json:"index"`
	Classified bool     `json:"classified"`          // false: heuristics only (budget or MaxChunks)
	P          float64  `json:"p"`                   // the classifier's injection probability (the prior if not classified)
	Markers    []Marker `json:"markers,omitempty"`   // heuristics
	Score      float64  `json:"score"`               // combined
	Relevance  float64  `json:"relevance,omitempty"` // P(relevant), when asked
}

// Report is a Screen's verdict with the details.
type Report struct {
	agent.Verdict
	Chunks    []ChunkReport `json:"chunks"`
	Worst     int           `json:"worst"`     // index of the chunk with the highest score
	Relevance float64       `json:"relevance"` // the best chunk's (1 when not asked)
	Calls     int           `json:"calls"`     // classifier calls
	Took      time.Duration `json:"took"`
}

// Screen checks untrusted text for instructions aimed at the assistant (agent.Guard).
func (g *Guard) Screen(ctx context.Context, question, content string) (agent.Verdict, error) {
	r, err := g.ScreenReport(ctx, question, content)
	return r.Verdict, err
}

// ScreenReport is Screen with the per-chunk details.
func (g *Guard) ScreenReport(ctx context.Context, question, content string) (Report, error) {
	start := time.Now()
	cfg := g.cfg
	chunks := Chunks(content, cfg.ChunkSize, cfg.Overlap)
	r := Report{Relevance: 1}
	if len(chunks) == 0 {
		r.Verdict = agent.Verdict{Label: "irrelevant", Score: 1, Why: "empty"}
		return r, nil
	}
	r.Chunks = make([]ChunkReport, len(chunks))
	for i, ch := range chunks {
		r.Chunks[i] = ChunkReport{Index: i, P: cfg.HeuristicPrior, Markers: Markers(ch)}
	}
	// the whole text too: a marker split between two chunks, or spread out (invisible characters), still counts
	var whole []Marker
	if !cfg.NoHeuristics {
		whole = Markers(content)
	}

	// the classifier, in order of suspicion, within the budget
	order := g.order(r.Chunks)
	if len(order) > cfg.MaxChunks {
		order = order[:cfg.MaxChunks]
	}
	deadline := start.Add(cfg.Budget)
	q := cfg.Injection
	for lo := 0; lo < len(order); lo += cfg.Batch {
		if lo > 0 && time.Now().After(deadline) {
			break
		}
		hi := min(len(order), lo+cfg.Batch)
		states := make([]string, 0, hi-lo)
		for _, i := range order[lo:hi] {
			states = append(states, q.State(question, chunks[i]))
		}
		ans, err := g.c.Choose(ctx, states, q.Instructions, q.Options)
		r.Calls++
		if err != nil {
			return r, err
		}
		for k, i := range order[lo:hi] {
			r.Chunks[i].Classified = true
			r.Chunks[i].P = ans[k].Probabilities[q.Positive]
		}
	}
	r.Worst = 0
	for i := range r.Chunks {
		c := &r.Chunks[i]
		if c.Classified && cfg.NoHeuristics {
			c.Markers = nil
		}
		c.Score = Combine(c.P, c.Markers)
		if c.Score > r.Chunks[r.Worst].Score {
			r.Worst = i
		}
	}
	// whole-text markers that no chunk has on its own
	if extra := missing(whole, r.Chunks); len(extra) > 0 {
		w := &r.Chunks[r.Worst]
		w.Markers = append(w.Markers, extra...)
		w.Score = Combine(w.P, w.Markers)
	}
	worst := r.Chunks[r.Worst]
	if worst.Score >= cfg.Threshold {
		r.Verdict = agent.Verdict{Label: "injection", Score: round3(worst.Score), Why: why(worst, len(chunks))}
		r.Took = time.Since(start)
		return r, nil
	}

	// relevance: the first chunks (a page's topic shows early); skipped when out of time
	if cfg.IrrelevantBelow > 0 && question != "" && time.Now().Before(deadline) {
		rq := cfg.Relevance
		n := min(len(chunks), cfg.RelevanceChunks)
		states := make([]string, n)
		for i := range states {
			states[i] = rq.State(question, chunks[i])
		}
		ans, err := g.c.Choose(ctx, states, rq.Instructions, rq.Options)
		r.Calls++
		if err != nil {
			return r, err
		}
		r.Relevance = 0
		for i, a := range ans {
			r.Chunks[i].Relevance = a.Probabilities[rq.Positive]
			r.Relevance = max(r.Relevance, r.Chunks[i].Relevance)
		}
		if r.Relevance < cfg.IrrelevantBelow {
			r.Verdict = agent.Verdict{Label: "irrelevant", Score: round3(1 - r.Relevance),
				Why: fmt.Sprintf("relevance %.2f", r.Relevance)}
			r.Took = time.Since(start)
			return r, nil
		}
	}
	r.Verdict = agent.Verdict{Label: "ok", Score: round3(1 - worst.Score)}
	if len(worst.Markers) > 0 {
		r.Why = "markers below the threshold: " + names(worst.Markers)
	}
	r.Took = time.Since(start)
	return r, nil
}

// order: chunks with markers first (strongest first), then the first and the last chunk, then the rest in order.
func (g *Guard) order(cs []ChunkReport) []int {
	idx := make([]int, len(cs))
	rank := make([]float64, len(cs))
	for i, c := range cs {
		idx[i] = i
		rank[i] = Sum(c.Markers)
		if rank[i] == 0 && (i == 0 || i == len(cs)-1) {
			rank[i] = 0.01
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return rank[idx[a]] > rank[idx[b]] })
	return idx
}

// Combine adds the markers' weights to the classifier's log-odds: score = sigmoid(logit(p) + Sum(markers)).
func Combine(p float64, ms []Marker) float64 {
	p = min(max(p, 1e-4), 1-1e-4)
	return 1 / (1 + math.Exp(-(math.Log(p/(1-p)) + Sum(ms))))
}

func missing(whole []Marker, cs []ChunkReport) []Marker {
	seen := map[string]bool{}
	for _, c := range cs {
		for _, m := range c.Markers {
			seen[m.Name] = true
		}
	}
	var out []Marker
	for _, m := range whole {
		if !seen[m.Name] {
			out = append(out, m)
		}
	}
	return out
}

func why(c ChunkReport, n int) string {
	var parts []string
	if n > 1 {
		parts = append(parts, fmt.Sprintf("part %d of %d", c.Index+1, n))
	}
	if c.Classified {
		parts = append(parts, fmt.Sprintf("classifier %.2f", c.P))
	} else {
		parts = append(parts, "not classified (time budget)")
	}
	if len(c.Markers) > 0 {
		parts = append(parts, "markers: "+names(c.Markers))
	}
	return strings.Join(parts, "; ")
}

func names(ms []Marker) string {
	s := make([]string, len(ms))
	for i, m := range ms {
		s[i] = m.Name
		if m.Match != "" {
			s[i] += fmt.Sprintf(" (%q)", m.Match)
		}
	}
	return strings.Join(s, ", ")
}

// Grade rates an answer (agent.Guard): good, bad or unsure.
func (g *Guard) Grade(ctx context.Context, question, answer string, sources []string) (agent.Verdict, error) {
	cfg := g.cfg
	q := cfg.Grading
	state := gradeState(question, answer, sources, cfg.SourceChars)
	c := g.c
	if g.grader != nil {
		c = g.grader
	}
	ans, err := c.Choose(ctx, []string{state}, q.Instructions, q.Options)
	if err != nil {
		return agent.Verdict{}, err
	}
	p := ans[0].Probabilities[q.Positive]
	switch {
	case p >= cfg.GradeGood:
		return agent.Verdict{Label: "good", Score: round3(p)}, nil
	case p <= cfg.GradeBad:
		return agent.Verdict{Label: "bad", Score: round3(1 - p), Why: ans[0].Choice}, nil
	}
	return agent.Verdict{Label: "unsure", Score: round3(1 - math.Abs(p-0.5)*2), Why: fmt.Sprintf("P(good) %.2f", p)}, nil
}

// gradeState: the question, the answer and the sources (shared out fairly, cut to chars in all) as JSON.
func gradeState(question, answer string, sources []string, chars int) string {
	src := []string{} // [] rather than null: "no sources" must read as such
	if len(sources) > 0 {
		per := chars / len(sources)
		for _, s := range sources {
			s = strings.Join(strings.Fields(s), " ")
			if len(s) > per {
				s = truncate(s, per) + " …"
			}
			src = append(src, s)
		}
	}
	b, _ := json.Marshal(struct {
		Question string   `json:"question"`
		Answer   string   `json:"answer"`
		Sources  []string `json:"sources"`
	}{question, answer, src})
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }
