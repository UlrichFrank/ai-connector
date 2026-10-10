package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
)

// Only file urls get the route: the fields that hold them and strings that are a /files/ url as a whole; free text
// (a chat's answer, its messages, a text event, logs) stays as written, even when it mentions /files/ paths.
func TestRouteText(t *testing.T) {
	rt := &route{rid: "m1"}
	in := `{"id":"j1","state":"done","kind":"chat","request":{"images":["/files/uploads/a.png"],"audio":"/files/b.wav",` +
		`"source":"/files/song one.wav","prompt":"draw /files/x","messages":[{"Role":"user","Content":"/files/x.png"}]},` +
		`"files":["/files/out.png"],"preview":"/files/p.mp4","cover":"/files/c.png","text":"see [x](/files/tools/r.pdf) and /files/b",` +
		`"logs":["/files/l"],"result":{"file":"/files/r.wav","messages":[{"Content":"/files/m"}],"other":"/files/o","note":"at /files/n"},` +
		`"tools":[{"files":["/files/t.pdf"],"result":"saved /files/t.pdf","arguments":{"path":"/files/arg"}}],` +
		`"event":{"type":"text","text":"/files/e"},"fileEvent":{"type":"file","file":"/files/f.png"},"path":"/Users/me/files/c"}`
	b, ok := mapJSON([]byte(in), rt.out)
	if !ok {
		t.Fatal("not JSON")
	}
	var got map[string]any
	json.Unmarshal(b, &got)
	req := got["request"].(map[string]any)
	res := got["result"].(map[string]any)
	tool := got["tools"].([]any)[0].(map[string]any)
	for what, pair := range map[string][2]any{
		"images":       {req["images"].([]any)[0], "/files/m1/uploads/a.png"},
		"audio":        {req["audio"], "/files/m1/b.wav"},
		"source":       {req["source"], "/files/m1/song one.wav"},
		"prompt":       {req["prompt"], "draw /files/x"},
		"message":      {req["messages"].([]any)[0].(map[string]any)["Content"], "/files/x.png"},
		"files":        {got["files"].([]any)[0], "/files/m1/out.png"},
		"preview":      {got["preview"], "/files/m1/p.mp4"},
		"cover":        {got["cover"], "/files/m1/c.png"},
		"text":         {got["text"], "see [x](/files/tools/r.pdf) and /files/b"},
		"logs":         {got["logs"].([]any)[0], "/files/l"},
		"result file":  {res["file"], "/files/m1/r.wav"},
		"result msg":   {res["messages"].([]any)[0].(map[string]any)["Content"], "/files/m"},
		"result url":   {res["other"], "/files/m1/o"},
		"result note":  {res["note"], "at /files/n"},
		"tool files":   {tool["files"].([]any)[0], "/files/m1/t.pdf"},
		"tool result":  {tool["result"], "saved /files/t.pdf"},
		"tool args":    {tool["arguments"].(map[string]any)["path"], "/files/arg"},
		"text event":   {got["event"].(map[string]any)["text"], "/files/e"},
		"file event":   {got["fileEvent"].(map[string]any)["file"], "/files/m1/f.png"},
		"another path": {got["path"], "/Users/me/files/c"},
		"job id":       {got["id"], "m1.j1"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: %v, want %v", what, pair[0], pair[1])
		}
	}
	// and back: what comes in with the route loses it, text stays
	back, _ := mapJSON([]byte(`{"images":["/files/m1/a.png"],"audio":"/files/m1/b.wav","parent":"m1.j1",`+
		`"messages":[{"Role":"assistant","Content":"see /files/m1/x"}]}`), rt.in)
	var bm map[string]any
	json.Unmarshal(back, &bm)
	if bm["images"].([]any)[0] != "/files/a.png" || bm["audio"] != "/files/b.wav" || bm["parent"] != "j1" ||
		bm["messages"].([]any)[0].(map[string]any)["Content"] != "see /files/m1/x" {
		t.Errorf("in %s", back)
	}
	if rt.inPath("/api/jobs/m1.abc/cancel") != "/api/jobs/abc/cancel" || rt.inPath("/files/m1/x/y.png") != "/files/x/y.png" ||
		rt.inPath("/api/jobs/abc") != "/api/jobs/abc" || rt.inPath("/files/m2/x") != "/files/m2/x" {
		t.Error("inPath")
	}
	for _, r := range []string{"", "M1", "m.1", "m/1", strings.Repeat("a", 17)} {
		if ValidRoute(r) {
			t.Errorf("route %q valid", r)
		}
	}
}

// With the hub's route header every job id, parent and file url carries the prefix, in answers and events; what
// comes in with the prefix is understood; without the header nothing changes.
func TestRouted(t *testing.T) {
	cat := &engine.Catalog{Models: []*engine.Model{{ID: "chat", Kind: engine.Chat, Backend: "echo"}}}
	m := jobs.New(cat, []engine.Backend{echo{}}, t.TempDir())
	t.Cleanup(func() {
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !m.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	})
	srv := httptest.NewServer((&Server{Jobs: m, Version: "v1"}).Handler())
	defer srv.Close()
	do := func(method, path, ctype string, body io.Reader, routed bool) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, body)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if routed {
			req.Header.Set(RouteHeader, "m1")
			req.Header.Set(WorkerIDHeader, "w1")
			req.Header.Set(WorkerNameHeader, "Mac%20Studio")
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	decode := func(r *http.Response, v any) {
		t.Helper()
		defer r.Body.Close()
		if r.StatusCode != 200 {
			b, _ := io.ReadAll(r.Body)
			t.Fatalf("%s: %d %s", r.Request.URL.Path, r.StatusCode, b)
		}
		if err := json.NewDecoder(r.Body).Decode(v); err != nil {
			t.Fatal(err)
		}
	}

	// events, from before the first job
	ev := do("GET", "/api/events", "", nil, true)
	defer ev.Body.Close()
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(ev.Body)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				lines <- d
			}
		}
	}()

	// an upload: its url carries the prefix, and it comes back as a reference
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "ref.png")
	fw.Write([]byte("png"))
	mw.Close()
	var up struct{ URL string }
	decode(do("POST", "/api/upload", mw.FormDataContentType(), &body, true), &up)
	if !strings.HasPrefix(up.URL, "/files/m1/uploads/") {
		t.Fatalf("upload url %q", up.URL)
	}
	if r := do("GET", up.URL, "", nil, true); r.StatusCode != 200 {
		t.Fatalf("file through the route: %d", r.StatusCode)
	}

	var j map[string]any
	decode(do("POST", "/api/jobs", "application/json", strings.NewReader(`{"model":"chat","images":["`+up.URL+`"],"messages":[{"Role":"user","Content":"q"}]}`), true), &j)
	id, _ := j["id"].(string)
	if !strings.HasPrefix(id, "m1.") {
		t.Fatalf("job id %q", id)
	}
	if w, _ := j["worker"].(map[string]any); w["id"] != "w1" || w["name"] != "Mac Studio" {
		t.Fatalf("worker %v", j["worker"])
	}
	if imgs := j["request"].(map[string]any)["images"].([]any); imgs[0] != up.URL {
		t.Fatalf("images %v", imgs)
	}
	raw := strings.TrimPrefix(id, "m1.")
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if d, _ := m.Job(raw); !d.Active() {
			break
		}
	}
	// a follow-up names its parent with the prefix
	var f map[string]any
	decode(do("POST", "/api/jobs", "application/json", strings.NewReader(`{"model":"chat","parent":"`+id+`","messages":[{"Role":"user","Content":"q"}]}`), true), &f)
	if f["request"].(map[string]any)["parent"] != id {
		t.Fatalf("parent %v", f["request"])
	}
	if fj, _ := m.Job(strings.TrimPrefix(f["id"].(string), "m1.")); fj == nil || fj.Request.Parent != raw {
		t.Fatalf("stored parent %+v", fj)
	}
	// addressed by the prefixed id
	var g map[string]any
	decode(do("GET", "/api/jobs/"+id, "", nil, true), &g)
	if g["id"] != id {
		t.Fatalf("get %v", g["id"])
	}
	var list []map[string]any
	decode(do("GET", "/api/jobs", "", nil, true), &list)
	for _, x := range list {
		if !strings.HasPrefix(x["id"].(string), "m1.") {
			t.Fatalf("listed %v", x["id"])
		}
	}
	// without the header: as before
	list = nil
	decode(do("GET", "/api/jobs", "", nil, false), &list)
	if list[0]["id"] != raw || list[0]["worker"] != nil || strings.HasPrefix(list[0]["request"].(map[string]any)["images"].([]any)[0].(string), "/files/m1/") {
		t.Fatalf("unrouted %v", list[0])
	}
	// the events
	select {
	case d := <-lines:
		var u map[string]any
		json.Unmarshal([]byte(d), &u)
		if !strings.HasPrefix(u["jobId"].(string), "m1.") {
			t.Fatalf("event %s", d)
		}
		if job, ok := u["job"].(map[string]any); ok && !strings.HasPrefix(job["id"].(string), "m1.") {
			t.Fatalf("event job %s", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	// info: routes, specs, state
	var info struct {
		Routes bool
		Specs  Specs
		State  WorkerState
	}
	t.Setenv("MLXLAB_FAKE_RAM_GB", "16")
	decode(do("GET", "/api/info", "", nil, true), &info)
	if !info.Routes || info.Specs.RAMGB != 16 || info.Specs.Version != "v1" || info.State.Loaded == nil {
		t.Fatalf("info %+v", info)
	}
}
