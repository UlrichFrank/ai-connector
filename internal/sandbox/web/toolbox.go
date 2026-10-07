package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/philipparndt/ai-connector/internal/agent"
)

var _ agent.Toolbox = (*Sandbox)(nil)

const (
	ToolSearch = "web_search"
	ToolFetch  = "web_fetch"
)

var tools = []agent.Tool{
	{
		Name: ToolSearch,
		Description: "Search the web. Returns a numbered list of results (title, URL, snippet). Use web_fetch to " +
			"read a result.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"query":{"type":"string","description":"what to search for"},` +
			`"count":{"type":"integer","minimum":1,"maximum":20,"description":"number of results (default 8)"}},` +
			`"required":["query"]}`),
		Zone: agent.ZoneWeb, Untrusted: true,
	},
	{
		Name:        ToolFetch,
		Description: "Fetch a web page (http or https) and return its title and readable text (size-capped).",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"url":{"type":"string","description":"the http(s) URL to read"}},"required":["url"]}`),
		Zone: agent.ZoneWeb, Untrusted: true,
	},
}

// Tools are web_search and web_fetch (zone web, untrusted, read-only).
func (s *Sandbox) Tools(context.Context) ([]agent.Tool, error) {
	return append([]agent.Tool(nil), tools...), nil
}

type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

type fetchResult struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Text        string `json:"text"`
	ContentType string `json:"contentType"`
	Truncated   bool   `json:"truncated"`
}

// Call runs a tool in the sandbox (starting it if needed). A failure of the target (a refused or unreachable
// URL, no results) is a Result with IsError; an error means the sandbox itself failed.
func (s *Sandbox) Call(ctx context.Context, name string, args json.RawMessage) (agent.Result, error) {
	switch name {
	case ToolSearch:
		var in struct {
			Query string `json:"query"`
			Count int    `json:"count"`
		}
		if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.Query) == "" {
			return agent.Result{Text: "web_search needs {\"query\": \"...\"}", IsError: true}, nil
		}
		if err := s.Ensure(ctx); err != nil {
			return agent.Result{}, err
		}
		var out struct {
			Results []searchResult `json:"results"`
		}
		if msg, err := s.post(ctx, "/search", in, &out, 60*time.Second); err != nil || msg != "" {
			return errResult("search failed", msg, err)
		}
		return searchToResult(in.Query, out.Results), nil
	case ToolFetch:
		var in struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return agent.Result{Text: "web_fetch needs {\"url\": \"https://...\"}", IsError: true}, nil
		}
		u, err := url.Parse(strings.TrimSpace(in.URL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return agent.Result{Text: "web_fetch: only http and https URLs can be read", IsError: true}, nil
		}
		if err := s.Ensure(ctx); err != nil {
			return agent.Result{}, err
		}
		var out fetchResult
		if msg, err := s.post(ctx, "/fetch", in, &out, 90*time.Second); err != nil || msg != "" {
			return errResult("could not read "+in.URL, msg, err)
		}
		return fetchToResult(out), nil
	}
	return agent.Result{Text: "unknown tool " + name, IsError: true}, nil
}

func errResult(what, msg string, err error) (agent.Result, error) {
	if err != nil {
		return agent.Result{}, fmt.Errorf("web sandbox: %s: %w", what, err)
	}
	return agent.Result{Text: what + ": " + msg, IsError: true}, nil
}

// post sends a request to the tools service. msg is the service's error (a target failure); err a transport one.
func (s *Sandbox) post(ctx context.Context, path string, in, out any, timeout time.Duration) (msg string, err error) {
	s.touch()
	defer s.touch()
	body, _ := json.Marshal(in)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL()+path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return e.Error, nil
		}
		return resp.Status, nil
	}
	return "", json.Unmarshal(b, out)
}

func (s *Sandbox) touch() {
	s.mu.Lock()
	s.lastUse = time.Now()
	s.mu.Unlock()
}

func searchToResult(q string, rs []searchResult) agent.Result {
	if len(rs) == 0 {
		return agent.Result{Text: fmt.Sprintf("No results for %q.", q)}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Search results for %q:\n", q)
	res := agent.Result{}
	for i, r := range rs {
		fmt.Fprintf(&b, "\n%d. %s\n   %s\n", i+1, oneLine(r.Title), r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", oneLine(r.Snippet))
		}
		res.Sources = append(res.Sources, agent.Source{Title: oneLine(r.Title), URL: r.URL})
	}
	res.Text = b.String()
	return res
}

func fetchToResult(f fetchResult) agent.Result {
	var b strings.Builder
	title := oneLine(f.Title)
	if title == "" {
		title = f.URL
	}
	fmt.Fprintf(&b, "# %s\nURL: %s\n\n%s\n", title, f.URL, f.Text)
	if f.Truncated {
		b.WriteString("\n[the page was longer; the text was cut]\n")
	}
	return agent.Result{Text: b.String(), Sources: []agent.Source{{Title: title, URL: f.URL}}}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
