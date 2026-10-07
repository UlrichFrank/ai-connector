package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/agent"
	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/llm"
)

func toolsServer(t *testing.T) (*httptest.Server, *jobs.Manager) {
	cat := &engine.Catalog{Models: []*engine.Model{{ID: "chat", Name: "Chat", Kind: engine.Chat, Backend: "echo"}}}
	m := jobs.New(cat, []engine.Backend{echo{}}, t.TempDir())
	t.Cleanup(func() {
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !m.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	})
	home := func(context.Context) ([]ToolServer, error) {
		return []ToolServer{
			{Name: "smarthome", Tools: []agent.Tool{
				{Name: "smarthome__set_light", Description: "Switch a light", Changes: true},
				{Name: "smarthome__get_status", Description: "Read the house"},
			}},
			{Name: "vboard", Error: "connection refused"},
		}, nil
	}
	srv := httptest.NewServer((&Server{Jobs: m, HomeTools: home}).Handler())
	t.Cleanup(srv.Close)
	return srv, m
}

func do(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestApprovalsList(t *testing.T) {
	srv, m := toolsServer(t)
	if c, b := do(t, "GET", srv.URL+"/api/approvals", ""); c != 200 || strings.TrimSpace(b) != `{"always":[]}` {
		t.Fatal(c, b)
	}
	if c, b := do(t, "PUT", srv.URL+"/api/approvals/smarthome__set_light", ""); c != 200 || strings.TrimSpace(b) != `{"always":["smarthome__set_light"]}` {
		t.Fatal(c, b)
	}
	// a server that does not answer now: its names are taken on trust
	if c, b := do(t, "PUT", srv.URL+"/api/approvals/vboard__create_card", ""); c != 200 {
		t.Fatal(c, b)
	}
	// unknown names, web and files tools are refused
	for _, n := range []string{"smarthome__nope", "web_fetch", "make_file", "other__x", "vboard__"} {
		if c, b := do(t, "PUT", srv.URL+"/api/approvals/"+n, ""); c != 400 {
			t.Fatal(n, c, b)
		}
	}
	if !m.ApprovalStore().Has("vboard__create_card") || m.ApprovalStore().Has("web_fetch") {
		t.Fatal("store")
	}
	if c, b := do(t, "DELETE", srv.URL+"/api/approvals/vboard__create_card", ""); c != 200 || strings.TrimSpace(b) != `{"always":["smarthome__set_light"]}` {
		t.Fatal(c, b)
	}

	c, b := do(t, "GET", srv.URL+"/api/tools", "")
	var tools struct {
		Always  []string
		Servers []struct {
			Name, Error string
			Tools       []struct {
				Name, Description string
				Changes, Always   bool
			}
		}
	}
	if c != 200 || json.Unmarshal([]byte(b), &tools) != nil {
		t.Fatal(c, b)
	}
	if len(tools.Servers) != 2 || tools.Servers[1].Error == "" || len(tools.Servers[1].Tools) != 0 || len(tools.Servers[0].Tools) != 2 ||
		!tools.Servers[0].Tools[0].Always || !tools.Servers[0].Tools[0].Changes || tools.Servers[0].Tools[1].Changes || tools.Servers[0].Tools[1].Always ||
		strings.Join(tools.Always, ",") != "smarthome__set_light" {
		t.Fatal(b)
	}
}

func TestApprovalsWithoutHomeTools(t *testing.T) {
	cat := &engine.Catalog{Models: []*engine.Model{{ID: "chat", Kind: engine.Chat, Backend: "echo"}}}
	m := jobs.New(cat, []engine.Backend{echo{}}, t.TempDir())
	srv := httptest.NewServer((&Server{Jobs: m}).Handler())
	defer srv.Close()
	if c, _ := do(t, "PUT", srv.URL+"/api/approvals/smarthome__set_light", ""); c != 400 {
		t.Fatal(c)
	}
	if c, b := do(t, "GET", srv.URL+"/api/tools", ""); c != 200 || !strings.Contains(b, `"servers":[]`) {
		t.Fatal(c, b)
	}
}

func TestApproveAlwaysAndTranscript(t *testing.T) {
	srv, m := toolsServer(t)
	j, err := m.Submit(engine.Request{Model: "chat", Messages: []llm.Message{{Role: "user", Content: "q"}}})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if d, _ := m.Job(j.ID); !d.Active() {
			break
		}
	}
	if c, _ := do(t, "POST", srv.URL+"/api/jobs/"+j.ID+"/approve", `{"call":"c","allow":false,"always":true}`); c != 400 {
		t.Fatal(c)
	}
	if c, _ := do(t, "POST", srv.URL+"/api/jobs/"+j.ID+"/approve", `{"call":"c","allow":true,"always":true}`); c != 409 {
		t.Fatal(c) // nothing waits
	}
	c, b := do(t, "GET", srv.URL+"/api/jobs/"+j.ID+"/transcript", "")
	if c != 200 || !strings.HasPrefix(b, "mlxlab chat · Chat · zone: none · ") || !strings.Contains(b, "Question:\nq\n") {
		t.Fatal(c, b)
	}
	if c, _ := do(t, "GET", srv.URL+"/api/jobs/nope/transcript", ""); c != 404 {
		t.Fatal(c)
	}
}
