package tunnel

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A worker connects out; the hub's requests reach the worker's handler, streamed responses (SSE) included.
func TestRoundTrip(t *testing.T) {
	hub := &Hub{Token: "secret"}
	srv := httptest.NewServer(hub)
	defer srv.Close()

	worker := http.NewServeMux()
	worker.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hi "+r.URL.Query().Get("n")) })
	worker.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 3; i++ {
			io.WriteString(w, "data: x\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := make(chan bool, 4)
	go Serve(ctx, srv.URL, "secret", "test-mac", worker, func(c bool, _ error) { up <- c })
	if !<-up {
		t.Fatal("not connected")
	}
	waitConnected(t, hub, Legacy)
	if c, ok := hub.Lookup(Legacy); !ok || c.Name != "test-mac" {
		t.Fatalf("worker %+v %v", c, ok)
	}
	client := &http.Client{Transport: hub.Transport()}
	for i := 0; i < 20; i++ { // many requests, concurrently
		go client.Get("http://" + Host(Legacy) + "/hello?n=x")
	}
	r, err := client.Get("http://" + Host(Legacy) + "/hello?n=7")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	if string(b) != "hi 7" {
		t.Fatalf("got %q", b)
	}
	r, err = client.Get("http://" + Host(Legacy) + "/events")
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(r.Body)
	n := 0
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("%d events", n)
	}
	// the worker leaves: the hub says offline
	cancel()
	time.Sleep(200 * time.Millisecond)
	if len(hub.Connected()) != 0 {
		t.Fatal("still connected")
	}
	if _, err := client.Get("http://" + Host(Legacy) + "/hello"); !errors.Is(err, ErrOffline) {
		t.Fatalf("want offline, got %v", err)
	}
}

func TestWrongToken(t *testing.T) {
	srv := httptest.NewServer(&Hub{Token: "secret"})
	defer srv.Close()
	err := Serve(context.Background(), srv.URL, "nope", "m", http.NotFoundHandler(), nil)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

// waitConnected: the worker hears "connected" once the WebSocket is up, a moment before the hub takes the session.
func waitConnected(t *testing.T, hub *Hub, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := hub.Lookup(id); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the hub does not see the worker")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An enrolled worker is let in through Check (with its enrolled name and id); Disconnect closes its tunnel.
func TestCheckAndDisconnect(t *testing.T) {
	var revoked atomic.Bool
	hub := &Hub{Check: func(token string) (string, string, bool) {
		if token == "enrolled" && !revoked.Load() {
			return "w1", "Mac Studio", true
		}
		return "", "", false
	}}
	srv := httptest.NewServer(hub)
	defer srv.Close()

	// no legacy token set: an empty or unknown token is refused
	for _, tok := range []string{"", "nope"} {
		if err := Serve(context.Background(), srv.URL, tok, "m", http.NotFoundHandler(), nil); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("token %q: %v", tok, err)
		}
	}

	up := make(chan bool, 8)
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), srv.URL, "enrolled", "host-name", http.NotFoundHandler(), func(c bool, _ error) { up <- c })
	}()
	if !<-up {
		t.Fatal("not connected")
	}
	waitConnected(t, hub, "w1")
	if c := hub.Connected(); len(c) != 1 || c[0].ID != "w1" || c[0].Name != "Mac Studio" {
		t.Fatalf("connected %+v", c)
	}
	if hub.Disconnect("other") || hub.Disconnect("") {
		t.Fatal("disconnected a worker that is not connected")
	}
	changed := hub.Changed()
	revoked.Store(true)
	if !hub.Disconnect("w1") {
		t.Fatal("Disconnect found no worker")
	}
	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("no change after Disconnect")
	}
	if len(hub.Connected()) != 0 {
		t.Fatal("still connected")
	}
	select { // it retries, is refused, and gives up
	case err := <-done:
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the revoked worker kept trying")
	}
}

// Check and the legacy Token side by side: the legacy worker is Legacy and keeps the name it sends.
func TestLegacyBesideCheck(t *testing.T) {
	hub := &Hub{Token: "shared", Check: func(string) (string, string, bool) { return "", "", false }}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := make(chan bool, 4)
	go Serve(ctx, srv.URL, "shared", "old-mac", http.NotFoundHandler(), func(c bool, _ error) { up <- c })
	if !<-up {
		t.Fatal("not connected")
	}
	waitConnected(t, hub, Legacy)
	if c := hub.Connected(); len(c) != 1 || c[0].ID != Legacy || c[0].Name != "old-mac" {
		t.Fatalf("connected %+v", c)
	}
}

// Several workers at once: each is reached by its own Host; one leaving leaves the others; the same worker
// connecting again replaces only its own older connection.
func TestSeveralWorkers(t *testing.T) {
	hub := &Hub{Token: "shared", Check: func(token string) (string, string, bool) {
		if id, ok := strings.CutPrefix(token, "tok-"); ok {
			return id, "Mac " + id, true
		}
		return "", "", false
	}}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	serve := func(token, answer string) (stop func()) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, answer) })
		go func() { Serve(ctx, srv.URL, token, "host", h, nil); close(done) }()
		return func() { cancel(); <-done }
	}
	stopA := serve("tok-a", "A")
	defer stopA()
	stopB := serve("tok-b", "B")
	defer stopB()
	stopL := serve("shared", "L")
	defer stopL()
	for _, id := range []string{"a", "b", Legacy} {
		waitConnected(t, hub, id)
	}
	client := &http.Client{Transport: hub.Transport()}
	get := func(id string) (string, error) {
		r, err := client.Get("http://" + Host(id) + "/")
		if err != nil {
			return "", err
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return string(b), nil
	}
	for id, want := range map[string]string{"a": "A", "b": "B", Legacy: "L"} {
		if got, err := get(id); err != nil || got != want {
			t.Fatalf("%s: %q %v", id, got, err)
		}
	}
	if n := len(hub.Connected()); n != 3 {
		t.Fatalf("%d connected", n)
	}
	// a second connection of b replaces b's first, not a's
	stopB2 := serve("tok-b", "B2")
	defer stopB2()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := get("b")
		if got == "B2" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the newer b did not take over")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got, err := get("a"); err != nil || got != "A" {
		t.Fatalf("a after b reconnected: %q %v", got, err)
	}
	// a goes: b stays
	if !hub.Disconnect("a") {
		t.Fatal("no a")
	}
	stopA()
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, ok := hub.Lookup("a"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a still there")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := get("a"); !errors.Is(err, ErrOffline) {
		t.Fatalf("a: %v", err)
	}
	if got, err := get("b"); err != nil || got != "B2" {
		t.Fatalf("b after a left: %q %v", got, err)
	}
	if _, err := client.Get("http://worker/"); err == nil {
		t.Fatal("a host that names no worker")
	}
}
