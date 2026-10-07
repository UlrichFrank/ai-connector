package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// stdioTransport runs the server as a child process: newline-delimited JSON-RPC on its stdin and stdout; its
// stderr goes to a log.
type stdioTransport struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	wmu   sync.Mutex
	mux   *mux
	done  chan struct{} // closed when the process has exited
	werr  error
	once  sync.Once
}

func startStdio(s *Server, stderr io.Writer) (*stdioTransport, error) {
	env, err := expandAll("env", s.Env)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(s.Command, s.Args...)
	cmd.Dir = s.Dir
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if stderr == nil {
		stderr = io.Discard
	}
	cmd.Stderr = stderr
	// its own process group, so that closing also ends what it started (npx → node)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", s.Command, err)
	}
	t := &stdioTransport{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	t.mux = newMux(func(m *message) { _ = t.write(m) })
	go t.read(stdout, stderr)
	return t, nil
}

func (t *stdioTransport) read(stdout io.Reader, log io.Writer) {
	r := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var m message
			if json.Unmarshal(line, &m) == nil && m.JSONRPC == "2.0" {
				t.mux.deliver(&m)
			} else {
				fmt.Fprintf(log, "[stdout, not JSON-RPC] %s", line)
			}
		}
		if err != nil {
			break
		}
	}
	t.werr = t.cmd.Wait()
	reason := "the server exited"
	if t.werr != nil {
		reason += ": " + t.werr.Error()
	}
	t.mux.fail(fmt.Errorf("%w: %s", errClosed, reason))
	close(t.done)
}

func (t *stdioTransport) write(m *message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	t.wmu.Lock()
	defer t.wmu.Unlock()
	_, err = t.stdin.Write(append(b, '\n'))
	if err != nil {
		return fmt.Errorf("%w: %v", errClosed, err)
	}
	return nil
}

func (t *stdioTransport) request(ctx context.Context, m *message) (*message, error) {
	ch, err := t.mux.wait(m.ID)
	if err != nil {
		return nil, err
	}
	if err := t.write(m); err != nil {
		t.mux.forget(m.ID)
		return nil, err
	}
	return t.mux.await(ctx, m.ID, ch)
}

func (t *stdioTransport) notify(_ context.Context, m *message) error { return t.write(m) }
func (t *stdioTransport) negotiated(string)                          {}

// close closes stdin and waits for the server to exit; after 2 s it gets SIGTERM, after 2 more SIGKILL.
func (t *stdioTransport) close() error {
	t.once.Do(func() {
		t.wmu.Lock()
		_ = t.stdin.Close()
		t.wmu.Unlock()
		for _, sig := range []syscall.Signal{0, syscall.SIGTERM, syscall.SIGKILL} {
			if sig != 0 {
				_ = syscall.Kill(-t.cmd.Process.Pid, sig)
			}
			select {
			case <-t.done:
				return
			case <-time.After(2 * time.Second):
			}
		}
	})
	select {
	case <-t.done:
	case <-time.After(time.Second):
		return errors.New("the server did not exit")
	}
	return nil
}
