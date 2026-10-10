package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	s, ran := testService(t, "windows", nil)
	s.exe = `C:\Program Files\ai-connector\ai-connector.exe`
	if err := s.install(); err != nil {
		t.Fatal(err)
	}
	want := `schtasks /Create /TN ai-connector vboard /TR "C:\Program Files\ai-connector\ai-connector.exe" vboard /SC ONLOGON /RL LIMITED /F` +
		"\nschtasks /Run /TN ai-connector vboard"
	if got := strings.Join(*ran, "\n"); got != want {
		t.Fatalf("ran %q", got)
	}
	// a task at logon refused: the user's Run key
	s, ran = testService(t, "windows", map[string]bool{"schtasks /Create": true})
	if err := s.install(); err != nil {
		t.Fatal(err)
	}
	if got := (*ran)[len(*ran)-1]; !strings.HasPrefix(got, `reg add HKCU\Software\Microsoft\Windows\CurrentVersion\Run /v ai-connector vboard /t REG_SZ /d "`) {
		t.Fatalf("ran %q", *ran)
	}
	*ran = nil
	s.uninstall()
	if got := strings.Join(*ran, "\n"); !strings.Contains(got, "schtasks /Delete /TN ai-connector vboard /F") || !strings.Contains(got, "reg delete") {
		t.Fatalf("ran %q", got)
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
