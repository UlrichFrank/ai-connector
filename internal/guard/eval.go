package guard

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"
)

// The eval set (testdata/*.jsonl): realistic web snippets labelled ok / injection / irrelevant, and answers labelled
// good / bad. Run it with `go test -tags eval ./internal/guard -run Eval -v` or `mlxlab guard eval`.
//
//go:embed testdata/screen.jsonl testdata/holdout.jsonl testdata/grade.jsonl
var testdata embed.FS

// ScreenCase is one line of testdata/screen.jsonl.
type ScreenCase struct {
	ID       string `json:"id"`
	Label    string `json:"label"` // ok | injection | irrelevant
	Kind     string `json:"kind"`  // news, docs, … ; direct, polite, hidden-html, german, base64, …
	Question string `json:"question"`
	Content  string `json:"content"`
}

// GradeCase is one line of testdata/grade.jsonl.
type GradeCase struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"` // good | bad
	Kind     string   `json:"kind"`
	Question string   `json:"question"`
	Answer   string   `json:"answer"`
	Sources  []string `json:"sources"`
}

// LoadEval reads the eval set: the screen cases the guard was tuned on, and the grade cases.
func LoadEval() ([]ScreenCase, []GradeCase, error) {
	var sc []ScreenCase
	var gc []GradeCase
	if err := readJSONL("testdata/screen.jsonl", &sc); err != nil {
		return nil, nil, err
	}
	if err := readJSONL("testdata/grade.jsonl", &gc); err != nil {
		return nil, nil, err
	}
	return sc, gc, nil
}

// LoadHoldout reads the screen cases written after the tuning and never tuned on (testdata/holdout.jsonl).
func LoadHoldout() ([]ScreenCase, error) {
	var sc []ScreenCase
	return sc, readJSONL("testdata/holdout.jsonl", &sc)
}

func readJSONL[T any](name string, out *[]T) error {
	raw, err := testdata.ReadFile(name)
	if err != nil {
		return err
	}
	s := bufio.NewScanner(bytes.NewReader(raw))
	s.Buffer(make([]byte, 1<<20), 1<<22)
	for n := 1; s.Scan(); n++ {
		if strings.TrimSpace(s.Text()) == "" {
			continue
		}
		var v T
		if err := json.Unmarshal(s.Bytes(), &v); err != nil {
			return fmt.Errorf("%s:%d: %w", name, n, err)
		}
		*out = append(*out, v)
	}
	return s.Err()
}

// EvalResult sums up a run.
type EvalResult struct {
	Screen, Holdout ScreenResult
	Grade           Confusion
	GradeLatency    Latency
}

// ScreenResult sums up the screen cases of one set.
type ScreenResult struct {
	Confusion
	// InjectionRecall: injections caught. BenignFlagged: ok pages (incl. the ones about injection) called injection.
	InjectionRecall, BenignFlagged float64
	Latency                        Latency
}

// Confusion counts gold label × predicted label.
type Confusion struct {
	Labels []string
	N      map[string]map[string]int
}

func newConfusion(labels ...string) Confusion {
	return Confusion{Labels: labels, N: map[string]map[string]int{}}
}

func (c *Confusion) add(gold, pred string) {
	if c.N[gold] == nil {
		c.N[gold] = map[string]int{}
	}
	c.N[gold][pred]++
	if !slices.Contains(c.Labels, pred) {
		c.Labels = append(c.Labels, pred)
	}
}

// PR is a label's precision and recall.
func (c Confusion) PR(label string) (precision, recall float64, support int) {
	tp, predicted := 0, 0
	for _, g := range c.Labels {
		predicted += c.N[g][label]
	}
	tp = c.N[label][label]
	for _, n := range c.N[label] {
		support += n
	}
	if predicted > 0 {
		precision = float64(tp) / float64(predicted)
	}
	if support > 0 {
		recall = float64(tp) / float64(support)
	}
	return
}

func (c Confusion) write(w io.Writer, gold []string) {
	fmt.Fprintf(w, "  %-12s", "gold \\ pred")
	for _, p := range c.Labels {
		fmt.Fprintf(w, " %10s", p)
	}
	fmt.Fprintln(w)
	for _, g := range gold {
		fmt.Fprintf(w, "  %-12s", g)
		for _, p := range c.Labels {
			fmt.Fprintf(w, " %10d", c.N[g][p])
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "  %-12s %9s %9s %8s\n", "label", "precision", "recall", "support")
	for _, l := range c.Labels {
		p, r, n := c.PR(l)
		fmt.Fprintf(w, "  %-12s %9.2f %9.2f %8d\n", l, p, r, n)
	}
}

// Latency is the distribution of call times.
type Latency []time.Duration

func (l Latency) String() string {
	if len(l) == 0 {
		return "-"
	}
	s := slices.Clone(l)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var sum time.Duration
	for _, d := range s {
		sum += d
	}
	q := func(f float64) time.Duration { return s[min(len(s)-1, int(f*float64(len(s))))] }
	ms := func(d time.Duration) string { return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000) }
	return fmt.Sprintf("n=%d mean %s  p50 %s  p95 %s  max %s", len(s), ms(sum/time.Duration(len(s))), ms(q(0.5)), ms(q(0.95)), ms(s[len(s)-1]))
}

// RunEval runs the eval set (tuning set, holdout, grade) through g and writes the per-case lines (verbose: all,
// else the mistakes) and the summary to w.
func RunEval(ctx context.Context, g *Guard, w io.Writer, verbose bool) (EvalResult, error) {
	sc, gc, err := LoadEval()
	if err != nil {
		return EvalResult{}, err
	}
	ho, err := LoadHoldout()
	if err != nil {
		return EvalResult{}, err
	}
	var res EvalResult
	if res.Screen, err = screenSet(ctx, g, w, "screen (tuning set)", sc, verbose); err != nil {
		return res, err
	}
	if res.Holdout, err = screenSet(ctx, g, w, "screen (holdout, never tuned on)", ho, verbose); err != nil {
		return res, err
	}
	res.Grade = newConfusion("good", "bad", "unsure")
	fmt.Fprintf(w, "\ngrade: %d cases\n", len(gc))
	for _, c := range gc {
		start := time.Now()
		v, err := g.Grade(ctx, c.Question, c.Answer, c.Sources)
		if err != nil {
			return res, fmt.Errorf("%s: %w", c.ID, err)
		}
		res.GradeLatency = append(res.GradeLatency, time.Since(start))
		res.Grade.add(c.Label, v.Label)
		if verbose || v.Label != c.Label {
			fmt.Fprintf(w, "%s%-22s %-18s gold %-5s got %-7s score %.2f  %s\n", mark(v.Label == c.Label), c.ID, c.Kind, c.Label, v.Label, v.Score, v.Why)
		}
	}
	for _, r := range []struct {
		name string
		r    ScreenResult
	}{{"screen (tuning set)", res.Screen}, {"screen (holdout)", res.Holdout}} {
		fmt.Fprintf(w, "\n%s:\n", r.name)
		r.r.write(w, []string{"ok", "injection", "irrelevant"})
		fmt.Fprintf(w, "  injection recall %.2f, benign pages flagged as injection %.2f\n", r.r.InjectionRecall, r.r.BenignFlagged)
		fmt.Fprintf(w, "  latency per Screen: %s\n", r.r.Latency)
	}
	fmt.Fprintln(w, "\ngrade:")
	res.Grade.write(w, []string{"good", "bad"})
	fmt.Fprintf(w, "  latency per Grade: %s\n", res.GradeLatency)
	return res, nil
}

func mark(right bool) string {
	if right {
		return "  "
	}
	return "✗ "
}

func screenSet(ctx context.Context, g *Guard, w io.Writer, name string, sc []ScreenCase, verbose bool) (ScreenResult, error) {
	res := ScreenResult{Confusion: newConfusion("ok", "injection", "irrelevant")}
	fmt.Fprintf(w, "\n%s: %d cases\n", name, len(sc))
	inj, injHit, ok, okFlag := 0, 0, 0, 0
	for _, c := range sc {
		start := time.Now()
		r, err := g.ScreenReport(ctx, c.Question, c.Content)
		if err != nil {
			return res, fmt.Errorf("%s: %w", c.ID, err)
		}
		res.Latency = append(res.Latency, time.Since(start))
		res.add(c.Label, r.Label)
		switch c.Label {
		case "injection":
			inj++
			if r.Label == "injection" {
				injHit++
			}
		case "ok":
			ok++
			if r.Label == "injection" {
				okFlag++
			}
		}
		if verbose || r.Label != c.Label {
			w0 := r.Chunks[r.Worst]
			fmt.Fprintf(w, "%s%-26s %-16s gold %-10s got %-10s score %.2f  p %.3f  markers %.1f  rel %.2f  chunks %d  %4d ms  %s\n",
				mark(r.Label == c.Label), c.ID, c.Kind, c.Label, r.Label, r.Score, w0.P, Sum(w0.Markers), r.Relevance, len(r.Chunks),
				r.Took.Milliseconds(), r.Why)
		}
	}
	if inj > 0 {
		res.InjectionRecall = float64(injHit) / float64(inj)
	}
	if ok > 0 {
		res.BenignFlagged = float64(okFlag) / float64(ok)
	}
	return res, nil
}
