package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// startDetached starts exe args… on its own, apart from this console (closing the window does not end it), without a
// window of its own — as the Run key starts it at login.
func startDetached(exe string, args []string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
