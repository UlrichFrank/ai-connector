//go:build !linux

package engine

// procListeners: no /proc here (macOS has lsof; Windows neither): the port is not checked.
func procListeners(int) ([]int, bool) { return nil, false }
