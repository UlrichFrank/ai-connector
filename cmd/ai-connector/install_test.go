package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// testService is a service on goos in a temporary home whose commands are recorded (fail: commands that fail).
func testService(t *testing.T, goos string, fail map[string]bool) (*service, *[]string) {
	t.Helper()
	var ran []string
	s := &service{goos: goos, home: t.TempDir(), exe: "/Applications/AI & Co/ai-connector", uid: 501,
		env: [][2]string{{"PATH", "/opt/homebrew/bin:/usr/bin"}, {"MLXLAB_HOME", "/Users/me/my <files>"}}}
	s.run = func(name string, args ...string) (string, error) {
		line := name + " " + strings.Join(args, " ")
		ran = append(ran, line)
		for f := range fail {
			if strings.HasPrefix(line, f) {
				return "Access is denied.", errors.New("exit status 1")
			}
		}
		return "", nil
	}
	return s, &ran
}

func TestInstallLaunchd(t *testing.T) {
	s, ran := testService(t, "darwin", map[string]bool{"launchctl bootout": true}) // none there yet
	if err := s.install(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.home, "Library", "LaunchAgents", "de.rnd7.ai-connector.vboard.plist")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	plist := string(b)
	for _, want := range []string{"<string>de.rnd7.ai-connector.vboard</string>", "<string>/Applications/AI &amp; Co/ai-connector</string>\n\t\t<string>vboard</string>",
		"<key>MLXLAB_HOME</key>\n\t\t<string>/Users/me/my &lt;files&gt;</string>", "<key>RunAtLoad</key>\n\t<true/>", "<key>KeepAlive</key>",
		filepath.Join(s.home, "Library", "Logs", "ai-connector-vboard.log")} {
		if !strings.Contains(plist, want) {
			t.Errorf("the plist lacks %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "vbp_") || strings.Contains(plist, "--token") {
		t.Fatal("the token is in the plist")
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if got := strings.Join(*ran, "\n"); got != "launchctl bootout gui/501/de.rnd7.ai-connector.vboard\nlaunchctl bootstrap gui/501 "+p {
		t.Fatalf("ran %q", got)
	}
	*ran = nil
	if err := s.uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("the plist is still there")
	}
	if got := strings.Join(*ran, "\n"); got != "launchctl bootout gui/501/de.rnd7.ai-connector.vboard" {
		t.Fatalf("ran %q", got)
	}
	if err := s.uninstall(); err != nil { // nothing there: fine
		t.Fatal(err)
	}
}

func TestInstallSystemd(t *testing.T) {
	s, ran := testService(t, "linux", nil)
	s.env = append(s.env, [2]string{"HF_HOME", `/data/h"f 100%`})
	if err := s.install(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.home, ".config", "systemd", "user", "ai-connector-vboard.service")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	unit := string(b)
	for _, want := range []string{`ExecStart="/Applications/AI & Co/ai-connector" vboard`, `Environment="PATH=/opt/homebrew/bin:/usr/bin"`,
		`Environment="HF_HOME=/data/h\"f 100%%"`, "Restart=always", "WantedBy=default.target"} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit lacks %q:\n%s", want, unit)
		}
	}
	if got := strings.Join(*ran, "\n"); got != "systemctl --user daemon-reload\nsystemctl --user enable ai-connector-vboard.service\nsystemctl --user restart ai-connector-vboard.service" {
		t.Fatalf("ran %q", got)
	}
	*ran = nil
	if err := s.uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("the unit is still there")
	}
	if got := strings.Join(*ran, "\n"); got != "systemctl --user disable --now ai-connector-vboard.service\nsystemctl --user daemon-reload" {
		t.Fatalf("ran %q", got)
	}

	// $XDG_CONFIG_HOME moves the unit
	s, _ = testService(t, "linux", nil)
	s.env = append(s.env, [2]string{"XDG_CONFIG_HOME", "/x/cfg"})
	if s.file() != "/x/cfg/systemd/user/ai-connector-vboard.service" {
		t.Fatalf("file %q", s.file())
	}
}

func TestInstallWindows(t *testing.T) {
	windows := func(fail map[string]bool) (*service, *[]string) {
		s, ran := testService(t, "windows", fail)
		s.exe = `C:\Program Files\ai-connector\ai-connector.exe`
		s.env = [][2]string{{"PATH", `C:\Windows;C:\Tools`}, {"MLXLAB_HOME", `D:\AI files\`}, {"HF_HOME", `D:\hf`}}
		s.startNow = func(exe string, args []string) error {
			*ran = append(*ran, "start "+exe+" "+strings.Join(args, " "))
			return nil
		}
		return s, ran
	}
	// the kept environment as --env (PATH not: the task has the user's), quoted for the command line
	cmd := `"C:\Program Files\ai-connector\ai-connector.exe" vboard --env "MLXLAB_HOME=D:\AI files\\" --env HF_HOME=D:\hf`
	s, ran := windows(nil)
	if s.command() != cmd {
		t.Fatalf("command %s", s.command())
	}
	if err := s.install(); err != nil {
		t.Fatal(err)
	}
	want := `schtasks /Create /TN ai-connector vboard /TR ` + cmd + ` /SC ONLOGON /RL LIMITED /F` + "\nschtasks /Run /TN ai-connector vboard"
	if got := strings.Join(*ran, "\n"); got != want {
		t.Fatalf("ran %q", got)
	}
	if !strings.Contains(s.describe(), "scheduled task") {
		t.Fatal(s.describe())
	}
	// a task at logon refused: the user's Run key
	s, ran = windows(map[string]bool{"schtasks /Create": true})
	if err := s.install(); err != nil {
		t.Fatal(err)
	}
	if got := (*ran)[len(*ran)-2]; got != `reg add HKCU\Software\Microsoft\Windows\CurrentVersion\Run /v ai-connector vboard /t REG_SZ /d `+cmd+` /f` {
		t.Fatalf("ran %q", *ran)
	}
	// "now and at every login" without a task: it starts now too, apart from the console (not only at the next login)
	if got := (*ran)[len(*ran)-1]; got != `start C:\Program Files\ai-connector\ai-connector.exe vboard --env MLXLAB_HOME=D:\AI files\ --env HF_HOME=D:\hf` {
		t.Fatalf("ran %q", *ran)
	}
	// a copy runs already (an update): it is stopped, the new one starts
	s, ran = windows(map[string]bool{"schtasks /Create": true})
	held, stopped := true, 0
	s.holder = func() (int, bool) { return 4711, held }
	s.stop = func(pid int) error { stopped = pid; held = false; return nil }
	if err := s.install(); err != nil {
		t.Fatal(err)
	}
	if stopped != 4711 || !strings.HasPrefix((*ran)[len(*ran)-1], "start ") {
		t.Fatalf("stopped %d, ran %q", stopped, *ran)
	}
	if !strings.Contains(s.describe(), `in HKCU\Software`) {
		t.Fatal(s.describe())
	}
	// longer than schtasks /TR takes (261): the Run key at once
	s, ran = windows(nil)
	s.env = append(s.env, [2]string{"HF_HUB_CACHE", `D:\` + strings.Repeat("models ", 30)})
	if len(s.command()) <= 261 {
		t.Fatal(len(s.command()))
	}
	if err := s.install(); err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 2 || !strings.HasPrefix((*ran)[0], "reg add ") || !strings.HasPrefix((*ran)[1], "start ") || !strings.Contains((*ran)[0], `--env "HF_HUB_CACHE=D:\models models`) {
		t.Fatalf("ran %q", *ran)
	}
	*ran = nil
	s.uninstall()
	if got := strings.Join(*ran, "\n"); !strings.Contains(got, "schtasks /Delete /TN ai-connector vboard /F") || !strings.Contains(got, "reg delete") {
		t.Fatalf("ran %q", got)
	}
}

func TestWinQuote(t *testing.T) {
	// as CommandLineToArgvW reads them back (Go's syscall.EscapeArg does the same)
	for in, want := range map[string]string{"a": "a", "": `""`, `C:\x`: `C:\x`, "a b": `"a b"`, `C:\x y\`: `"C:\x y\\"`,
		`say "hi"`: `"say \"hi\""`, `a\"b`: `"a\\\"b"`, `a\\b c`: `"a\\b c"`, "a\tb": "\"a\tb\""} {
		if got := winQuote(in); got != want {
			t.Errorf("winQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestInstallReplaces: --install stops the copy that holds the lock (the PID it wrote), never one whose lock is free.
func TestInstallReplaces(t *testing.T) {
	type copyState struct {
		pid        int
		held       bool
		endsOnStop bool
	}
	setup := func(goos string, c *copyState) (*service, *[]string, *strings.Builder) {
		s, ran := testService(t, goos, nil)
		var out strings.Builder
		s.out, s.stopWait = &out, 300*time.Millisecond
		s.holder = func() (int, bool) { return c.pid, c.held }
		s.stop = func(pid int) error {
			*ran = append(*ran, fmt.Sprintf("stop %d", pid))
			if c.endsOnStop {
				c.held = false
			}
			return nil
		}
		return s, ran, &out
	}

	// a copy from a terminal: stopped before launchd starts the service
	c := &copyState{pid: 4242, held: true, endsOnStop: true}
	s, ran, out := setup("darwin", c)
	if err := s.install(); err != nil {
		t.Fatal(err)
	}
	if (*ran)[0] != "stop 4242" || !strings.HasPrefix((*ran)[2], "launchctl bootstrap") {
		t.Fatalf("ran %q", *ran)
	}
	if !strings.Contains(out.String(), "stopped the running copy (pid 4242): the service takes over") {
		t.Fatalf("said %q", out.String())
	}

	// a stale PID (the lock is free): nothing is stopped
	c = &copyState{pid: 4242, held: false}
	s, ran, out = setup("linux", c)
	if err := s.install(); err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 3 || slices.Contains(*ran, "stop 4242") || out.Len() != 0 {
		t.Fatalf("ran %q, said %q", *ran, out.String())
	}

	// held, its PID unknown (yet): not stopped
	c = &copyState{pid: 0, held: true}
	s, ran, out = setup("linux", c)
	_ = s.install()
	if slices.ContainsFunc(*ran, func(l string) bool { return strings.HasPrefix(l, "stop") }) || !strings.Contains(out.String(), "PID is unknown") {
		t.Fatalf("ran %q, said %q", *ran, out.String())
	}

	// one that does not end: said so, installed all the same (the service waits for it)
	c = &copyState{pid: 4242, held: true}
	s, ran, out = setup("linux", c)
	if err := s.install(); err != nil {
		t.Fatal(err)
	}
	if (*ran)[0] != "stop 4242" || len(*ran) != 4 || !strings.Contains(out.String(), "did not end within") {
		t.Fatalf("ran %q, said %q", *ran, out.String())
	}

	// Windows: stopped once the task is there, before it runs
	c = &copyState{pid: 4242, held: true, endsOnStop: true}
	s, ran, _ = setup("windows", c)
	if err := s.install(); err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 3 || !strings.HasPrefix((*ran)[0], "schtasks /Create") || (*ran)[1] != "stop 4242" || !strings.HasPrefix((*ran)[2], "schtasks /Run") {
		t.Fatalf("ran %q", *ran)
	}
	// the Run key starts it at the next login only: the running copy is left alone
	c = &copyState{pid: 4242, held: true, endsOnStop: true}
	s, ran, out = setup("windows", c)
	s.run = func(name string, args ...string) (string, error) {
		*ran = append(*ran, name+" "+strings.Join(args, " "))
		if name == "schtasks" {
			return "Access is denied.", errors.New("exit status 1")
		}
		return "", nil
	}
	if err := s.install(); err != nil {
		t.Fatal(err)
	}
	// the Run key instead of a task: the running copy (an older version, perhaps) makes room for the update all the same
	if !slices.Contains(*ran, "stop 4242") || !strings.Contains(out.String(), "stopped the running copy (pid 4242)") {
		t.Fatalf("ran %q, said %q", *ran, out.String())
	}
}

func TestApplyEnv(t *testing.T) {
	got := map[string]string{}
	set := func(k, v string) error { got[k] = v; return nil }
	if err := applyEnv([]string{`MLXLAB_HOME=D:\AI files\`, "HF_HOME=a=b", "PATH="}, set); err != nil {
		t.Fatal(err)
	}
	if got["MLXLAB_HOME"] != `D:\AI files\` || got["HF_HOME"] != "a=b" || len(got) != 3 {
		t.Fatalf("%q", got)
	}
	for _, bad := range []string{"VBOARD_TOKEN=x", "MLXLAB_HOME", "=x"} {
		if err := applyEnv([]string{bad}, set); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestInstallElsewhere(t *testing.T) {
	s, _ := testService(t, "plan9", nil)
	if err := s.install(); err == nil {
		t.Fatal("installed on plan9")
	}
}

func TestDefaultAgents(t *testing.T) {
	ready := func(v bool) func() bool { return func() bool { return v } }
	// Claude Code ready, no --agents: the chat model first (the default one when none is chosen), then Sonnet and Opus
	as, err := defaultAgents("", "", ready(true))
	if err != nil || len(as) != 3 || as[0].ID != "default" || as[0].Model != "" || as[1].Model != "claude-code/sonnet" || as[2].Model != "claude-code/opus" {
		t.Fatalf("%+v %v", as, err)
	}
	if as, _ := defaultAgents("", "qwen3.5-4b", ready(true)); as[0].Model != "qwen3.5-4b" {
		t.Fatalf("chat %+v", as)
	}
	// not ready: the chat model alone
	if as, _ := defaultAgents("", "", ready(false)); len(as) != 0 {
		t.Fatalf("not ready %+v", as)
	}
	// --agents decides
	asked := false
	if as, _ := defaultAgents("qwen3.5-4b", "", func() bool { asked = true; return true }); len(as) != 1 || asked {
		t.Fatalf("--agents %+v (asked %v)", as, asked)
	}
}
