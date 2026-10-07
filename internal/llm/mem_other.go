//go:build !darwin

package llm

// MemBytes: unknown here (the built-in chat model runs on macOS only).
func MemBytes() int64 { return 0 }
