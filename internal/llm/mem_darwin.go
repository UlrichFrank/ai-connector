//go:build darwin

package llm

import (
	"encoding/binary"
	"syscall"
)

// MemBytes is the machine's memory (sysctl hw.memsize; 0 if unknown).
func MemBytes() int64 {
	s, err := syscall.Sysctl("hw.memsize")
	if err != nil {
		return 0
	}
	b := []byte(s)
	for len(b) < 8 { // Sysctl drops a trailing zero byte
		b = append(b, 0)
	}
	return int64(binary.LittleEndian.Uint64(b[:8]))
}
