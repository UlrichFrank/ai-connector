//go:build !darwin && !linux

package qwen3

func maxRSS() float64 { return 0 }
