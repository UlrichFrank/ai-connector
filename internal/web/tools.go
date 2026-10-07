package web

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"
)

// The home tools and the ones allowed always (docs/chat-tools.md, "Always allow" for tools).

// toolSep joins an MCP server's name and its tool's (internal/mcp.Sep).
const toolSep = "__"

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	l, err := s.Jobs.ApprovalStore().List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string][]string{"always": l})
}

// allowAlways puts a home tool on the list. Only tools the home zone has right now; while a server does not
// answer, a name of that server (<server>__<tool>) is taken on trust.
func (s *Server) allowAlways(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("tool")
	if err := s.homeTool(r.Context(), name); err != "" {
		http.Error(w, err, http.StatusBadRequest)
		return
	}
	if err := s.Jobs.ApprovalStore().Add(name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.listApprovals(w, r)
}

// homeTool says why name cannot be allowed always ("" if it can).
func (s *Server) homeTool(ctx context.Context, name string) string {
	if name == "" || len(name) > 200 || strings.ContainsAny(name, " /\n\t") {
		return "not a tool name: " + name
	}
	if s.HomeTools == nil {
		return "this Mac has no home tools"
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	servers, err := s.HomeTools(ctx)
	if err != nil {
		return "the home tools cannot be listed: " + err.Error()
	}
	for _, sv := range servers {
		for _, t := range sv.Tools {
			if t.Name == name {
				return ""
			}
		}
	}
	for _, sv := range servers {
		if rest, ok := strings.CutPrefix(name, sv.Name+toolSep); ok && rest != "" && sv.Error != "" {
			return "" // the server does not answer now: its tools cannot be checked
		}
	}
	return "no home tool " + name + " (only tools of the MCP servers can be allowed always)"
}

// toolView is a home tool as the settings page sees it.
type toolView struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Changes     bool   `json:"changes"` // asks before each call, unless Always
	Always      bool   `json:"always"`
}

type serverView struct {
	Name  string     `json:"name"`
	Error string     `json:"error,omitempty"`
	Tools []toolView `json:"tools"`
}

// tools: GET /api/tools — the home zone's tools by server, and the list of tools allowed always.
func (s *Server) tools(w http.ResponseWriter, r *http.Request) {
	always, err := s.Jobs.ApprovalStore().List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := map[string]any{"always": always, "servers": []serverView{}}
	if s.HomeTools == nil {
		writeJSON(w, out)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	servers, err := s.HomeTools(ctx)
	if err != nil {
		out["error"] = err.Error()
	}
	views := []serverView{}
	for _, sv := range servers {
		v := serverView{Name: sv.Name, Error: sv.Error, Tools: []toolView{}}
		for _, t := range sv.Tools {
			v.Tools = append(v.Tools, toolView{Name: t.Name, Description: t.Description, Changes: t.Changes, Always: slices.Contains(always, t.Name)})
		}
		views = append(views, v)
	}
	out["servers"] = views
	writeJSON(w, out)
}
