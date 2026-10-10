//go:build windows

package agents

import "os"

// startPTY: a tty session needs a pseudo terminal; ConPTY is not wired up yet.
func startPTY(name string, args []string, dir string, env []string, cols, rows int) (ptyProcess, error) {
	return nil, ErrTTYWindows
}

// interrupt: Windows has no SIGINT for another process; it is stopped.
func interrupt(p *os.Process) {
	if p != nil {
		p.Kill()
	}
}
