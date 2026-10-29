package engine

import "regexp"

// MiniMax Music 3 (MiniMaxAI/MiniMax-Music3: an 8B global LM from Qwen3-8B, a 0.6B local LM, a 2.4B flow-matching
// DiT and a Flow-VAE decoder) runs on the pure-MLX port github.com/mikolaj92/minimax-music3-mlx, installed into its
// own uv environment at a pinned commit; the worker (workers/minimax3.py) imports it. The weights are that port's
// quantised conversions (mikoy92/MiniMax-Music3-MLX-8bit and -4bit, not gated), pinned in musicModels.

// minimax3Instrumental are the lyrics of an instrumental song: wordless sections, each with an explicit
// "(instrumental)" line (structure tags alone condition the model weakly).
const minimax3Instrumental = "[intro]\n(instrumental)\n[instrumental]\n(instrumental)\n[outro]\n(instrumental)"

var instrumentalWord = regexp.MustCompile(`(?i)\binstrumental\b`)
