package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/laya"
)

// slowEmbedder takes a while for every call and records how many texts each call had.
type slowEmbedder struct {
	mu    sync.Mutex
	calls []int
	each  time.Duration
}

func (e *slowEmbedder) Embed(texts []string, _ int) [][]float32 {
	e.mu.Lock()
	e.calls = append(e.calls, len(texts))
	e.mu.Unlock()
	time.Sleep(e.each)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = []float32{float32(len(t))}
	}
	return out
}

type quickChooser struct{}

func (quickChooser) Choose(states []string, _ string, options []laya.Option, _ int) ([]laya.Answer, error) {
	out := make([]laya.Answer, len(states))
	for i := range out {
		out[i] = laya.Answer{Choice: options[0].Label}
	}
	return out, nil
}

// A direct call does not wait behind a long batch job of the same backend: it goes before the job's next part (the
// embedder), or does not wait at all (the classifier, which an embed job does not use). The job's result is whole.
func TestTextBackendDirectBeforeJobs(t *testing.T) {
	emb := &slowEmbedder{each: 40 * time.Millisecond}
	b := &TextBackend{emb: emb, cls: quickChooser{}}
	em := &Model{ID: "qwen3-embedding-0.6b", Kind: Embed, Name: "Embedder"}
	cm := &Model{ID: "laya-typed-decisions", Kind: Classify, Name: "Laya"}
	texts := make([]string, 20*textJobPart) // 20 parts: ~0.8 s
	for i := range texts {
		texts[i] = string(make([]byte, i%7))
	}
	var res map[string]any
	done := make(chan error, 1)
	go func() {
		done <- b.Run(context.Background(), em, Request{Texts: texts}, "", func(e Event) {
			if e.Type == "result" {
				res = e.Data
			}
		})
	}()
	for {
		emb.mu.Lock()
		n := len(emb.calls)
		emb.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	start := time.Now()
	if a, err := b.Classify(context.Background(), cm, []string{"x"}, "q", []Option{{Label: "a"}}); err != nil || a[0].Choice != "a" {
		t.Fatalf("classify %v %v", a, err)
	}
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Fatalf("the classifier waited %s for the embed job", took)
	}
	start = time.Now()
	v, err := b.Embed(context.Background(), em, []string{"abc"})
	if err != nil || len(v) != 1 || v[0][0] != 3 {
		t.Fatalf("embed %v %v", v, err)
	}
	if took := time.Since(start); took > 300*time.Millisecond { // the part that runs (40 ms), then it
		t.Fatalf("the direct embed waited %s (the job takes ~800 ms)", took)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if res["dim"] != 1 {
		t.Fatalf("result %v", res)
	}
	emb.mu.Lock()
	defer emb.mu.Unlock()
	total, direct := 0, 0
	for _, c := range emb.calls {
		if c == 1 {
			direct++
			continue
		}
		if c != textJobPart {
			t.Fatalf("a part of %d texts", c)
		}
		total += c
	}
	if total != len(texts) || direct != 1 {
		t.Fatalf("parts %v", emb.calls)
	}
}

// The priority lock: a high waiter goes before low ones that waited longer.
func TestPrioLock(t *testing.T) {
	var l prioLock
	l.lock(false)
	order := make(chan string, 3)
	var wg sync.WaitGroup
	for _, w := range []struct {
		name string
		high bool
	}{{"low1", false}, {"low2", false}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.lock(w.high)
			order <- w.name
			l.unlock()
		}()
	}
	time.Sleep(20 * time.Millisecond)
	wg.Add(1)
	go func() {
		defer wg.Done()
		l.lock(true)
		order <- "high"
		l.unlock()
	}()
	time.Sleep(20 * time.Millisecond)
	l.unlock()
	wg.Wait()
	if first := <-order; first != "high" {
		t.Fatalf("first %s", first)
	}
}
