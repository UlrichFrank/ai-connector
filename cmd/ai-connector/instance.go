package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// One `ai-connector vboard` per settings folder: two (one from a terminal, one started at login) would dial vboard
// with the same token and share the worker's state. The first holds a lock on <files>/vboard/instance.lock — an OS
// lock, gone when its process ends however it ends — with its PID in the file; another one waits until it is free
// (instead of ending: launchd's KeepAlive and systemd's Restart would start it again and again).

const instanceLockName = "instance.lock"

// instanceLock is the lock file of the settings folder dir.
func instanceLock(dir string) string { return filepath.Join(dir, instanceLockName) }

// takeInstance takes the lock and writes this process's PID into it; f nil: another process holds it (pid: the one
// it wrote, 0 when it has not yet). Closing f frees the lock.
func takeInstance(path string) (f *os.File, pid int, err error) {
	f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, 0, err
	}
	ok, err := tryLock(f)
	if err != nil || !ok {
		pid = readPID(f)
		f.Close()
		return nil, pid, err
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, 0, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, 0, nil
}

// lockHolder is the PID of the process that holds the lock; held false: none does (a PID in the file is stale then,
// nothing to stop). pid 0 while held: it has not written its PID (yet).
func lockHolder(path string) (pid int, held bool) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, false // no file: none ever ran here
	}
	defer f.Close()
	if ok, err := tryLock(f); ok || err != nil {
		return 0, false // free (closing f frees it again)
	}
	for i := 0; i < 10; i++ { // it may be between its lock and its PID
		if pid = readPID(f); pid > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return pid, true
}

func readPID(f *os.File) int {
	b := make([]byte, 32)
	n, _ := f.ReadAt(b, 0)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b[:n])))
	return pid
}

// waitInstance takes the lock, trying again every while another process holds it (waiting tells about it, once per
// holder); ctx ends the wait.
func waitInstance(ctx context.Context, path string, every time.Duration, waiting func(pid int)) (*os.File, error) {
	told := -1
	for {
		f, pid, err := takeInstance(path)
		if err != nil {
			return nil, err
		}
		if f != nil {
			return f, nil
		}
		if pid != told {
			waiting(pid)
			told = pid
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
	}
}
