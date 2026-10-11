package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestInstanceLock: one holds the lock with its PID in the file; another open of it (as another process would) is
// refused and sees that PID; once it is closed, the lock is free and the PID left in the file counts for nothing.
func TestInstanceLock(t *testing.T) {
	path := instanceLock(t.TempDir())
	if _, held := lockHolder(path); held {
		t.Fatal("held before anyone took it")
	}
	f, _, err := takeInstance(path)
	if err != nil || f == nil {
		t.Fatal(f, err)
	}
	if g, pid, err := takeInstance(path); g != nil || err != nil || pid != os.Getpid() {
		t.Fatalf("second take: %v %d %v", g, pid, err)
	}
	if pid, held := lockHolder(path); !held || pid != os.Getpid() {
		t.Fatalf("holder %d %v", pid, held)
	}
	f.Close()
	if pid, held := lockHolder(path); held {
		t.Fatalf("held by %d after close", pid)
	}

	// a stale PID from a copy that died: free
	if err := os.WriteFile(path, []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, held := lockHolder(path); held {
		t.Fatal("a stale PID counts as held")
	}
	f, _, err = takeInstance(path)
	if err != nil || f == nil {
		t.Fatal(f, err)
	}
	defer f.Close()
	if b, _ := os.ReadFile(path); string(b) != strconv.Itoa(os.Getpid())+"\n" {
		t.Fatalf("file %q", b)
	}
}

// TestWaitInstance: the second waits (and says so once) until the first ends, then takes over; a stop ends the wait.
func TestWaitInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), instanceLockName)
	first, _, err := takeInstance(path)
	if err != nil || first == nil {
		t.Fatal(first, err)
	}
	told := make(chan int, 10)
	done := make(chan error, 1)
	go func() {
		f, err := waitInstance(context.Background(), path, 10*time.Millisecond, func(pid int) { told <- pid })
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	if pid := <-told; pid != os.Getpid() {
		t.Fatalf("waiting for %d", pid)
	}
	select {
	case err := <-done:
		t.Fatalf("took it while held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if len(told) != 0 {
		t.Fatal("said it more than once")
	}
	first.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not take over")
	}

	first, _, _ = takeInstance(path)
	defer first.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	if f, err := waitInstance(ctx, path, 10*time.Millisecond, func(int) {}); f != nil || !errors.Is(err, context.Canceled) {
		t.Fatal(f, err)
	}
}
