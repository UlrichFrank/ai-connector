package engine

import (
	"os"
	"os/exec"
	"strconv"
)

func ownGroup(*exec.Cmd) {}

// killGroup ends the process and its children (a venv's python.exe starts the base interpreter as its child).
func killGroup(pid int) {
	if exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run() != nil {
		if p, err := os.FindProcess(pid); err == nil {
			p.Kill()
		}
	}
}

func alive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH").Output()
	return err == nil && len(out) > 0 && !containsNoTasks(string(out))
}

func containsNoTasks(s string) bool { return len(s) < 10 || s[:5] == "INFO:" }
