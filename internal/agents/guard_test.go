package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/agents/fakeclaude"
)

// started is what the fake claude logged about one start (AGENTS_FAKE_ARGS).
type started struct {
	Args        []string          `json:"args"`
	MCPRaw      string            `json:"mcpRaw"`
	MCPExpanded string            `json:"mcpExpanded"`
	Env         map[string]string `json:"env"`
}

func (s started) flag(name string) (string, bool) {
	for i, a := range s.Args {
		if a == name {
			if i+1 < len(s.Args) && !strings.HasPrefix(s.Args[i+1], "--") {
				return s.Args[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

// guardedManager is a fake manager in a config folder like ~/.config/mlxlab (with a worker.json), whose claude
// logs how it was started.
func guardedManager(t *testing.T) (*Manager, string) {
	t.Helper()
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "worker.json"), []byte(`{"token":"worker-secret"}`), 0o600)
	os.MkdirAll(filepath.Join(home, "outputs"), 0o755)
	args := filepath.Join(t.TempDir(), "args.jsonl")
	m := &Manager{Root: filepath.Join(home, "agents"), Home: home, Command: os.Args[0],
		Env: []string{fakeclaude.Env, "AGENTS_FAKE_ARGS=" + args}, Version: "9.9.9 (fake)"}
	t.Cleanup(m.Close)
	return m, args
}

func startsOf(t *testing.T, file string) []started {
	t.Helper()
	b, _ := os.ReadFile(file)
	var out []started
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var s started
		if l != "" && json.Unmarshal([]byte(l), &s) == nil {
			out = append(out, s)
		}
	}
	return out
}

// The session's control files hold no secrets and are out of the agent's reach: MCP headers go through the
// environment, the file tools are confined to the session's folder, the control tree and the config folder denied.
func TestSessionGuard(t *testing.T) {
	m, argsLog := guardedManager(t)
	secret := "Bearer board-token-123"
	o := Options{Key: "board-9", Owner: "alice", MCP: []MCPServer{{Name: "vboard", URL: "http://x/mcp",
		Headers: map[string]string{"Authorization": secret, "X-Board": "b9"}}}}
	res, err := m.Turn(context.Background(), o, "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctl := filepath.Join(m.Root, ".ctl", res.Session)
	// nothing secret on disk
	b, err := os.ReadFile(filepath.Join(ctl, "mcp.json"))
	if err != nil || strings.Contains(string(b), "board-token-123") || !strings.Contains(string(b), "${AIC_MCP_0_0}") {
		t.Fatalf("mcp.json: %s %v", b, err)
	}
	if st, _ := os.Stat(ctl); runtime.GOOS != "windows" && st.Mode().Perm() != 0o700 {
		t.Fatalf(".ctl mode %v", st.Mode())
	}
	all := startsOf(t, argsLog)
	if len(all) != 1 {
		t.Fatalf("starts %+v", all)
	}
	s := all[0]
	// Claude Code expands the references from its environment
	if !strings.Contains(s.MCPExpanded, `"Authorization": "`+secret+`"`) || !strings.Contains(s.MCPExpanded, `"X-Board": "b9"`) ||
		s.Env["AIC_MCP_0_0"] != secret || strings.Contains(s.MCPRaw, secret) {
		t.Fatalf("mcp: raw %s expanded %s env %v", s.MCPRaw, s.MCPExpanded, s.Env)
	}
	// confined and denied
	if _, ok := s.flag("--restricted"); !ok {
		t.Fatalf("not --restricted: %v", s.Args)
	}
	if tools, _ := s.flag("--tools"); tools != "Read,Glob,Grep,WebSearch,WebFetch,TodoWrite" {
		t.Fatalf("--tools %q", tools)
	}
	allowed, _ := s.flag("--allowedTools")
	if !strings.HasPrefix(allowed, "Read(./**),Glob(./**),Grep(./**),") || !strings.Contains(allowed, "mcp__vboard") ||
		strings.Contains(allowed, "Read,") {
		t.Fatalf("--allowedTools %q", allowed)
	}
	deny, _ := s.flag("--disallowedTools")
	for _, want := range []string{
		"Read(" + absRule(filepath.Join(m.Root, ".ctl")) + "/**)", "Edit(" + absRule(filepath.Join(m.Root, ".ctl")) + "/**)",
		"Read(" + absRule(filepath.Join(m.Home, "worker.json")) + ")", "Read(" + absRule(filepath.Join(m.Home, "agents.json")) + ")",
		"Read(" + absRule(filepath.Join(m.Home, "outputs")) + "/**)", "Read(" + absRule(filepath.Join(m.Root, "resume.json")) + ")",
		"Read(~/.claude*)", "Read(~/.claude*/**)", "Read(~/.ssh/**)", "Read(~/.config/mlxlab/**)", "Read(//proc/**)",
	} {
		if !strings.Contains(","+deny+",", ","+want+",") {
			t.Errorf("--disallowedTools lacks %s: %s", want, deny)
		}
	}
	// never its own folder (a deny would beat the allow-list)
	own := absRule(filepath.Join(m.Root, res.Session))
	if strings.Contains(deny, own+")") || strings.Contains(deny, own+"/**)") || strings.Contains(deny, "Read("+absRule(m.Root)+"/**)") {
		t.Fatalf("its own folder is denied: %s", deny)
	}

	// a second session denies the first one's folder
	res2, err := m.Turn(context.Background(), Options{Key: "board-10", Owner: "alice"}, "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	all = startsOf(t, argsLog)
	deny2, _ := all[len(all)-1].flag("--disallowedTools")
	if !strings.Contains(deny2, "Read("+own+"/**)") || strings.Contains(deny2, absRule(filepath.Join(m.Root, res2.Session))+"/**)") {
		t.Fatalf("second session's deny rules: %s", deny2)
	}
}

// A person's tty session may start Claude Code's other tools (they answer its prompts), still denied the secrets.
func TestPersonSessionTools(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no tty on Windows")
	}
	m, argsLog := guardedManager(t)
	if _, err := m.OpenTTY("alice", "", ""); err != nil {
		t.Fatal(err)
	}
	var all []started
	waitUntil(t, func() bool { all = startsOf(t, argsLog); return len(all) == 1 })
	if tools, _ := all[0].flag("--tools"); tools != "default" {
		t.Fatalf("--tools %q", tools)
	}
	if deny, _ := all[0].flag("--disallowedTools"); !strings.Contains(deny, "/.ctl/**)") {
		t.Fatalf("deny %s", deny)
	}
}

// A person's tty session works only inside the agents folder or a folder of agents.json.
func TestCheckCwd(t *testing.T) {
	m, _ := guardedManager(t)
	extra := t.TempDir()
	m.Config.Folders = []string{extra}
	inside := filepath.Join(m.Root, "work")
	ctl := filepath.Join(m.Root, ".ctl", "x")
	for _, d := range []string{inside, ctl, filepath.Join(extra, "proj")} {
		os.MkdirAll(d, 0o755)
	}
	outside := t.TempDir()
	link := filepath.Join(m.Root, "escape")
	os.Symlink(outside, link)
	for _, c := range []struct {
		cwd string
		ok  bool
	}{
		{inside, true},
		{filepath.Join(extra, "proj"), true},
		{extra, true},
		{m.Root, false},     // every session's folder
		{ctl, false},        // the control tree
		{m.Home, false},     // the config folder
		{outside, false},    // elsewhere
		{link, false},       // a link out of the agents folder
		{"relative", false}, // not absolute
		{filepath.Join(m.Root, "missing"), false},
	} {
		if _, err := m.CheckCwd(c.cwd); (err == nil) != c.ok {
			t.Errorf("CheckCwd(%s): %v, want ok=%v", c.cwd, err, c.ok)
		}
	}
	if _, err := m.OpenTTY("alice", "", outside); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("OpenTTY elsewhere: %v", err)
	}
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}

// A worker with its own state dir (mlxlab worker --state): its sessions may read neither that dir's files nor the
// shared config dir's (the other worker's worker.json, history, …) — also when the state dir lies inside it.
func TestDenySharedConfig(t *testing.T) {
	for _, nested := range []bool{false, true} {
		shared := t.TempDir()
		state := t.TempDir()
		if nested {
			state = filepath.Join(shared, "local")
		}
		os.MkdirAll(filepath.Join(shared, "models"), 0o755)
		os.WriteFile(filepath.Join(shared, "worker.json"), []byte(`{"token":"pi-secret"}`), 0o600)
		os.MkdirAll(filepath.Join(state, "agents", "s1"), 0o755)
		os.WriteFile(filepath.Join(state, "history.json"), []byte(`[]`), 0o600)
		m := &Manager{Root: filepath.Join(state, "agents"), Home: state, Shared: []string{shared}}
		cwd := filepath.Join(state, "agents", "s1")
		rules := m.denyRules(&Session{cwd: cwd})
		has := func(p string) bool { return slices.Contains(rules, "Read("+absRule(p)+")") }
		for _, p := range []string{filepath.Join(shared, "worker.json"), filepath.Join(shared, "models"), filepath.Join(state, "history.json"), filepath.Join(state, "worker.json")} {
			if !has(p) {
				t.Errorf("nested=%v: %s not denied: %v", nested, p, rules)
			}
		}
		for _, p := range []string{cwd, state, shared, m.Root} {
			if has(p) {
				t.Errorf("nested=%v: %s denied (the session's own folder is in it)", nested, p)
			}
		}
	}
}
