package tensor

import (
	"sync/atomic"
	"time"
)

// Where the time of a forward pass goes (for benchmarks): the linear layers, attention, and on the GPU the encoding of
// the command buffers, the wait for them, the copies into and out of the pool buffers, how many pool buffers were
// made and how many buffers were set aside near a 2 GiB boundary (metal_darwin.go). The rest of a forward pass is
// norms, activations, RoPE and copies.
var prof struct {
	linear, attention, gpuEncode, gpuWait, gpuCopy, gpuCalls, gpuAllocs, gpuSpanning atomic.Int64
}

// Profile is the time spent so far (see prof).
type Profile struct {
	Linear, Attention, GPUEncode, GPUWait, GPUCopy time.Duration
	GPUCalls, GPUAllocs, GPUSetAside               int64
}

// ReadProfile returns the times since the last ResetProfile.
func ReadProfile() Profile {
	return Profile{time.Duration(prof.linear.Load()), time.Duration(prof.attention.Load()),
		time.Duration(prof.gpuEncode.Load()), time.Duration(prof.gpuWait.Load()), time.Duration(prof.gpuCopy.Load()),
		prof.gpuCalls.Load(), prof.gpuAllocs.Load(), prof.gpuSpanning.Load()}
}

// ResetProfile sets the times to zero.
func ResetProfile() {
	prof.linear.Store(0)
	prof.attention.Store(0)
	prof.gpuEncode.Store(0)
	prof.gpuWait.Store(0)
	prof.gpuCopy.Store(0)
	prof.gpuCalls.Store(0)
	prof.gpuAllocs.Store(0)
	prof.gpuSpanning.Store(0)
}

func since(c *atomic.Int64, start time.Time) { c.Add(int64(time.Since(start))) }
