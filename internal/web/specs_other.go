//go:build !darwin && !linux && !windows

package web

// probe: nothing known about this system.
func probe() Specs { return Specs{} }
