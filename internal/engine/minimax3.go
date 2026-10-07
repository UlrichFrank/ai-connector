package engine

import "regexp"

// MiniMax Music 3 (MiniMaxAI/MiniMax-Music3: an 8B global LM from Qwen3-8B, a 0.6B local LM, a 2.4B flow-matching
// DiT and a Flow-VAE decoder) runs on the pure-MLX port github.com/mikolaj92/minimax-music3-mlx, installed into its
// own uv environment at a pinned commit; the worker (workers/minimax3.py) imports it. The weights are that port's
// quantised conversions (mikoy92/MiniMax-Music3-MLX-8bit and -4bit, not gated), pinned in musicModels.

// minimax3Commit is the pinned commit of github.com/mikolaj92/minimax-music3-mlx (2026-08-24: the progress callback).
const minimax3Commit = "b7699bc5995296403780f32500c2450e3a0ee2a1"

// minimax3Pins are its dependencies, pinned (the port itself only asks for mlx>=0.22, mlx-lm>=0.22 …).
var minimax3Pins = []string{"mlx==0.32.3", "mlx-lm==0.32.0", "numpy==2.5.3", "soundfile==0.14.0", "safetensors==0.8.0",
	"transformers==5.18.0", "tokenizers==0.23.2", "huggingface_hub==1.33.0"}

// minimax3Licence is the weights' licence (the pickers show it): commercial use is allowed, with conditions.
const minimax3Licence = "MiniMax-Music3 Community License: commercial use allowed if \"MiniMax-Music3\" is shown in the " +
	"product's UI (above USD 20M yearly revenue only with MiniMax's written authorization); acceptable use policy applies"

// minimax3Speed is what a song costs on a 64 GB M1 Max (measured, docs/music.md).
const minimax3Speed = "Slow: about 6 min for 30 s on an M1 Max, 20 GB peak."

// minimax3Longest is the longest song (MiniMax: "up to five minutes"; 9,000 frames at 25 Hz is the hard limit).
const minimax3Longest = 300

// minimax3Instrumental are the lyrics of an instrumental song: wordless sections, each with an explicit
// "(instrumental)" line (structure tags alone condition the model weakly).
const minimax3Instrumental = "[intro]\n(instrumental)\n[instrumental]\n(instrumental)\n[outro]\n(instrumental)"

var instrumentalWord = regexp.MustCompile(`(?i)\binstrumental\b`)
