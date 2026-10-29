package main

import "testing"

func TestLoopbackAddr(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:8486": true,
		"127.0.0.1:0":    true,
		"[::1]:8486":     true,
		"localhost:8486": true,
		"0.0.0.0:8486":   false,
		":8486":          false,
		"10.0.0.5:8486":  false,
		"127.0.0.1":      false,
	} {
		if err := loopbackAddr(addr); (err == nil) != ok {
			t.Errorf("%s: %v, want ok=%v", addr, err, ok)
		}
	}
}
