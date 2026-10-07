// Package agent runs a chat with tools: the model may call tools, sees their results and answers. What a chat may
// touch depends on its zone, and the zone is enforced here, in code — never by asking the model:
//
//	home   the internal MCP servers (smart home, vboard). No web content ever enters such a chat.
//	web    web search and page fetching, run in a sandbox VM that can reach only the internet (not the LAN, the Pi
//	       or the Mac). Home tools are not offered and are refused if called anyway.
//	files  code that writes files (PDF, documents, charts) runs in a sandbox VM without any network.
//
// A chat's zone is fixed when it starts (Request.Zone) and kept by every follow-up turn: a conversation that has
// seen web content can never become a home conversation (the worker checks the history, not the client's word).
//
// This file is the contract between the parts (written before them): the agent loop (loop.go), the MCP client
// (internal/mcp), the sandboxes (internal/sandbox) and the guard (internal/guard).
package agent

import (
	"context"
	"encoding/json"
	"time"
)

// Zone is what a chat may touch.
type Zone string

const (
	ZoneNone  Zone = ""      // a plain chat: no tools
	ZoneHome  Zone = "home"  // internal MCP servers
	ZoneWeb   Zone = "web"   // web search and fetch (sandboxed); may also write files (no network there)
	ZoneFiles Zone = "files" // write files in the offline sandbox only
)

// Tool is one function the model may call.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON schema
	// Zone the tool belongs to; the loop offers a tool only in its zone (files tools also in web chats).
	Zone Zone `json:"zone"`
	// Changes says the tool changes something in the world (switches a light, writes a calendar entry): the user
	// must approve every call first. Read-only tools run directly. MCP tools are "changes" unless their server
	// marks them readOnlyHint (or the config lists them as read-only).
	Changes bool `json:"changes"`
	// Untrusted says the result comes from outside (the web): it is screened by the guard before the model sees it.
	Untrusted bool `json:"untrusted"`
}

// Result is what a tool call gave back.
type Result struct {
	Text  string   `json:"text"`            // for the model (capped by the loop)
	Files []string `json:"files,omitempty"` // files made (absolute paths in the worker's output dir)
	// Sources: for web results, what was read (shown to the user as citations).
	Sources []Source `json:"sources,omitempty"`
	IsError bool     `json:"isError,omitempty"`
}

type Source struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Toolbox is a set of tools of one origin (an MCP server, the web sandbox, the files sandbox).
type Toolbox interface {
	Tools(ctx context.Context) ([]Tool, error)
	Call(ctx context.Context, name string, args json.RawMessage) (Result, error)
	Close() error
}

// Verdict is what the guard says about a text.
type Verdict struct {
	Label string  `json:"label"` // screen: "ok" | "injection" | "irrelevant"; grade: "good" | "bad" | "unsure"
	Score float64 `json:"score"` // the classifier's confidence in Label, 0..1
	Why   string  `json:"why,omitempty"`
}

// Guard classifies (on the Mac, with the Laya classifier): untrusted content before the model reads it, and the
// final answer. It is a second line of defence; the zones are the first.
type Guard interface {
	// Screen checks untrusted text (a search result, a page) for instructions aimed at the AI.
	Screen(ctx context.Context, question, content string) (Verdict, error)
	// Grade rates an answer: does it answer the question, does it stay with the sources.
	Grade(ctx context.Context, question, answer string, sources []string) (Verdict, error)
}

// ---------- what the job reports (engine events, see docs/chat-tools.md) ----------

// ToolEvent is the Data of an engine event of type "tool": one per change of a call's state.
type ToolEvent struct {
	ID        string          `json:"id"` // unique within the job
	Name      string          `json:"name"`
	Zone      Zone            `json:"zone"`
	Arguments json.RawMessage `json:"arguments"`
	// State: "approval" (waiting for the user) | "running" | "done" | "error" | "denied" (by the user or timed
	// out) | "blocked" (refused: wrong zone, unknown tool) | "held" (the guard held the result back).
	State   string   `json:"state"`
	Result  string   `json:"result,omitempty"` // shortened for display
	Sources []Source `json:"sources,omitempty"`
	Files   []string `json:"files,omitempty"` // /files/ urls once done
	Verdict *Verdict `json:"verdict,omitempty"`
	// While waiting for approval: when it counts as denied, and what the tool does (its description).
	Deadline    time.Time `json:"deadline,omitzero"`
	Description string    `json:"description,omitempty"`
	// Auto: a call that changes something ran without asking, because the user allowed the tool always
	// (Config.Always; docs/chat-tools.md, "Always allow" for tools).
	Auto bool `json:"auto,omitempty"`
}
