//go:build darwin || linux

package qwen3

import (
	"runtime"
	"syscall"
)

// maxRSS is the process's peak resident memory in MB.
func maxRSS() float64 {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	if runtime.GOOS == "darwin" {
		return float64(ru.Maxrss) / (1 << 20) // bytes
	}
	return float64(ru.Maxrss) / (1 << 10) // KB
}
