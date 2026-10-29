//go:build !windows

package engine

import (
	"os/exec"
	"syscall"
)

// ownGroup puts a server in a process group of its own (killGroup ends it with its children); on Linux it also dies
// with the worker (Pdeathsig, proc_linux.go).
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	dieWithParent(cmd.SysProcAttr)
}

func killGroup(pid int) { _ = syscall.Kill(-pid, syscall.SIGKILL) }

// alive: a process of that pid exists.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }
