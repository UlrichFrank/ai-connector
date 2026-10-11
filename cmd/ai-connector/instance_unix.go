//go:build !windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes f's lock without waiting (flock: it ends with the process); false: another process holds it.
func tryLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

// stopProcess asks the process to end (SIGTERM: it stops as on Ctrl-C).
func stopProcess(pid int) error { return unix.Kill(pid, unix.SIGTERM) }
