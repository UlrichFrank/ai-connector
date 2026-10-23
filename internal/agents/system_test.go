package agents

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// longSystem is a system prompt like vboard's chat sends (instructions, board context, tool hints), 100 KB.
func longSystem() string {
	return "BEGIN " + strings.Repeat("You work on a board. ", 100<<10/21) + " END"
}

// noLongArgs: no argument of a start is longer than the command line takes everywhere.
func noLongArgs(t *testing.T, st started) {
	t.Helper()
	for _, a := range st.Args {
		if len(a) > maxInlineArg {
			t.Fatalf("an argument of %d bytes: %.80s…", len(a), a)
		}
	}
}

// A long system prompt goes through --append-system-prompt-file (in the session's private folder, removed when the
// process ends), never on the command line: Windows' CreateProcess fails on one longer than 32767 characters.
func TestSystemPromptFile(t *testing.T) {
	m, argsLog := guardedManager(t)
	m.flags = "  --append-system-prompt <prompt>  …\n  via: --system-prompt[-file],\n  --append-system-prompt[-file], --add-dir"
	sys := longSystem()
	res, err := m.Turn(context.Background(), Options{Key: "board-1", Owner: "alice", System: sys}, "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ss := startsOf(t, argsLog)
	if len(ss) != 1 {
		t.Fatalf("starts: %d", len(ss))
	}
	noLongArgs(t, ss[0])
	file, ok := ss[0].flag("--append-system-prompt-file")
	if !ok || slices.Contains(ss[0].Args, "--append-system-prompt") {
		t.Fatalf("args: %v", ss[0].Args)
	}
	if ss[0].System != sys {
		t.Fatalf("system prompt: %d bytes, want %d", len(ss[0].System), len(sys))
	}
	ctl := filepath.Join(m.Root, ".ctl", res.Session)
	if file != filepath.Join(ctl, "system.md") {
		t.Fatalf("file %s, not in %s", file, ctl)
	}
	if fi, err := os.Stat(file); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", fi, err)
	}
	if err := m.Stop(res.Session); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("still there: %v", err)
	}
}

// A claude without --append-system-prompt-file: a short system prompt on the command line, a long one before the
// first turn's message (on stdin) — only the first.
func TestSystemPromptFallback(t *testing.T) {
	m, argsLog := guardedManager(t)
	m.flags = "  --append-system-prompt <prompt>  Append a system prompt to the default"
	short := "Answer briefly."
	if _, err := m.Turn(context.Background(), Options{Key: "board-s", Owner: "alice", System: short}, "hi", nil); err != nil {
		t.Fatal(err)
	}
	if v, _ := startsOf(t, argsLog)[0].flag("--append-system-prompt"); v != short {
		t.Fatalf("args: %v", startsOf(t, argsLog)[0].Args)
	}

	sys := longSystem()
	o := Options{Key: "board-l", Owner: "alice", System: sys}
	r := &recorder{}
	if _, err := m.Turn(context.Background(), o, "first", r.emit); err != nil {
		t.Fatal(err)
	}
	st := startsOf(t, argsLog)[1]
	noLongArgs(t, st)
	if _, ok := st.flag("--append-system-prompt"); ok || slices.Contains(st.Args, "--append-system-prompt-file") {
		t.Fatalf("args: %.300v", st.Args)
	}
	if got := r.text(); got != "echo: <system>\n"+sys+"\n</system>\n\nfirst" {
		t.Fatalf("first turn: %d bytes %.60q", len(got), got)
	}
	r = &recorder{}
	if _, err := m.Turn(context.Background(), o, "second", r.emit); err != nil {
		t.Fatal(err)
	}
	if got := r.text(); got != "echo: second" {
		t.Fatalf("second turn: %.60q", got)
	}

	// a terminal session cannot take it on stdin: a clear error instead of a failing start
	s := &Session{m: m, ctl: t.TempDir(), opts: Options{Driver: DriverTTY, System: sys}}
	if _, _, err := m.systemArgs(s, false); err == nil || !strings.Contains(err.Error(), "update Claude Code") {
		t.Fatalf("tty: %v", err)
	}
}

// On Windows a command line over 32 KB is refused with a clear error before CreateProcess fails on it.
func TestCheckArgs(t *testing.T) {
	defer func(g string) { goos = g }(goos)
	goos = "windows"
	if err := checkArgs(`C:\Users\x\.local\bin\claude.exe`, []string{"--model", "opus", "--append-system-prompt-file", `C:\x\system.md`}); err != nil {
		t.Fatal(err)
	}
	if err := checkArgs("claude", []string{"--allowedTools", strings.Repeat("mcp__x,", 6000)}); err == nil ||
		!strings.Contains(err.Error(), "more than Windows allows") {
		t.Fatalf("long: %v", err)
	}
	goos = "darwin"
	if err := checkArgs("claude", []string{"--allowedTools", strings.Repeat("mcp__x,", 6000)}); err != nil {
		t.Fatal(err)
	}
}
