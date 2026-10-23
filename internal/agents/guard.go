package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// What keeps a session from reading what it must not (docs/api.md, "Agents (Claude Code)", Security). A session
// reads content other people wrote (a shared board, a document): a prompt injection there may ask it to read the
// worker's secrets — the MCP token of its own session, the worker token, Claude Code's credentials, ssh keys — and
// write them somewhere it can reach. So:
//
//   - MCP secrets (header values) never go into a file: mcp.json names environment variables (${AIC_MCP_…}) that
//     Claude Code expands when it connects (checked with Claude Code 2.1.288), and only the process has them.
//   - Claude Code runs --restricted where it can (its file tools confined to the working folder, no command tools
//     unless the allow-list names them, the user's own settings files ignored), with --tools naming only the
//     built-in tools of the allow-list.
//   - The default allow-list reads only the session's folder (Read(./**) …).
//   - Deny rules (permissions.deny of the session's --settings file) deny reading and editing the control tree (<agents>/.ctl), the rest of the config folder
//     (worker.json with the worker token, agents.json, other sessions), ~/.claude*, ~/.ssh and /proc; of the
//     agents' own Claude Code config folder (<agents>/.claude-home, home.go) everything but the session's own
//     project folder, where Claude Code saves the large tool results the model is to read.
//   - A person's own tty session works only in a folder inside the agents folder or one agents.json lists.

// mcpEnvPrefix starts the environment variables that carry MCP header values.
const mcpEnvPrefix = "AIC_MCP_"

// mcpConfig is the --mcp-config of the session's MCP servers: header values are ${VAR} references, the values are
// in env (for the process only). allowed are the servers' tool rules (mcp__<name>).
func mcpConfig(servers []MCPServer) (cfg map[string]any, env, allowed []string) {
	mcp := map[string]any{}
	for i, srv := range servers {
		name := mcpName(srv.Name)
		if name == "" || srv.URL == "" {
			continue
		}
		typ := srv.Type
		if typ == "" {
			typ = "http"
		}
		entry := map[string]any{"type": typ, "url": srv.URL}
		if len(srv.Headers) > 0 {
			keys := make([]string, 0, len(srv.Headers))
			for k := range srv.Headers {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			h := map[string]string{}
			for j, k := range keys {
				v := fmt.Sprintf("%s%d_%d", mcpEnvPrefix, i, j)
				h[k] = "${" + v + "}"
				env = append(env, v+"="+srv.Headers[k])
			}
			entry["headers"] = h
		}
		mcp[name] = entry
		allowed = append(allowed, "mcp__"+name)
	}
	return map[string]any{"mcpServers": mcp}, env, allowed
}

// ToolSearch is Claude Code's tool that loads deferred tool definitions when they are needed. MCP tools are deferred
// only while it is available, so with --tools it must be named (measured with Claude Code 2.1.288, docs/api.md
// "Agents (Claude Code)", Tool search).
const ToolSearch = "ToolSearch"

// sessionTools is --tools of a json or tty session: the built-in tools of the allow-list, and ToolSearch when the
// session has MCP servers (their definitions are then loaded on demand instead of sent with every request).
func sessionTools(allowed []string, mcp bool) []string {
	names := builtinTools(allowed)
	if mcp && len(names) > 0 && !slices.Contains(names, ToolSearch) {
		names = append(names, ToolSearch)
	}
	return names
}

// builtinTools are the built-in tools an allow-list names (Read for "Read(./**)"; not the MCP tools), for --tools.
func builtinTools(allowed []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range allowed {
		name, _, _ := strings.Cut(strings.TrimSpace(a), "(")
		if name == "" || strings.HasPrefix(name, "mcp__") || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// absRule is a permission rule's path for an absolute path ("//" starts one from the file system's root).
func absRule(p string) string {
	return "//" + strings.TrimPrefix(filepath.ToSlash(p), "/")
}

// denyRules are the deny rules of a session (its --settings file): no reading or editing of the control tree, the rest of the
// config folder, other sessions' folders, Claude Code's own files (the user's ~/.claude* entirely; of the agents'
// config folder all but the session's own project folder, where its saved tool results are), ssh keys and /proc
// (the process environment holds the MCP secrets).
func (m *Manager) denyRules(s *Session) []string {
	var paths []string // absolute; each denied itself and everything below it
	paths = append(paths, filepath.Join(m.Root, ".ctl"), filepath.Join(m.Root, "resume.json"))
	inside := func(p, dir string) bool {
		rel, err := filepath.Rel(dir, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	// other sessions (those there now; a session's file tools are confined to its folder anyway)
	if es, err := os.ReadDir(m.Root); err == nil {
		for _, e := range es {
			p := filepath.Join(m.Root, e.Name())
			if e.Name() == ".ctl" || e.Name() == "resume.json" || p == m.ClaudeHome() || inside(s.cwd, p) {
				continue
			}
			paths = append(paths, p)
		}
	}
	// the agents' Claude Code config folder: all but the session's own project folder (home.go)
	homePaths, homePrefixes := m.homeDenyPaths(s)
	paths = append(paths, homePaths...)
	// the user's Claude Code config folder when it is not ~/.claude (denied below)
	if user, _ := m.lookupEnv("CLAUDE_CONFIG_DIR"); filepath.IsAbs(user) && !inside(s.cwd, user) {
		paths = append(paths, user)
	}
	// the config folder around the agents folder: worker.json (the worker token), agents.json, client.json, …;
	// and the shared config folder when this worker's state is elsewhere (each entry: the folders of the session
	// are left out, their parts are above)
	for _, dir := range append([]string{m.Home}, m.Shared...) {
		if dir == "" {
			continue
		}
		known := []string{"worker.json", "client.json", "agents.json", "history.json", "prompts.json", "approvals.json"}
		seen := map[string]bool{}
		if es, err := os.ReadDir(dir); err == nil {
			for _, e := range es {
				known = append(known, e.Name())
			}
		}
		for _, n := range known {
			p := filepath.Join(dir, n)
			if seen[n] || inside(m.Root, p) || inside(p, m.Root) || inside(s.cwd, p) {
				continue // the agents folder itself (its parts are above)
			}
			seen[n] = true
			paths = append(paths, p)
		}
	}
	var rules []string
	add := func(r string) {
		// Claude Code applies Read rules to Glob and Grep as best effort only: they are denied by name too
		rules = append(rules, "Read("+r+")", "Edit("+r+")", "Glob("+r+")", "Grep("+r+")")
	}
	for _, p := range paths {
		add(absRule(p))
		add(absRule(p) + "/**")
	}
	for _, p := range homePrefixes {
		add(absRule(p) + "*")
	}
	home, _ := os.UserHomeDir()
	if mlx := filepath.Join(home, ".config", "mlxlab"); home != "" && !inside(s.cwd, mlx) && !inside(m.Root, mlx) {
		add("~/.config/mlxlab")
		add("~/.config/mlxlab/**")
	}
	for _, r := range []string{"~/.claude*", "~/.claude*/**", "~/.ssh", "~/.ssh/**", "//proc/**"} {
		add(r)
	}
	return rules
}

// CheckCwd says whether a person's tty session may work in cwd: an absolute folder inside the agents folder (not
// its control tree or Claude Code's config folder, not the agents folder itself) or inside one of agents.json's
// folders. It answers the folder
// with its links resolved.
func (m *Manager) CheckCwd(cwd string) (string, error) {
	bad := fmt.Errorf("cwd %q is not a folder on this worker", cwd)
	if !filepath.IsAbs(cwd) {
		return "", bad
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(cwd))
	if err != nil {
		return "", bad
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return "", bad
	}
	under := func(dir string, self bool) bool {
		d, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return false
		}
		rel, err := filepath.Rel(d, real)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false
		}
		return self || rel != "."
	}
	if under(m.Root, false) {
		rel, _ := filepath.Rel(mustReal(m.Root), real)
		if first := strings.Split(rel, string(filepath.Separator))[0]; first != ".ctl" && first != claudeHomeName {
			return real, nil
		}
	}
	for _, f := range m.Config.Folders {
		if filepath.IsAbs(f) && under(f, true) {
			return real, nil
		}
	}
	return "", fmt.Errorf("cwd %q is outside the agents folder %s and the folders of agents.json", cwd, m.Root)
}

func mustReal(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}
