package web

import (
	"regexp"
	"strconv"
)

// probe asks macOS (sysctl, ioreg, sw_vers).
func probe() Specs {
	v := run("sw_vers", "-productVersion")
	sp := Specs{Chip: run("sysctl", "-n", "machdep.cpu.brand_string"), MacOS: v, OS: "macOS", OSVersion: v}
	if b, err := strconv.ParseInt(run("sysctl", "-n", "hw.memsize"), 10, 64); err == nil {
		sp.RAMGB = gb(b)
	}
	if m := gpuCores.FindStringSubmatch(run("ioreg", "-rc", "AGXAccelerator", "-d1")); m != nil {
		sp.GPUCores, _ = strconv.Atoi(m[1])
	}
	return sp
}

var gpuCores = regexp.MustCompile(`"gpu-core-count"\s*=\s*(\d+)`)
