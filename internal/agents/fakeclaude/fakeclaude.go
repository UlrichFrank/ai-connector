// Package fakeclaude is a fake Claude Code for tests: a test binary runs it (Main) when it finds Env in its
// environment, so tests of the agents, the worker API and the hub need no real claude.
//
// With -p it speaks stream-json: init, partial text deltas, a tool call (a prompt with "tool"), a slow answer it
// stops on an interrupt control request ("slow"), an early exit ("die"), and the result with usage and the
// session's total cost — many turns on one process. Without -p it is an interactive "terminal" that runs the hooks
// of its --settings file like Claude Code (SessionStart, UserPromptSubmit, Pre/PostToolUse, Stop) for each line
// typed. Every start is logged to $AGENTS_FAKE_LOG, and its arguments, MCP config and environment as JSON to
// $AGENTS_FAKE_ARGS.
package fakeclaude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Env is what the environment of claude needs for the fake (plus AGENTS_FAKE_LOG=<file> to log its starts).
const Env = "AGENTS_FAKE=1"

// Active: this process is to be the fake (call Main).
func Active() bool { return os.Getenv("AGENTS_FAKE") == "1" }

// Main runs the fake with claude's arguments and returns the exit code.
func Main(args []string) int { return fakeClaude(args) }

func fakeClaude(args []string) int {
	opt := map[string]string{}
	print := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--version":
			fmt.Println("9.9.9 (Claude Code)")
			return 0
		case a == "-p":
			print = true
		case strings.HasPrefix(a, "--") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--"):
			opt[a] = args[i+1]
			i++
		}
	}
	if log := os.Getenv("AGENTS_FAKE_LOG"); log != "" {
		f, _ := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, "start model=%s resume=%s tools=%s\n", opt["--model"], opt["--resume"], opt["--allowedTools"])
		f.Close()
	}
	if log := os.Getenv("AGENTS_FAKE_ARGS"); log != "" {
		// how it was started, as JSON per start: the arguments, the MCP config as written and as Claude Code
		// expands it (${VAR} from the environment), the AIC_MCP_* environment
		raw, _ := os.ReadFile(opt["--mcp-config"])
		env := map[string]string{}
		for _, kv := range os.Environ() {
			if k, v, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "AIC_MCP_") {
				env[k] = v
			}
		}
		b, _ := json.Marshal(map[string]any{"args": args, "mcpRaw": string(raw), "mcpExpanded": os.ExpandEnv(string(raw)), "env": env})
		f, _ := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		f.Write(append(b, '\n'))
		f.Close()
	}
	sid := opt["--resume"]
	if sid == "" {
		sid = fmt.Sprintf("fake-%d", os.Getpid())
	}
	if !print {
		return fakeTTY(opt, sid)
	}
	out := json.NewEncoder(os.Stdout)
	lines := make(chan map[string]any, 100)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var v map[string]any
			if json.Unmarshal(sc.Bytes(), &v) == nil {
				lines <- v
			}
		}
		close(lines)
	}()
	cost, turns := 0.0, 0
	send := func(v map[string]any) {
		v["session_id"] = sid
		out.Encode(v)
	}
	delta := func(s string) {
		send(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": s}}})
	}
	message := func(id string, deltas ...string) {
		send(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_start", "message": map[string]any{"id": id}}})
		send(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}}})
		for _, d := range deltas {
			delta(d)
		}
		send(map[string]any{"type": "assistant", "message": map[string]any{"id": id, "role": "assistant",
			"content": []any{map[string]any{"type": "text", "text": strings.Join(deltas, "")}}}})
	}
	for v := range lines {
		if v["type"] != "user" {
			continue
		}
		turns++
		text, _ := v["message"].(map[string]any)["content"].(string)
		send(map[string]any{"type": "system", "subtype": "init", "cwd": mustWd(), "model": opt["--model"]})
		send(map[string]any{"type": "system", "subtype": "status", "status": "requesting"})
		answer := ""
		switch {
		case strings.Contains(text, "die"):
			return 3
		case strings.Contains(text, "slow"):
			send(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_start", "message": map[string]any{"id": "slow"}}})
			interrupted := false
			for i := 0; i < 1000 && !interrupted; i++ {
				delta("tick ")
				select {
				case c := <-lines:
					if c["type"] == "control_request" {
						send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": c["request_id"]}})
						interrupted = true
					}
				case <-time.After(10 * time.Millisecond):
				}
			}
			if interrupted {
				send(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
				send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "total_cost_usd": cost, "num_turns": 1})
				continue
			}
			answer = "ticked"
		case strings.Contains(text, "sticky"): // works on a board through its vboard MCP server
			send(map[string]any{"type": "assistant", "message": map[string]any{"id": "m1", "role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_v1", "name": "mcp__vboard__create_items",
					"input": map[string]any{"items": []any{map[string]any{"type": "sticky", "data": map[string]any{"text": "hello", "color": "yellow"}}}}}}}})
			send(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_v1", "content": `{"batchId":"bt_1"}`}}}})
			message("m2", "Added a yellow ", "sticky.")
			answer = "Added a yellow sticky."
		case strings.Contains(text, "tool"):
			message("m1", "Let me ", "look.")
			send(map[string]any{"type": "assistant", "message": map[string]any{"id": "m1", "role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"file_path": "/x/notes.txt"}}}}})
			send(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "the notes"}}}})
			message("m2", "Found ", "it.")
			answer = "Let me look.\n\nFound it."
		default:
			answer = "echo: " + text
			message(fmt.Sprintf("m%d", turns), "echo: ", text)
		}
		cost += 0.01
		send(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": answer, "num_turns": 1,
			"total_cost_usd": cost, "duration_ms": 5,
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 7}})
	}
	return 0
}

func mustWd() string {
	wd, _ := os.Getwd()
	return wd
}

var bracketed = regexp.MustCompile("\x1b\\[20[01]~")

// fakeTTY: prints a banner, runs the SessionStart hook, then takes every line typed as a prompt: the
// UserPromptSubmit hook, an answer on the terminal, the Stop hook with last_assistant_message.
func fakeTTY(opt map[string]string, sid string) int {
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	if b, err := os.ReadFile(opt["--settings"]); err == nil {
		json.Unmarshal(b, &settings)
	}
	hook := func(ev string, v map[string]any) {
		v["hook_event_name"], v["session_id"] = ev, sid
		b, _ := json.Marshal(v)
		for _, h := range settings.Hooks[ev] {
			for _, c := range h.Hooks {
				cmd := exec.Command("sh", "-c", c.Command)
				cmd.Stdin = bytes.NewReader(b)
				cmd.Run()
			}
		}
	}
	fmt.Print("fake claude ready\r\n")
	hook("SessionStart", map[string]any{"source": "startup"})
	r := bufio.NewReader(os.Stdin)
	for {
		line, err := r.ReadString('\n') // the terminal turns Enter (\r) into \n
		if err != nil {
			return 0
		}
		prompt := strings.TrimSpace(bracketed.ReplaceAllString(line, ""))
		if prompt == "" {
			continue
		}
		hook("UserPromptSubmit", map[string]any{"prompt": prompt})
		if strings.Contains(prompt, "tool") {
			hook("PreToolUse", map[string]any{"tool_name": "Read", "tool_use_id": "t1", "tool_input": map[string]any{"file_path": "/x/y"}})
			hook("PostToolUse", map[string]any{"tool_name": "Read", "tool_use_id": "t1", "tool_response": "contents"})
		}
		fmt.Printf("answer: %s\r\n", prompt)
		hook("Stop", map[string]any{"last_assistant_message": "echo: " + prompt})
	}
}
