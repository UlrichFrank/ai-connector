package web

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// probe reads /proc and /etc/os-release.
func probe() Specs {
	sp := Specs{OS: "Linux"}
	sp.Chip = field("/proc/cpuinfo", "model name", ':')
	if sp.Chip == "" { // arm boards name the board, not the core
		if b, err := os.ReadFile("/proc/device-tree/model"); err == nil {
			sp.Chip = strings.TrimRight(string(b), "\x00\n")
		}
	}
	if sp.Chip == "" { // arm in a VM names neither
		sp.Chip = fmt.Sprintf("%d cores", runtime.NumCPU())
	}
	if kb, err := strconv.ParseInt(strings.TrimSuffix(field("/proc/meminfo", "MemTotal", ':'), " kB"), 10, 64); err == nil {
		sp.RAMGB = gb(kb << 10)
	}
	sp.OSVersion = strings.Trim(field("/etc/os-release", "PRETTY_NAME", '='), `"`)
	return sp
}

// field is the value of the first "key<sep> value" line of a file.
func field(path, key string, sep byte) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), string(sep))
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
