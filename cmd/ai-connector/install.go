package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// `ai-connector vboard --install` starts it at login: a launchd agent on macOS, a systemd user unit on Linux, a
// scheduled task on Windows (the Run key of the user when Windows refuses the task). It runs `ai-connector vboard`
// without flags — the settings, the token too, are in <files>/vboard/settings.json (0600), not in the service's
// file. `--uninstall` stops and removes it.

const (
	serviceLabel  = "de.rnd7.ai-connector.vboard" // launchd
	serviceUnit   = "ai-connector-vboard.service" // systemd --user
	serviceTask   = "ai-connector vboard"         // schtasks, the Run key
	serviceRunKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
)

// serviceEnvKeys are kept from the environment of `--install` (where the files and the models are, what the agents
// find on the PATH).
var serviceEnvKeys = []string{"PATH", "MLXLAB_HOME", "XDG_CONFIG_HOME", "MLXLAB_STATE", "MLXLAB_MODELS_FROM", "MLXLAB_RUNTIMES",
	"HF_HOME", "HF_HUB_CACHE", "HF_TOKEN_PATH", "CLAUDE_CONFIG_DIR"}

// service is the start at login on one system.
type service struct {
	goos string
	home string
	exe  string
	env  [][2]string // KEY, value
	uid  int
	// run runs a command; it returns its output (tests record the commands instead)
	run func(name string, args ...string) (string, error)
}

func newService(getenv func(string) string) (service, error) {
	exe, err := os.Executable()
	if err != nil {
		return service{}, err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if strings.Contains(exe, "go-build") {
		return service{}, fmt.Errorf("%s is a temporary build (go run): build ai-connector and install from the program", exe)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return service{}, err
	}
	s := service{goos: runtime.GOOS, home: home, exe: exe, uid: os.Getuid(), run: func(name string, args ...string) (string, error) {
		out, err := exec.Command(name, args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}}
	for _, k := range serviceEnvKeys {
		if v := getenv(k); v != "" {
			s.env = append(s.env, [2]string{k, v})
		}
	}
	return s, nil
}

// file is where the service is described ("" on Windows: the task holds it).
func (s service) file() string {
	switch s.goos {
	case "darwin":
		return filepath.Join(s.home, "Library", "LaunchAgents", serviceLabel+".plist")
	case "linux":
		cfg := filepath.Join(s.home, ".config")
		for _, kv := range s.env {
			if kv[0] == "XDG_CONFIG_HOME" {
				cfg = kv[1]
			}
		}
		return filepath.Join(cfg, "systemd", "user", serviceUnit)
	}
	return ""
}

// logFile is where a launchd agent writes ("" elsewhere: journalctl --user -u ai-connector-vboard on Linux).
func (s service) logFile() string {
	if s.goos == "darwin" {
		return filepath.Join(s.home, "Library", "Logs", "ai-connector-vboard.log")
	}
	return ""
}

func xmlEsc(v string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(v))
	return b.String()
}

// systemdQuote quotes a word for ExecStart= and Environment= ("…" with \ and " escaped, % doubled).
func systemdQuote(v string) string {
	v = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(v)
	return `"` + v + `"`
}

// content is the launchd plist or the systemd unit.
func (s service) content() string {
	var b strings.Builder
	switch s.goos {
	case "darwin":
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + serviceLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEsc(s.exe) + `</string>
		<string>vboard</string>
	</array>
`)
		if len(s.env) > 0 {
			b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
			for _, kv := range s.env {
				b.WriteString("\t\t<key>" + xmlEsc(kv[0]) + "</key>\n\t\t<string>" + xmlEsc(kv[1]) + "</string>\n")
			}
			b.WriteString("\t</dict>\n")
		}
		b.WriteString(`	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>StandardOutPath</key>
	<string>` + xmlEsc(s.logFile()) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEsc(s.logFile()) + `</string>
</dict>
</plist>
`)
	case "linux":
		b.WriteString("[Unit]\nDescription=ai-connector for vboard (vboard's local AI)\nWants=network-online.target\nAfter=network-online.target\n\n[Service]\n")
		b.WriteString("ExecStart=" + systemdQuote(s.exe) + " vboard\n")
		for _, kv := range s.env {
			b.WriteString("Environment=" + systemdQuote(kv[0]+"="+kv[1]) + "\n")
		}
		b.WriteString("Restart=always\nRestartSec=10\n\n[Install]\nWantedBy=default.target\n")
	}
	return b.String()
}

// command is the Windows task's command line.
func (s service) command() string { return `"` + s.exe + `" vboard` }

// installCmds start the service (after its file is written).
func (s service) installCmds() [][]string {
	switch s.goos {
	case "darwin":
		domain := fmt.Sprintf("gui/%d", s.uid)
		return [][]string{{"launchctl", "bootout", domain + "/" + serviceLabel}, {"launchctl", "bootstrap", domain, s.file()}}
	case "linux":
		return [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", serviceUnit},
			{"systemctl", "--user", "restart", serviceUnit}}
	case "windows":
		return [][]string{{"schtasks", "/Create", "/TN", serviceTask, "/TR", s.command(), "/SC", "ONLOGON", "/RL", "LIMITED", "/F"},
			{"schtasks", "/Run", "/TN", serviceTask}}
	}
	return nil
}

// uninstallCmds stop and remove it (their failures do not count: it may not be there).
func (s service) uninstallCmds() [][]string {
	switch s.goos {
	case "darwin":
		return [][]string{{"launchctl", "bootout", fmt.Sprintf("gui/%d/%s", s.uid, serviceLabel)}}
	case "linux":
		return [][]string{{"systemctl", "--user", "disable", "--now", serviceUnit}}
	case "windows":
		return [][]string{{"schtasks", "/End", "/TN", serviceTask}, {"schtasks", "/Delete", "/TN", serviceTask, "/F"},
			{"reg", "delete", serviceRunKey, "/v", serviceTask, "/f"}}
	}
	return nil
}

// install writes the service's file and starts it.
func (s service) install() error {
	cmds := s.installCmds()
	if cmds == nil {
		return fmt.Errorf("--install: not on %s (macOS, Linux and Windows only)", s.goos)
	}
	if p := s.file(); p != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(s.content()), 0o600); err != nil {
			return err
		}
		_ = os.Chmod(p, 0o600)
	}
	if l := s.logFile(); l != "" {
		_ = os.MkdirAll(filepath.Dir(l), 0o755)
	}
	for i, c := range cmds {
		out, err := s.run(c[0], c[1:]...)
		if err == nil || s.goos == "darwin" && i == 0 { // bootout: an older one may not be there
			continue
		}
		if s.goos == "windows" && i == 0 {
			// a scheduled task at logon may need rights this user does not have: the user's Run key does it too
			if _, rerr := s.run("reg", "add", serviceRunKey, "/v", serviceTask, "/t", "REG_SZ", "/d", s.command(), "/f"); rerr == nil {
				fmt.Printf("the scheduled task was refused (%s): added to %s instead (starts at the next login)\n", out, serviceRunKey)
				return nil
			}
		}
		return fmt.Errorf("%s: %v: %s", strings.Join(c, " "), err, out)
	}
	return nil
}

// uninstall stops it and removes its file.
func (s service) uninstall() error {
	for _, c := range s.uninstallCmds() {
		_, _ = s.run(c[0], c[1:]...)
	}
	if p := s.file(); p != "" {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if s.goos == "linux" {
			_, _ = s.run("systemctl", "--user", "daemon-reload")
		}
	}
	return nil
}

// describe says what was installed, for the person who ran it.
func (s service) describe() string {
	switch s.goos {
	case "darwin":
		return fmt.Sprintf("installed %s: ai-connector vboard starts at login (now too)\n  log: %s", s.file(), s.logFile())
	case "linux":
		return fmt.Sprintf("installed %s: ai-connector vboard starts at login (now too)\n  log: journalctl --user -u %s", s.file(), serviceUnit)
	}
	return "installed the scheduled task \"" + serviceTask + "\": ai-connector vboard starts at login (now too)"
}
