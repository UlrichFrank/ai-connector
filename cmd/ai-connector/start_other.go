//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// startDetached starts exe args… on its own, in a session of its own (only Windows' Run-key fallback uses it).
func startDetached(exe string, args []string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
