package agents

import (
	"os"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/agents/fakeclaude"
)

// TestMain doubles as a fake claude (internal/agents/fakeclaude): the tests run this test binary as claude.
func TestMain(m *testing.M) {
	if fakeclaude.Active() {
		os.Exit(fakeclaude.Main(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeManager is a manager whose claude is this test binary.
func fakeManager(t *testing.T) (*Manager, string) {
	t.Helper()
	log := t.TempDir() + "/claude.log"
	m := &Manager{Root: t.TempDir(), Command: os.Args[0], Env: []string{fakeclaude.Env, "AGENTS_FAKE_LOG=" + log},
		Version: "9.9.9 (fake)"}
	t.Cleanup(m.Close)
	return m, log
}

// starts are the fake's start lines.
func starts(t *testing.T, log string) []string {
	t.Helper()
	b, _ := os.ReadFile(log)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
