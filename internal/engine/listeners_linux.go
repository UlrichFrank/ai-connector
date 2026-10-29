package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procListeners finds the processes listening on a port from /proc (a Linux without lsof): the socket's inode in
// /proc/net/tcp{,6} (state 0A: listen), then the processes with a descriptor of that socket.
func procListeners(port int) ([]int, bool) {
	inodes := map[string]bool{}
	hexPort := fmt.Sprintf(":%04X", port)
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(b), "\n")[1:] {
			fs := strings.Fields(l)
			if len(fs) > 9 && strings.HasSuffix(fs[1], hexPort) && fs[3] == "0A" {
				inodes["socket:["+fs[9]+"]"] = true
			}
		}
	}
	if len(inodes) == 0 {
		return nil, true
	}
	var pids []int
	procs, _ := os.ReadDir("/proc")
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		fds, _ := os.ReadDir(filepath.Join("/proc", p.Name(), "fd"))
		for _, fd := range fds {
			if l, err := os.Readlink(filepath.Join("/proc", p.Name(), "fd", fd.Name())); err == nil && inodes[l] {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids, true
}
