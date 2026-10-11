package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLock takes f's lock without waiting (LockFileEx: Windows frees it when the process ends); false: another process
// holds it. The locked byte lies at 4 GiB, far beyond the PID, which stays readable for the others.
func tryLock(f *os.File) (bool, error) {
	ol := windows.Overlapped{OffsetHigh: 1}
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

// stopProcess ends the process (Windows has no SIGTERM for a program without a window).
func stopProcess(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}
