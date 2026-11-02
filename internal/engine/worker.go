package engine

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/philipparndt/ai-connector/internal/pyenv"
)

// The Python workers of the backends that are not ported to Go: each keeps its model loaded and answers requests.
//
// Protocol (the same for every worker): one JSON request per line on stdin; events as JSON lines on file descriptor 3
// ({"type": "status"|"log"|"progress"|"result"|"error"|"ready", …}); stdout and stderr are the libraries' own output,
// shown as log lines.
//
//go:embed workers
var workerFiles embed.FS

// pyWorker is one running worker process.
type pyWorker struct {
	env    pyenv.Env
	script string                 // file name in workers/
	src    func() ([]byte, error) // the script's source when it is not in workers/ (a local runtime)
	args   []string               // after the script
	extra  []string               // environment variables

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events *bufio.Scanner
	logs   chan string
	exited chan struct{}
}

type workerEvent struct {
	Type     string  `json:"type"`
	Text     string  `json:"text"`
	Step     int     `json:"step"`
	Total    int     `json:"total"`
	File     string  `json:"file"`
	Seconds  float64 `json:"seconds"`
	Duration float64 `json:"duration"`
	PeakGB   float64 `json:"peakGB"`
	Width    int     `json:"width"`  // an image it made (the cutout worker says so)
	Height   int     `json:"height"` //
	Words    []Word  `json:"words"`  // speech: when each word is said (Kokoro)
}

// running: the process is alive.
func (w *pyWorker) running() bool {
	if w.cmd == nil {
		return false
	}
	select {
	case <-w.exited:
		w.cmd = nil
		return false
	default:
		return true
	}
}

// start installs the environment if needed and starts the worker (if it is not running yet).
func (w *pyWorker) start(ctx context.Context, root string, emit Emit) error {
	if w.running() {
		return nil
	}
	if err := w.env.Ensure(ctx, root, emit.Log); err != nil {
		return err
	}
	read := w.src
	if read == nil {
		read = func() ([]byte, error) { return workerFiles.ReadFile("workers/" + w.script) }
	}
	src, err := read()
	if err != nil {
		return err
	}
	script := filepath.Join(w.env.Dir(root), "mlxlab_"+w.script)
	if err := os.WriteFile(script, src, 0o644); err != nil {
		return err
	}
	er, ew, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd := exec.Command(w.env.Bin(root, "python"), append([]string{"-u", script}, w.args...)...)
	cmd.ExtraFiles = []*os.File{ew} // fd 3
	// the store has the files: the libraries must not reach out to Hugging Face by themselves
	cmd.Env = append(append(os.Environ(), "PYTHONUNBUFFERED=1", "TQDM_MININTERVAL=1", "HF_HUB_OFFLINE=1",
		"AGX_RELAX_CDM_CTXSTORE_TIMEOUT=1"), w.extra...) // a longer GPU watchdog (long Metal jobs while the display is on)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		er.Close()
		ew.Close()
		return err
	}
	ew.Close()
	w.cmd, w.stdin, w.exited = cmd, stdin, make(chan struct{})
	w.logs = make(chan string, 256)
	w.events = bufio.NewScanner(er)
	w.events.Buffer(make([]byte, 64<<10), 4<<20)
	go pyenv.Lines(out, func(s string) {
		select {
		case w.logs <- s:
		default: // nobody listens between jobs
		}
	})
	exited := w.exited
	go func() {
		_ = cmd.Wait()
		close(exited)
		// the events it wrote just before it ended are still in the pipe (a trainer ends right after its result):
		// the reader gets them, then EOF; the pipe closes a little later in case a child process still holds it
		time.AfterFunc(5*time.Second, func() { er.Close() })
	}()
	return nil
}

// stop ends the worker (its model leaves memory).
func (w *pyWorker) stop() {
	if w.cmd == nil {
		return
	}
	w.stdin.Close()
	select {
	case <-w.exited:
	case <-time.After(3 * time.Second):
		_ = w.cmd.Process.Kill()
		<-w.exited
	}
	w.cmd = nil
}

// call sends one request and passes the worker's events on until its result (returned) or error. A cancel kills
// the worker: the libraries have no other way to stop.
func (w *pyWorker) call(ctx context.Context, req any, emit Emit) (workerEvent, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return workerEvent{}, err
	}
	if _, err := w.stdin.Write(append(b, '\n')); err != nil {
		w.stop()
		return workerEvent{}, fmt.Errorf("the worker: %w", err)
	}
	evs := make(chan workerEvent)
	go func() {
		defer close(evs)
		for w.events.Scan() {
			var ev workerEvent
			if json.Unmarshal(w.events.Bytes(), &ev) == nil {
				evs <- ev
				if ev.Type == "result" || ev.Type == "error" {
					return
				}
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			_ = w.cmd.Process.Kill()
			<-w.exited
			for range evs {
			}
			w.cmd = nil
			return workerEvent{}, ctx.Err()
		case s := <-w.logs:
			emit.Log(s)
		case ev, ok := <-evs:
			if !ok {
				w.stop()
				return workerEvent{}, errors.New("the worker stopped (see the log)")
			}
			switch ev.Type {
			case "status":
				emit.Status(ev.Text)
			case "log":
				emit.Log(ev.Text)
			case "progress":
				emit.Progress(ev.Step, ev.Total)
			case "error":
				return ev, errors.New(ev.Text)
			case "result":
				return ev, nil
			}
		}
	}
}
