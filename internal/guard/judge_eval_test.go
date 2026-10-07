//go:build eval

package guard

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/llm"
)

// TestJudge compares a chat model as judge with Laya on the same eval set:
// GUARD_JUDGE=<MLX model dir> go test -tags eval ./internal/guard -run TestJudge -v
// (GUARD_JUDGE_LAYA=1 also runs Laya alone, without heuristics).
func TestJudge(t *testing.T) {
	dir := os.Getenv("GUARD_JUDGE")
	if dir == "" {
		t.Skip("GUARD_JUDGE not set")
	}
	start := time.Now()
	eng, err := llm.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	t.Logf("loaded %s in %v", dir, time.Since(start))
	judge := ChatJudge{Complete: func(ctx context.Context, system, user string) (string, error) {
		p := llm.Render(eng.Format, []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: user}}, nil, false)
		var out string
		_, err := eng.Generate(ctx, p, llm.Options{MaxTokens: 8}, func(s string) { out += s })
		return out, err
	}}
	ctx := context.Background()
	run := func(name string, c Classifier, cfg Config) {
		os.Stdout.WriteString("\n========== " + name + " ==========\n")
		if _, err := RunEval(ctx, New(c, cfg), os.Stdout, os.Getenv("GUARD_VERBOSE") != ""); err != nil {
			t.Fatal(err)
		}
	}
	run("judge alone", judge, Config{Injection: &JudgeInjectionQuestion, NoHeuristics: true})
	run("judge + heuristics", judge, Config{Injection: &JudgeInjectionQuestion})
	if os.Getenv("GUARD_JUDGE_LAYA") != "" {
		m := loadLaya(t)
		run("laya alone", ModelClassifier{m}, Config{NoHeuristics: true})
	}
}
