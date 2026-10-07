package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/philipparndt/ai-connector/internal/agent"
	"github.com/philipparndt/ai-connector/internal/engine"
)

// TranscriptOptions: what a transcript includes besides the questions, the tool calls and the answers.
type TranscriptOptions struct {
	Thinking bool // each answer's thinking
	Full     bool // tool results unshortened (as the model read them)
}

// ErrNotChat: a transcript is for chats only.
var ErrNotChat = errors.New("not a chat")

// Transcript is a chat as plain text, for copying (docs/chat-tools.md, "Copying a chat for analysis"): from the
// first turn of the chat job id belongs to, through id, on to the newest turn of that branch.
func (m *Manager) Transcript(id string, o TranscriptOptions) (string, error) {
	m.mu.Lock()
	j := m.find(id)
	if j == nil {
		m.mu.Unlock()
		return "", errors.New("no such job")
	}
	if j.Kind != engine.Chat {
		m.mu.Unlock()
		return "", ErrNotChat
	}
	turns := Branch(m.jobs, id)
	for i, t := range turns {
		turns[i] = t.clone()
	}
	m.mu.Unlock()
	return WriteTranscript(turns, o), nil
}

// chatTurns is how far into the conversation a job is: its messages without the system prompt.
func chatTurns(j *Job) int {
	n := 0
	for _, m := range j.Request.Messages {
		if m.Role != "system" {
			n++
		}
	}
	return n
}

// Branch is the turns of the chat that job id is on, oldest first: back along `parent` to the first turn, then
// forward along the newest continuation of each turn. A parent with as many messages is a sibling (a regenerated
// answer, another model's answer in a compare chat: the same place in the conversation), not a turn before.
func Branch(all []*Job, id string) []*Job {
	byID := map[string]*Job{}
	for _, j := range all {
		if j.Kind == engine.Chat {
			byID[j.ID] = j
		}
	}
	// prev: the turn a job continues (siblings resolved)
	prev := func(j *Job) *Job {
		n := chatTurns(j)
		seen := map[string]bool{j.ID: true}
		for p := byID[j.Request.Parent]; p != nil && !seen[p.ID]; p = byID[p.Request.Parent] {
			seen[p.ID] = true
			switch pt := chatTurns(p); {
			case pt < n:
				return p
			case pt > n:
				return nil
			}
		}
		return nil
	}
	start := byID[id]
	if start == nil {
		return nil
	}
	parent := map[string]*Job{}
	for _, j := range byID {
		if p := prev(j); p != nil {
			parent[j.ID] = p
		}
	}
	out := []*Job{start}
	seen := map[string]bool{start.ID: true}
	for p := parent[start.ID]; p != nil && !seen[p.ID]; p = parent[p.ID] {
		seen[p.ID] = true
		out = append([]*Job{p}, out...)
	}
	for cur := start; ; {
		var next *Job
		for _, j := range all { // in history order: the later of two at the same time wins
			if parent[j.ID] == cur && !seen[j.ID] && (next == nil || !j.Created.Before(next.Created)) {
				next = j
			}
		}
		if next == nil {
			break
		}
		seen[next.ID] = true
		out = append(out, next)
		cur = next
	}
	return out
}

// shortResult is how much of a tool result the transcript shows unless Full.
const shortResult = 600

// WriteTranscript writes the turns of one branch of a chat (Branch) as text.
func WriteTranscript(turns []*Job, o TranscriptOptions) string {
	var b strings.Builder
	if len(turns) == 0 {
		return ""
	}
	first := turns[0]
	zone := first.Request.Zone
	if zone == "" {
		zone = "none"
	}
	fmt.Fprintf(&b, "mlxlab chat · %s · zone: %s · %s\n", nameOf(first), zone, first.Created.UTC().Format(time.RFC3339))
	system := "(default)"
	for _, m := range first.Request.Messages {
		if m.Role == "system" {
			system = strings.TrimSpace(m.Content)
			break
		}
	}
	fmt.Fprintf(&b, "System: %s\n", system)
	for _, j := range turns {
		b.WriteString("\n———\nQuestion:\n")
		b.WriteString(strings.TrimSpace(question(j)))
		b.WriteString("\n")
		if lines := toolLines(j, o.Full); len(lines) > 0 {
			b.WriteString("\nTools:\n")
			for _, l := range lines {
				b.WriteString(l)
				b.WriteString("\n")
			}
		}
		thoughts, answer := splitThinking(j.Text)
		if o.Thinking && len(thoughts) == 0 {
			thoughts = historyThoughts(j)
		}
		if o.Thinking && len(thoughts) > 0 {
			b.WriteString("\nThinking:\n")
			b.WriteString(strings.Join(thoughts, "\n\n"))
			b.WriteString("\n")
		}
		if answer != "" || j.State == Done {
			if j.Name != first.Name && j.Name != "" {
				fmt.Fprintf(&b, "\nAnswer (%s):\n", j.Name)
			} else {
				b.WriteString("\nAnswer:\n")
			}
			if answer == "" {
				answer = "(empty)"
			}
			b.WriteString(answer)
			b.WriteString("\n")
		}
		if s := statsLine(j); s != "" {
			b.WriteString("\n(" + s + ")\n")
		}
	}
	return b.String()
}

func nameOf(j *Job) string {
	if j.Name != "" {
		return j.Name
	}
	return j.Model
}

// question is the turn's last user message.
func question(j *Job) string {
	ms := j.Request.Messages
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].Role == "user" {
			return ms[i].Content
		}
	}
	return ""
}

// splitThinking separates the thinking (<think>…</think>, one block per round of a chat with tools) from the
// answer. An unclosed block (the job stopped while it thought) is thinking to the end.
func splitThinking(text string) (thoughts []string, answer string) {
	var ans []string // the words between the thoughts (one piece per round)
	say := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			ans = append(ans, s)
		}
	}
	rest := text
	// the thought was opened by the prompt: a closing tag without an opening one ends a thought at the start
	if c, o := strings.Index(rest, "</think>"), strings.Index(rest, "<think>"); c >= 0 && (o < 0 || c < o) {
		if t := strings.TrimSpace(rest[:c]); t != "" {
			thoughts = append(thoughts, t)
		}
		rest = rest[c+len("</think>"):]
	}
	for {
		o := strings.Index(rest, "<think>")
		if o < 0 {
			say(rest)
			break
		}
		say(rest[:o])
		rest = rest[o+len("<think>"):]
		c := strings.Index(rest, "</think>")
		if c < 0 {
			if t := strings.TrimSpace(rest); t != "" {
				thoughts = append(thoughts, t)
			}
			break
		}
		if t := strings.TrimSpace(rest[:c]); t != "" {
			thoughts = append(thoughts, t)
		}
		rest = rest[c+len("</think>"):]
	}
	return thoughts, strings.Join(ans, "\n\n")
}

// historyThoughts: the thinking of every round of the turn as its history keeps it (result.messages: each assistant
// message after the question starts with its round's "<think>"), for a job whose text has none (one kept without
// its streamed text).
func historyThoughts(j *Job) []string {
	msgs := ResultMessages(j)
	from := len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			from = i + 1
			break
		}
	}
	var out []string
	for _, m := range msgs[min(from, len(msgs)):] {
		if m.Role == "assistant" {
			t, _ := splitThinking(m.Content)
			out = append(out, t...)
		}
	}
	return out
}

// toolLines: one line per call, in the order they were made.
//
//	✓ name {args} — result
//	⏸ name {args} — approval: allowed always — result     (or "approval: approved", "approval: waiting")
//	✗ name {args} — held: <guard reason> | denied: … | blocked: … | error: …
//	… name {args} — running
func toolLines(j *Job, full bool) []string {
	var results map[string]string
	if full {
		results = map[string]string{}
		for _, m := range ResultMessages(j) {
			if m.Role == "tool" && m.ToolCallID != "" {
				results[m.ToolCallID] = m.Content
			}
		}
	}
	var out []string
	for _, t := range j.Tools {
		var ev agent.ToolEvent
		b, _ := json.Marshal(t)
		if json.Unmarshal(b, &ev) != nil {
			continue
		}
		asked, _ := t["asked"].(bool)
		result := ev.Result
		if r, ok := results[ev.ID]; ok && ev.State != "held" {
			result = r
		}
		if !full {
			result = shorten(result, shortResult)
		}
		var mark string
		var parts []string
		switch {
		case ev.Auto:
			parts = append(parts, "approval: allowed always")
		case ev.State == "approval":
			parts = append(parts, "approval: waiting")
		case asked && ev.State != "denied":
			parts = append(parts, "approval: approved")
		}
		switch ev.State {
		case "done":
			mark = "✓"
			if len(parts) > 0 {
				mark = "⏸"
			}
			parts = append(parts, result)
		case "approval":
			mark = "⏸"
		case "running":
			mark = "…"
			parts = append(parts, "running")
		case "held":
			mark = "✗"
			why := "it contained instructions aimed at the assistant"
			if ev.Verdict != nil && ev.Verdict.Why != "" {
				why = ev.Verdict.Why
			}
			parts = append(parts, "held: "+why)
		default: // denied, blocked, error
			mark = "✗"
			parts = append(parts, ev.State+": "+result)
		}
		line := mark + " " + ev.Name + " " + compactJSON(ev.Arguments)
		var rest []string
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				rest = append(rest, p)
			}
		}
		if len(rest) > 0 {
			line += " — " + strings.Join(rest, " — ")
		}
		out = append(out, indent(line))
	}
	return out
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// indent keeps a multi-line result under its call.
func indent(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// statsLine: the guard's verdict, speed, tokens, time — or how the turn ended when it did not answer.
func statsLine(j *Job) string {
	var parts []string
	switch j.State {
	case Failed:
		e := j.Error
		if e == "" {
			e = j.Status
		}
		parts = append(parts, "failed: "+e)
	case Canceled:
		parts = append(parts, "canceled")
	case Queued:
		parts = append(parts, "queued")
	case Running:
		parts = append(parts, "still running")
	}
	if v := verdictOf(j.Stats["verdict"]); v != nil && v.Label != "" {
		parts = append(parts, fmt.Sprintf("verdict: %s %.2f", v.Label, v.Score))
	}
	if tps := number(j.Stats["tokensPerSecond"]); tps > 0 {
		parts = append(parts, fmt.Sprintf("%.1f tok/s", tps))
	}
	if n := number(j.Stats["tokens"]); n > 0 {
		parts = append(parts, strconv.Itoa(int(n))+" tokens")
	}
	if !j.Started.IsZero() && !j.Finished.IsZero() {
		parts = append(parts, Duration(j.Seconds()))
	}
	return strings.Join(parts, " · ")
}

func verdictOf(v any) *agent.Verdict {
	switch x := v.(type) {
	case nil:
		return nil
	case agent.Verdict:
		return &x
	case *agent.Verdict:
		return x
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out agent.Verdict
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return &out
}

func number(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return 0
}
