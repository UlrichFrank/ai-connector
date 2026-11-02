package agents

import (
	"errors"
	"strings"
	"testing"
)

// The command line is counted as Windows gets it: each argument quoted as Go quotes it there.
func TestWinCommandLine(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"plain", "plain"},
		{"", `""`},
		{"a b", `"a b"`},
		{`a"b`, `a\"b`},
		{`C:\x y\`, `"C:\x y\\"`},
		{`say "hi" now`, `"say \"hi\" now"`},
	} {
		if got := winEscape(c.in); got != c.want {
			t.Errorf("%q: %q, want %q", c.in, got, c.want)
		}
	}
	if got := winCommandLine(`C:\Program Files\claude.exe`, []string{"-p", "a b"}); got != `"C:\Program Files\claude.exe" -p "a b"` {
		t.Fatalf("line %q", got)
	}
}

// On Windows an environment variable longer than it takes is named before the start; a start that fails as "too
// long" anyway says the sizes. Elsewhere neither.
func TestWindowsStartLimits(t *testing.T) {
	defer func(g string) { goos = g }(goos)
	goos = "windows"
	env := []string{"HOME=C:\\Users\\x", "PATH=" + strings.Repeat(`C:\tools;`, 4000), "AIC_MCP_0_0=Bearer x"}
	if err := checkEnv(env); err == nil || !strings.Contains(err.Error(), "PATH is 36005 characters") {
		t.Fatalf("checkEnv: %v", err)
	}
	if err := checkEnv(env[:1]); err != nil {
		t.Fatal(err)
	}
	// quoting counts: 20,000 quotes are 40,000 characters on the command line
	if err := checkArgs("claude", []string{strings.Repeat(`"`, 20000)}); err == nil || !strings.Contains(err.Error(), "more than Windows allows") {
		t.Fatalf("checkArgs: %v", err)
	}
	err := startError(winErrTooLong, `C:\Users\x\.local\bin\claude.exe`, []string{"-p"}, env, `C:\Users\x\.config\mlxlab\agents\s0123`)
	msg := err.Error()
	if !errors.Is(err, winErrTooLong) || !strings.Contains(msg, "the largest: PATH 36005") || !strings.Contains(msg, "the command line is 35 of") ||
		!strings.Contains(msg, `working folder 38 characters (C:\Users\x\.config\mlxlab\agents\s0123)`) {
		t.Fatalf("startError: %s", msg)
	}
	if msg := startError(errors.New("access denied"), "claude", nil, env, "").Error(); msg != "starting claude: access denied" {
		t.Fatalf("another error: %s", msg)
	}
	goos = "darwin"
	if err := checkEnv(env); err != nil {
		t.Fatal(err)
	}
	if msg := startError(winErrTooLong, "claude", nil, env, "").Error(); strings.Contains(msg, "largest") {
		t.Fatalf("not Windows: %s", msg)
	}
}
