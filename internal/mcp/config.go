// Package mcp is the client of the MCP servers a home chat may use (vboard, the smart home): it reads
// ~/.config/mlxlab/mcp.json, talks JSON-RPC 2.0 to each server over streamable HTTP, the older HTTP+SSE transport or
// stdio, and offers each server's tools as an agent.Toolbox in the home zone (docs/chat-tools.md).
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/philipparndt/ai-connector/internal/engine"
)

// Config is mcp.json: the MCP servers by name.
type Config struct {
	Servers map[string]*Server `json:"servers"`
}

// Server is one MCP server: either url (+ headers) or command/args/env.
type Server struct {
	Name string `json:"-"`

	// Type forces the transport: "http" (streamable HTTP, the default for url; falls back to "sse"), "sse" (the
	// older HTTP+SSE transport) or "stdio" (the default for command).
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"` // values may use ${ENV}

	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"` // values may use ${ENV}
	Dir     string            `json:"dir,omitempty"` // working directory of the command

	// ReadOnly lists tools that change nothing (glob patterns like "get_*"), in addition to those the server marks
	// readOnlyHint. All other tools need the user's approval for every call.
	ReadOnly []string `json:"readOnly,omitempty"`
	Disabled bool     `json:"disabled,omitempty"`
	// Timeout of one call in seconds (default 60).
	Timeout float64 `json:"timeout,omitempty"`
}

// Sep joins the server name and the tool name: vboard__search_cards.
const Sep = "__"

// ConfigPath is where mcp.json lives (honours $MLXLAB_HOME).
func ConfigPath() string { return filepath.Join(engine.ConfigDir(), "mcp.json") }

var nameRE = regexp.MustCompile(`^[A-Za-z0-9-]+(_[A-Za-z0-9-]+)*$`)

// LoadConfig reads mcp.json at p. A missing file is no servers. The file holds tokens, so it is refused when
// others than its owner may read it.
func LoadConfig(p string) (*Config, error) {
	info, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others (mode %04o) and may hold tokens: run chmod 600 %s", p, perm, p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	c, err := ParseConfig(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return c, nil
}

// ParseConfig parses and checks the content of mcp.json.
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	for name, s := range c.Servers {
		if s == nil {
			return nil, fmt.Errorf("server %q: empty", name)
		}
		s.Name = name
		if err := s.check(); err != nil {
			return nil, fmt.Errorf("server %q: %w", name, err)
		}
	}
	return &c, nil
}

func (s *Server) check() error {
	if !nameRE.MatchString(s.Name) || strings.Contains(s.Name, Sep) {
		return errors.New("the name may use letters, digits, - and single _ (it prefixes the tool names)")
	}
	switch s.Type {
	case "":
		switch {
		case s.URL != "" && s.Command != "":
			return errors.New("either url or command, not both")
		case s.URL == "" && s.Command == "":
			return errors.New("needs url or command")
		}
	case "http", "sse":
		if s.URL == "" {
			return fmt.Errorf("type %s needs url", s.Type)
		}
	case "stdio":
		if s.Command == "" {
			return errors.New("type stdio needs command")
		}
	default:
		return fmt.Errorf("unknown type %q (http, sse or stdio)", s.Type)
	}
	if s.URL != "" && !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
		return fmt.Errorf("url %q: http:// or https://", s.URL)
	}
	for _, p := range s.ReadOnly {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("readOnly pattern %q: %w", p, err)
		}
	}
	return nil
}

// transport is the transport to use: "http", "sse" or "stdio".
func (s *Server) transport() string {
	switch {
	case s.Type != "":
		return s.Type
	case s.Command != "":
		return "stdio"
	}
	return "http"
}

// Enabled lists the servers that are not disabled, sorted by name.
func (c *Config) Enabled() []*Server {
	var out []*Server
	for _, s := range c.Servers {
		if !s.Disabled {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// IsReadOnly says whether the config lists the tool (its name on the server) as read-only.
func (s *Server) IsReadOnly(tool string) bool {
	for _, p := range s.ReadOnly {
		if ok, _ := path.Match(p, tool); ok {
			return true
		}
	}
	return false
}

var envRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expand replaces ${NAME} with the environment variable NAME; an unset one is an error (a missing token would
// otherwise turn into a confusing 401). Other $ signs are kept.
func expand(v string, getenv func(string) (string, bool)) (string, error) {
	var missing []string
	out := envRE.ReplaceAllStringFunc(v, func(m string) string {
		name := envRE.FindStringSubmatch(m)[1]
		val, ok := getenv(name)
		if !ok {
			missing = append(missing, name)
		}
		return val
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("$%s is not set", strings.Join(missing, ", $"))
	}
	return out, nil
}

func expandAll(kind string, m map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(m))
	for k, v := range m {
		x, err := expand(v, os.LookupEnv)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", kind, k, err)
		}
		out[k] = x
	}
	return out, nil
}
