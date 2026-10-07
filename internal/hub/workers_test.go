package hub

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/tunnel"
)

// enroll makes a worker through the API and returns its id and token.
func enroll(t *testing.T, base, device, name string) (id, token string) {
	t.Helper()
	r := do(t, "POST", base+"/api/workers", device, strings.NewReader(`{"name":"`+name+`"}`))
	if r.StatusCode != 200 {
		t.Fatalf("enroll: %d", r.StatusCode)
	}
	var out struct {
		Worker Worker `json:"worker"`
		Token  string `json:"token"`
	}
	decode(t, r, &out)
	if out.Worker.ID == "" || out.Worker.Name != name || out.Worker.Created.IsZero() || len(out.Token) != 43 {
		t.Fatalf("enrolled %+v", out)
	}
	return out.Worker.ID, out.Token
}

type listedWorker struct {
	Worker
	Connected bool `json:"connected"`
}

func listWorkers(t *testing.T, base, device string) []listedWorker {
	t.Helper()
	r := do(t, "GET", base+"/api/workers", device, nil)
	if r.StatusCode != 200 {
		t.Fatalf("list workers: %d", r.StatusCode)
	}
	var list []listedWorker
	decode(t, r, &list)
	return list
}

func info(t *testing.T, base string) workerState {
	t.Helper()
	var out struct {
		Worker workerState `json:"worker"`
	}
	decode(t, do(t, "GET", base+"/api/info", "", nil), &out)
	return out.Worker
}

// waitFor polls cond for a few seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// putTool places a fake Mac program where the hub serves it from.
func putTool(t *testing.T, dir string) []byte {
	t.Helper()
	bin := make([]byte, 300_000)
	rand.Read(bin)
	if err := os.MkdirAll(filepath.Join(dir, "mlxlab"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mlxlab", "Darwin-arm64"), bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func download(t *testing.T, base, platform, token string) (*http.Response, []byte) {
	t.Helper()
	r := do(t, "GET", base+"/tools/mlxlab/"+platform, token, nil)
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r, b
}

// Enroll a Mac in the web UI, connect it with its own token, download the program with it, revoke it.
func TestEnrolledWorker(t *testing.T) {
	tools := t.TempDir()
	bin := putTool(t, tools)
	_, srv := newHubWith(t, Config{Password: password, WorkerToken: workerToken, Data: t.TempDir(), Tools: tools})
	device := signIn(t, srv.URL, "laptop")

	// device sign-in needed; a worker token is not a device token
	for _, req := range []struct{ method, path string }{{"GET", "/api/workers"}, {"POST", "/api/workers"}, {"DELETE", "/api/workers/x"}} {
		if r := do(t, req.method, srv.URL+req.path, "", nil); r.StatusCode != 401 {
			t.Fatalf("%s %s without sign-in: %d", req.method, req.path, r.StatusCode)
		}
	}
	if l := listWorkers(t, srv.URL, device); len(l) != 0 {
		t.Fatalf("workers at start %v", l)
	}
	id, token := enroll(t, srv.URL, device, "Mac Studio")
	if r := do(t, "GET", srv.URL+"/api/workers", token, nil); r.StatusCode != 401 {
		t.Fatalf("worker token as a device: %d", r.StatusCode)
	}
	l := listWorkers(t, srv.URL, device)
	if len(l) != 1 || l[0].ID != id || l[0].Name != "Mac Studio" || l[0].Connected {
		t.Fatalf("workers %+v", l)
	}

	// the program, with the worker's token
	r, b := download(t, srv.URL, "Darwin-arm64", token)
	if r.StatusCode != 200 || !bytes.Equal(b, bin) {
		t.Fatalf("download: %d, %d bytes", r.StatusCode, len(b))
	}
	if r.Header.Get("Content-Type") != "application/octet-stream" || r.Header.Get("Content-Length") != strconv.Itoa(len(bin)) {
		t.Fatalf("download headers %v", r.Header)
	}

	// the tunnel, with the worker's token: the hub knows which worker it is
	worker := http.NewServeMux()
	worker.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "[]") })
	_, ended := connectWorkerWith(t, srv.URL, token, worker)
	st := info(t, srv.URL)
	if !st.Connected || st.ID != id || st.Name != "Mac Studio" {
		t.Fatalf("info worker %+v", st)
	}
	l = listWorkers(t, srv.URL, device)
	if len(l) != 1 || !l[0].Connected || l[0].LastSeen.IsZero() {
		t.Fatalf("workers while connected %+v", l)
	}
	if r := do(t, "GET", srv.URL+"/api/models", device, nil); r.StatusCode != 200 {
		t.Fatalf("proxy through the enrolled worker: %d", r.StatusCode)
	}

	// revoked: the tunnel closes, the worker cannot come back, the download is refused
	if r := do(t, "DELETE", srv.URL+"/api/workers/nope", device, nil); r.StatusCode != 404 {
		t.Fatalf("revoke unknown: %d", r.StatusCode)
	}
	if r := do(t, "DELETE", srv.URL+"/api/workers/"+id, device, nil); r.StatusCode != 200 {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	waitFor(t, "the revoked worker's tunnel to close", func() bool { return !info(t, srv.URL).Connected })
	select {
	case err := <-ended:
		if !errors.Is(err, tunnel.ErrUnauthorized) {
			t.Fatalf("the revoked worker ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the revoked worker kept trying")
	}
	if r, _ := download(t, srv.URL, "Darwin-arm64", token); r.StatusCode != 401 {
		t.Fatalf("download with a revoked token: %d", r.StatusCode)
	}
	if l := listWorkers(t, srv.URL, device); len(l) != 0 {
		t.Fatalf("workers after revoke %v", l)
	}
}

// The shared MLXLAB_WORKER_TOKEN keeps working (the first Mac); without it, only enrolled workers get in.
func TestLegacyWorkerToken(t *testing.T) {
	tools := t.TempDir()
	putTool(t, tools)
	_, srv := newHubWith(t, Config{Password: password, WorkerToken: workerToken, Data: t.TempDir(), Tools: tools})
	connectWorker(t, srv.URL, http.NewServeMux())
	if st := info(t, srv.URL); !st.Connected || st.ID != tunnel.Legacy || st.Name != "test-mac" {
		t.Fatalf("legacy worker %+v", st)
	}
	if r, _ := download(t, srv.URL, "Darwin-arm64", workerToken); r.StatusCode != 200 {
		t.Fatalf("download with the legacy token: %d", r.StatusCode)
	}

	// a hub without the shared token: it refuses it (and an empty one), enrolled workers still connect
	_, srv2 := newHubWith(t, Config{Password: password, Data: t.TempDir(), Tools: tools})
	for _, tok := range []string{workerToken, ""} {
		if err := tunnel.Serve(t.Context(), srv2.URL, tok, "m", http.NotFoundHandler(), nil); !errors.Is(err, tunnel.ErrUnauthorized) {
			t.Fatalf("token %q on a hub without one: %v", tok, err)
		}
		if r, _ := download(t, srv2.URL, "Darwin-arm64", tok); r.StatusCode != 401 {
			t.Fatalf("download with %q on a hub without one: %d", tok, r.StatusCode)
		}
	}
	device := signIn(t, srv2.URL, "laptop")
	id, token := enroll(t, srv2.URL, device, "MacBook")
	connectWorkerWith(t, srv2.URL, token, http.NewServeMux())
	if st := info(t, srv2.URL); !st.Connected || st.ID != id {
		t.Fatalf("enrolled worker on a hub without the shared token %+v", st)
	}
}

// Two Macs stay connected side by side (no more "a newer one replaces the older"); revoking one leaves the other.
func TestBothWorkersStay(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	device := signIn(t, srv.URL, "laptop")
	idA, tokA := enroll(t, srv.URL, device, "A")
	idB, tokB := enroll(t, srv.URL, device, "B")
	connectWorkerWith(t, srv.URL, tokA, http.NewServeMux())
	connectWorkerWith(t, srv.URL, tokB, http.NewServeMux())
	connected := func() map[string]bool {
		out := map[string]bool{}
		for _, w := range listWorkers(t, srv.URL, device) {
			out[w.ID] = w.Connected
		}
		return out
	}
	waitFor(t, "A and B connected", func() bool { c := connected(); return c[idA] && c[idB] })
	if r := do(t, "DELETE", srv.URL+"/api/workers/"+idA, device, nil); r.StatusCode != 200 {
		t.Fatalf("revoke A: %d", r.StatusCode)
	}
	waitFor(t, "A gone, B still there", func() bool { c := connected(); return !c[idA] && c[idB] })
}

func TestDownload(t *testing.T) {
	tools := t.TempDir()
	bin := putTool(t, tools)
	_, srv := newHubWith(t, Config{Password: password, Data: t.TempDir(), Tools: tools})

	// no or a wrong token: 401
	for _, tok := range []string{"", "nope"} {
		if r, b := download(t, srv.URL, "Darwin-arm64", tok); r.StatusCode != 401 || bytes.Equal(b, bin) {
			t.Fatalf("download with %q: %d", tok, r.StatusCode)
		}
	}
	// another platform: 404 with a message, signed in or not
	device := signIn(t, srv.URL, "laptop")
	for _, p := range []string{"Linux-x86_64", "Darwin-x86_64", "Linux-aarch64"} {
		r, b := download(t, srv.URL, p, device)
		if r.StatusCode != 404 || !strings.Contains(string(b), "Apple Silicon Macs (Darwin-arm64)") {
			t.Fatalf("%s: %d %q", p, r.StatusCode, b)
		}
	}
	// a signed-in device: bearer, cookie, ?token=
	if r, b := download(t, srv.URL, "Darwin-arm64", device); r.StatusCode != 200 || !bytes.Equal(b, bin) {
		t.Fatalf("device bearer: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/tools/mlxlab/Darwin-arm64", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: device})
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
		t.Fatalf("device cookie: %v %v", r, err)
	}
	if r, _ := download(t, srv.URL, "Darwin-arm64?token="+device, ""); r.StatusCode != 200 {
		t.Fatalf("device query: %d", r.StatusCode)
	}

	// a hub image without the program: 404, not 500
	_, srv2 := newHubWith(t, Config{Password: password, Data: t.TempDir(), Tools: t.TempDir()})
	device2 := signIn(t, srv2.URL, "laptop")
	if r, _ := download(t, srv2.URL, "Darwin-arm64", device2); r.StatusCode != 404 {
		t.Fatalf("no program: %d", r.StatusCode)
	}
}

// workers.json keeps hashes only, survives a restart, and a lookup notes lastSeen.
func TestWorkersFile(t *testing.T) {
	dir := t.TempDir()
	_, srv := newHub(t, dir)
	device := signIn(t, srv.URL, "laptop")
	id, token := enroll(t, srv.URL, device, "Mac Studio")
	srv.Close()

	b, err := os.ReadFile(filepath.Join(dir, "workers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(token)) {
		t.Fatal("workers.json holds the plain token")
	}
	h := sha256.Sum256([]byte(token))
	if !bytes.Contains(b, []byte(hex.EncodeToString(h[:]))) {
		t.Fatalf("workers.json lacks the hash: %s", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".workers-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}

	ws, err := openWorkers(dir)
	if err != nil {
		t.Fatal(err)
	}
	wk, ok := ws.lookup(token)
	if !ok || wk.ID != id || wk.LastSeen.IsZero() {
		t.Fatalf("after restart: %+v %v", wk, ok)
	}
	if _, ok := ws.lookup("nope"); ok {
		t.Fatal("unknown token found")
	}
}
