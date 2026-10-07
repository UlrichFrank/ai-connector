//go:build !(darwin && arm64)

package llm

import (
	"context"
	"time"

	"github.com/philipparndt/ai-connector/internal/mlx"
)

// Engine: no built-in chat model in this build (MLX runs on macOS on Apple silicon only).
type Engine struct {
	Cfg    *Config
	Format string
}

// Stats describe one generation.
type Stats struct {
	PromptTokens, CachedTokens, Tokens int
	Prefill, FirstToken, Total         time.Duration
}

func (s Stats) TokensPerSecond() float64  { return 0 }
func (s Stats) PrefillPerSecond() float64 { return 0 }

// Available: no MLX here.
func Available() error { return mlx.ErrUnsupported }

// Load: no MLX here.
func Load(string) (*Engine, error) { return nil, mlx.ErrUnsupported }

func (e *Engine) Close()                     {}
func (e *Engine) Memory() (active, peak int) { return 0, 0 }
func (e *Engine) Defaults() Defaults         { return ModelDefaults(e.Cfg, GenerationConfig{}) }

func (e *Engine) Generate(context.Context, string, Options, func(string)) (Stats, error) {
	return Stats{}, mlx.ErrUnsupported
}
