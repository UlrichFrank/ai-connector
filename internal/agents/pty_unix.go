//go:build !windows

package agents

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

type unixPTY struct {
	*os.File
	cmd *exec.Cmd
}

func startPTY(name string, args []string, dir string, env []string, cols, rows int) (ptyProcess, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	return &unixPTY{File: f, cmd: cmd}, nil
}

func (p *unixPTY) Resize(cols, rows int) error {
	return pty.Setsize(p.File, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (p *unixPTY) Wait() error { return p.cmd.Wait() }

func (p *unixPTY) Interrupt() { p.cmd.Process.Signal(syscall.SIGTERM) }

func (p *unixPTY) Kill() { p.cmd.Process.Kill() }

// interrupt asks a process to stop what it does (SIGINT).
func interrupt(p *os.Process) {
	if p != nil {
		p.Signal(syscall.SIGINT)
	}
}
