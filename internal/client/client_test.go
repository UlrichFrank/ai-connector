package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
)

func TestLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/login" {
			http.NotFound(w, r)
			return
		}
		var body struct{ Password, Device string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Password != "secret" || body.Device != "box TUI" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"password"}`)
			return
		}
		fmt.Fprint(w, `{"token":"tok123","device":{"id":"d1","name":"box TUI"}}`)
	}))
	defer srv.Close()
	tok, err := Login(context.Background(), srv.URL, "secret", "box TUI", "Tester")
	if err != nil || tok != "tok123" {
		t.Fatalf("%q %v", tok, err)
	}
	if _, err := Login(context.Background(), srv.URL, "nope", "box TUI", "Tester"); !errors.Is(err, ErrPassword) {
		t.Fatalf("wrong password: %v", err)
	}
}

func TestErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") != "Bearer good":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"signin"}`)
		case r.URL.Path == "/api/models":
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":"offline","message":"The Mac is offline"}`)
		default:
			http.Error(w, "no such job", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if _, err := New(srv.URL, "bad").Jobs(ctx); !errors.Is(err, ErrSignIn) {
		t.Fatalf("401: %v", err)
	}
	if _, err := New(srv.URL, "bad").Events(ctx); !errors.Is(err, ErrSignIn) {
		t.Fatalf("events 401: %v", err)
	}
	c := New(srv.URL, "good")
	if _, err := c.Models(ctx); !errors.Is(err, ErrOffline) {
		t.Fatalf("503: %v", err)
	}
	var se *StatusError
	if err := c.Cancel(ctx, "x"); !errors.As(err, &se) || se.Code != 404 || se.Message != "no such job" {
		t.Fatalf("404: %v", err)
	}
}

func TestSubmit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/jobs" || r.Header.Get("Authorization") != "Bearer t" {
			http.NotFound(w, r)
			return
		}
		var req engine.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		_ = json.NewEncoder(w).Encode(jobs.Job{ID: "j1", Model: req.Model, Request: req, State: jobs.Queued})
	}))
	defer srv.Close()
	j, err := New(srv.URL+"/", "t").Submit(context.Background(), engine.Request{Model: "flux", Prompt: "a fox", Width: 512})
	if err != nil {
		t.Fatal(err)
	}
	if j.ID != "j1" || j.Model != "flux" || j.Request.Prompt != "a fox" || j.Request.Width != 512 || j.State != jobs.Queued {
		t.Fatalf("%+v", j)
	}
}

func TestFileURL(t *testing.T) {
	if u := New("https://h/", "a b").FileURL("/files/x.png"); u != "https://h/files/x.png?token=a+b" {
		t.Fatal(u)
	}
	if u := New("http://127.0.0.1:8484", "").FileURL("/files/x.png"); u != "http://127.0.0.1:8484/files/x.png" {
		t.Fatal(u)
	}
}

// The stream: the first connection sends two updates and drops; the client reconnects, lists the jobs again (one
// was deleted meanwhile, one is new) and goes on with the second connection.
func TestEventsReconnect(t *testing.T) {
	var conns, lists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			if lists.Add(1) == 1 {
				fmt.Fprint(w, `[{"id":"a","state":"running"}]`)
			} else {
				fmt.Fprint(w, `[{"id":"b","state":"queued"}]`)
			}
		case "/api/events":
			w.Header().Set("Content-Type", "text/event-stream")
			if conns.Add(1) == 1 {
				fmt.Fprint(w, ": hello\n\n")
				fmt.Fprint(w, "data: {\"jobId\":\"a\",\"event\":{\"type\":\"text\",\"text\":\"Hel\"}}\n\n")
				// one message over two data lines, CRLF endings
				fmt.Fprint(w, "data: {\"jobId\":\"a\",\r\ndata: \"event\":{\"type\":\"progress\",\"step\":2,\"total\":4}}\r\n\r\n")
				return // drops
			}
			fmt.Fprint(w, "data: {\"jobId\":\"b\",\"event\":{\"type\":\"status\",\"text\":\"loading\"}}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	c.retry = 10 * time.Millisecond
	states := make(chan error, 16)
	c.OnStream = func(_ string, err error) { states <- err }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := c.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for len(got) < 6 {
		select {
		case u, ok := <-ch:
			if !ok {
				t.Fatal("closed")
			}
			s := u.JobID + ":"
			switch {
			case u.Job != nil:
				s += "job " + string(u.Job.State)
			case u.Event != nil:
				s += fmt.Sprintf("%s %s %d/%d", u.Event.Type, u.Event.Text, u.Event.Step, u.Event.Total)
			default:
				s += "deleted"
			}
			got = append(got, s)
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout, got %q", got)
		}
	}
	want := []string{"a:job running", "a:text Hel 0/0", "a:progress  2/4", "a:deleted", "b:job queued", "b:status loading 0/0"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	// connected, dropped, connected again
	if a, b, c := <-states, <-states, <-states; a != nil || b == nil || c != nil {
		t.Fatalf("states %v %v %v", a, b, c)
	}
	cancel()
	for range ch {
	}
}

func TestApply(t *testing.T) {
	j := &jobs.Job{ID: "a"}
	Apply(j, engine.Event{Type: "text", Text: "Hel"})
	Apply(j, engine.Event{Type: "text", Text: "lo"})
	Apply(j, engine.Event{Type: "progress", Step: 3, Total: 8})
	Apply(j, engine.Event{Type: "file", File: "/files/a.png"})
	Apply(j, engine.Event{Type: "stats", Data: map[string]any{"seed": 1.0}})
	if j.Text != "Hello" || j.Step != 3 || j.Total != 8 || len(j.Files) != 1 || j.Stats["seed"] != 1.0 {
		t.Fatalf("%+v", j)
	}
}

func TestConfig(t *testing.T) {
	t.Setenv("MLXLAB_HOME", t.TempDir())
	c, err := LoadConfig()
	if err != nil || c != (Config{}) {
		t.Fatalf("%+v %v", c, err)
	}
	if err := SaveConfig(Config{Server: "https://h", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if c, err = LoadConfig(); err != nil || c.Server != "https://h" || c.Token != "t" {
		t.Fatalf("%+v %v", c, err)
	}
	if st, _ := os.Stat(ConfigPath()); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
}
