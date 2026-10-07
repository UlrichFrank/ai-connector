package hub

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/tunnel"
)

const (
	password    = "correct horse"
	workerToken = "worker-secret"
)

func newHub(t *testing.T, dir string) (*Server, *httptest.Server) {
	t.Helper()
	return newHubWith(t, Config{Password: password, WorkerToken: workerToken, Data: dir, Tools: t.TempDir()})
}

func newHubWith(t *testing.T, cfg Config) (*Server, *httptest.Server) {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.failDelay = 50 * time.Millisecond
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func login(t *testing.T, base, pw, device string) (*http.Response, map[string]any) {
	t.Helper()
	return loginWith(t, base, map[string]string{"password": pw, "device": device, "user": "Philipp"})
}

// loginWith posts the body to /api/login.
func loginWith(t *testing.T, base string, body map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	r, err := http.Post(base+"/api/login", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var out map[string]any
	json.NewDecoder(r.Body).Decode(&out)
	return r, out
}

func signIn(t *testing.T, base, device string) string {
	t.Helper()
	r, out := login(t, base, password, device)
	if r.StatusCode != 200 {
		t.Fatalf("login: %d %v", r.StatusCode, out)
	}
	return out["token"].(string)
}

// get makes a request with the token as a bearer header ("" for none).
func do(t *testing.T, method, url, token string, body io.Reader) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func decode(t *testing.T, r *http.Response, v any) {
	t.Helper()
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// connectWorker attaches a fake worker through the real tunnel; the returned func disconnects it.
func connectWorker(t *testing.T, base string, h http.Handler) (disconnect func()) {
	t.Helper()
	disconnect, _ = connectWorkerWith(t, base, workerToken, h)
	return disconnect
}

// connectWorkerWith connects with the given token; ended yields what tunnel.Serve returned once it gave up
// (a refused token) or was stopped.
func connectWorkerWith(t *testing.T, base, token string, h http.Handler) (disconnect func(), ended <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	up := make(chan bool, 8)
	done := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- tunnel.Serve(ctx, base, token, "test-mac", h, func(c bool, _ error) {
			select {
			case up <- c:
			default:
			}
		})
		close(done)
	}()
	select {
	case c := <-up:
		if !c {
			t.Fatal("worker did not connect")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not connect in time")
	}
	// the worker hears "connected" once the WebSocket is up, a moment before the hub has taken the session
	deadline := time.Now().Add(5 * time.Second)
	for !info(t, base).Connected {
		if time.Now().After(deadline) {
			t.Fatal("the hub does not see the worker")
		}
		time.Sleep(5 * time.Millisecond)
	}
	var once bool
	disconnect = func() {
		if once {
			return
		}
		once = true
		cancel()
		<-done
	}
	t.Cleanup(disconnect)
	return disconnect, result
}

func TestNeedsPassword(t *testing.T) {
	if _, err := New(Config{WorkerToken: "x", Data: t.TempDir()}); err == nil {
		t.Fatal("started without a password")
	}
	// the shared worker token is optional now: Macs are enrolled in the web UI
	if _, err := New(Config{Password: "x", Data: t.TempDir()}); err != nil {
		t.Fatalf("without MLXLAB_WORKER_TOKEN: %v", err)
	}
}

func TestLogin(t *testing.T) {
	_, srv := newHub(t, t.TempDir())

	start := time.Now()
	r, out := login(t, srv.URL, "wrong", "x")
	if r.StatusCode != 401 {
		t.Fatalf("wrong password: %d", r.StatusCode)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("wrong password answered without delay")
	}
	if len(r.Cookies()) != 0 || out["token"] != nil {
		t.Fatal("wrong password got a session")
	}

	r, out = login(t, srv.URL, password, "Philipp's iPhone")
	if r.StatusCode != 200 {
		t.Fatalf("login: %d", r.StatusCode)
	}
	token, _ := out["token"].(string)
	if len(token) != 43 { // 32 bytes, base64url without padding
		t.Fatalf("token %q", token)
	}
	dev := out["device"].(map[string]any)
	if dev["name"] != "Philipp's iPhone" || dev["id"] == "" || dev["created"] == nil || dev["lastSeen"] == nil {
		t.Fatalf("device %v", dev)
	}
	c := r.Cookies()
	if len(c) != 1 || c[0].Name != CookieName || c[0].Value != token || !c[0].HttpOnly ||
		c[0].SameSite != http.SameSiteLaxMode || c[0].MaxAge < 360*24*3600 || c[0].Secure {
		t.Fatalf("cookie %+v", c)
	}

	// behind traefik with TLS the cookie is Secure
	req, _ := http.NewRequest("POST", srv.URL+"/api/login", strings.NewReader(`{"password":"`+password+`","user":"Philipp"}`))
	req.Header.Set("X-Forwarded-Proto", "https")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if c := r2.Cookies(); len(c) != 1 || !c[0].Secure {
		t.Fatalf("cookie over https %+v", c)
	}
}

func TestLoginRateLimit(t *testing.T) {
	s, srv := newHub(t, t.TempDir())
	s.failDelay = 0
	for i := 0; i < 10; i++ {
		if r, _ := login(t, srv.URL, "wrong", ""); r.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, r.StatusCode)
		}
	}
	if r, _ := login(t, srv.URL, password, ""); r.StatusCode != 429 {
		t.Fatalf("11th attempt: %d", r.StatusCode)
	}
}

func TestAuth(t *testing.T) {
	_, srv := newHub(t, t.TempDir())

	var info map[string]any
	decode(t, do(t, "GET", srv.URL+"/api/info", "", nil), &info)
	if info["app"] != "mlxlab" || info["role"] != "hub" || info["signedIn"] != false {
		t.Fatalf("info %v", info)
	}
	if w := info["worker"].(map[string]any); w["connected"] != false {
		t.Fatalf("worker %v", w)
	}

	// without a token: 401 signin, for the hub's endpoints and for the worker's alike
	for _, p := range []string{"/api/devices", "/api/worker/events", "/api/models", "/files/a.png"} {
		r := do(t, "GET", srv.URL+p, "", nil)
		var e map[string]string
		decode(t, r, &e)
		if r.StatusCode != 401 || e["error"] != "signin" {
			t.Fatalf("%s: %d %v", p, r.StatusCode, e)
		}
	}
	if r := do(t, "GET", srv.URL+"/api/devices", "not-a-token", nil); r.StatusCode != 401 {
		t.Fatalf("bad token: %d", r.StatusCode)
	}

	token := signIn(t, srv.URL, "laptop")
	// bearer
	if r := do(t, "GET", srv.URL+"/api/devices", token, nil); r.StatusCode != 200 {
		t.Fatalf("bearer: %d", r.StatusCode)
	}
	// cookie
	req, _ := http.NewRequest("GET", srv.URL+"/api/devices", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: token})
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 200 {
		t.Fatalf("cookie: %d", r.StatusCode)
	}
	// ?token=
	if r := do(t, "GET", srv.URL+"/api/devices?token="+token, "", nil); r.StatusCode != 200 {
		t.Fatalf("query: %d", r.StatusCode)
	}
	decode(t, do(t, "GET", srv.URL+"/api/info", token, nil), &info)
	if info["signedIn"] != true {
		t.Fatalf("info signed in %v", info)
	}

	// the static UI needs no token
	if r := do(t, "GET", srv.URL+"/", "", nil); r.StatusCode == 401 {
		t.Fatal("UI asks for sign-in")
	}
	// the tunnel checks its own token, a device token is not it
	if r := do(t, "GET", srv.URL+tunnel.Path, token, nil); r.StatusCode != 401 {
		t.Fatalf("tunnel with a device token: %d", r.StatusCode)
	}
}

func TestLogoutAndRevoke(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	phone := signIn(t, srv.URL, "phone")
	laptop := signIn(t, srv.URL, "laptop")

	var list []map[string]any
	decode(t, do(t, "GET", srv.URL+"/api/devices", laptop, nil), &list)
	if len(list) != 2 {
		t.Fatalf("devices %v", list)
	}
	var phoneID string
	for _, d := range list {
		if d["name"] == "laptop" && d["current"] != true || d["name"] == "phone" && d["current"] != false {
			t.Fatalf("current flag %v", list)
		}
		if d["name"] == "phone" {
			phoneID = d["id"].(string)
		}
	}

	if r := do(t, "DELETE", srv.URL+"/api/devices/nope", laptop, nil); r.StatusCode != 404 {
		t.Fatalf("unknown device: %d", r.StatusCode)
	}
	if r := do(t, "DELETE", srv.URL+"/api/devices/"+phoneID, laptop, nil); r.StatusCode != 200 {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	if r := do(t, "GET", srv.URL+"/api/devices", phone, nil); r.StatusCode != 401 {
		t.Fatalf("revoked phone: %d", r.StatusCode)
	}

	r := do(t, "POST", srv.URL+"/api/logout", laptop, nil)
	if r.StatusCode != 200 {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if c := r.Cookies(); len(c) != 1 || c[0].Name != CookieName || c[0].MaxAge >= 0 {
		t.Fatalf("logout cookie %+v", c)
	}
	if r := do(t, "GET", srv.URL+"/api/devices", laptop, nil); r.StatusCode != 401 {
		t.Fatalf("after logout: %d", r.StatusCode)
	}
}

func TestDevicesSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	_, srv := newHub(t, dir)
	token := signIn(t, srv.URL, "phone")
	srv.Close()

	b, err := os.ReadFile(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(token)) {
		t.Fatal("devices.json holds the plain token")
	}
	h := sha256.Sum256([]byte(token))
	if !bytes.Contains(b, []byte(hex.EncodeToString(h[:]))) {
		t.Fatalf("devices.json lacks the hash: %s", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".devices-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}

	_, srv2 := newHub(t, dir)
	var list []map[string]any
	r := do(t, "GET", srv2.URL+"/api/devices", token, nil)
	if r.StatusCode != 200 {
		t.Fatalf("after restart: %d", r.StatusCode)
	}
	decode(t, r, &list)
	if len(list) != 1 || list[0]["name"] != "phone" || list[0]["current"] != true {
		t.Fatalf("devices %v", list)
	}
}

func TestLastSeen(t *testing.T) {
	d, err := openDevices(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	token, dev, _ := d.add("x", "Philipp", "")
	old := dev.LastSeen.Add(-2 * time.Minute)
	d.byHash[hashToken(token)].LastSeen = old
	got, ok := d.lookup(token)
	if !ok || !got.LastSeen.After(old) {
		t.Fatalf("lastSeen not updated: %v", got.LastSeen)
	}
	// within a minute it stays put (no write per request)
	seen := got.LastSeen
	got, _ = d.lookup(token)
	if !got.LastSeen.Equal(seen) {
		t.Fatal("lastSeen moved within a minute")
	}
}

func TestOffline(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	token := signIn(t, srv.URL, "x")
	check := func(when string) {
		t.Helper()
		r := do(t, "GET", srv.URL+"/api/models", token, nil)
		var e map[string]string
		decode(t, r, &e)
		if r.StatusCode != 503 || e["error"] != "offline" || e["message"] != "The Mac is offline" {
			t.Fatalf("%s: %d %v", when, r.StatusCode, e)
		}
	}
	check("never connected")

	worker := http.NewServeMux()
	worker.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "[]") })
	disconnect := connectWorker(t, srv.URL, worker)
	if r := do(t, "GET", srv.URL+"/api/models", token, nil); r.StatusCode != 200 {
		t.Fatalf("online: %d", r.StatusCode)
	}
	disconnect()
	deadline := time.Now().Add(3 * time.Second)
	for {
		r := do(t, "GET", srv.URL+"/api/models", token, nil)
		r.Body.Close()
		if r.StatusCode == 503 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	check("after disconnect")
}

func TestProxy(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	token := signIn(t, srv.URL, "x")

	file := make([]byte, 100_000)
	rand.Read(file)
	gate := make(chan struct{})

	worker := http.NewServeMux()
	worker.HandleFunc("/api/echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"method":        r.Method,
			"path":          r.URL.Path,
			"query":         r.URL.RawQuery,
			"authorization": r.Header.Get("Authorization"),
			"cookie":        r.Header.Get("Cookie"),
			"proto":         r.Header.Get("X-Forwarded-Proto"),
		})
	})
	worker.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		select { // the second event only after the client has seen the first: proves it is not buffered
		case <-gate:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: two\n\n")
	})
	worker.HandleFunc("/files/song.bin", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "song.bin", time.Time{}, bytes.NewReader(file))
	})
	worker.HandleFunc("POST /api/upload", func(w http.ResponseWriter, r *http.Request) {
		f, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		h := sha256.New()
		n, _ := io.Copy(h, f)
		json.NewEncoder(w).Encode(map[string]any{"name": hdr.Filename, "size": n, "sha": hex.EncodeToString(h.Sum(nil)), "title": r.FormValue("title")})
	})
	connectWorker(t, srv.URL, worker)

	t.Run("json and stripped credentials", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/api/echo?a=1&token="+token+"&b=2", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Forwarded-Proto", "https")
		req.AddCookie(&http.Cookie{Name: CookieName, Value: token})
		req.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		decode(t, r, &got)
		if r.StatusCode != 200 || got["path"] != "/api/echo" || got["query"] != "a=1&b=2" {
			t.Fatalf("%d %v", r.StatusCode, got)
		}
		if got["authorization"] != "" || strings.Contains(got["cookie"], token) || got["cookie"] != "theme=dark" {
			t.Fatalf("credentials reached the worker: %v", got)
		}
		if got["proto"] != "https" {
			t.Fatalf("forwarded proto %q", got["proto"])
		}
		// a cookie-only request: nothing of the session is left
		req, _ = http.NewRequest("POST", srv.URL+"/api/echo", nil)
		req.AddCookie(&http.Cookie{Name: CookieName, Value: token})
		r, _ = http.DefaultClient.Do(req)
		decode(t, r, &got)
		if got["method"] != "POST" || got["cookie"] != "" {
			t.Fatalf("cookie only: %v", got)
		}
	})

	t.Run("sse streams", func(t *testing.T) {
		r := do(t, "GET", srv.URL+"/api/events?token="+token, "", nil)
		defer r.Body.Close()
		if ct := r.Header.Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("content type %q", ct)
		}
		br := bufio.NewReader(r.Body)
		line := make(chan string)
		go func() {
			for {
				s, err := br.ReadString('\n')
				if err != nil {
					close(line)
					return
				}
				line <- s
			}
		}()
		select {
		case s := <-line:
			if s != "data: one\n" {
				t.Fatalf("first line %q", s)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the first event was held back")
		}
		close(gate)
		for s := range line {
			if s == "data: two\n" {
				return
			}
		}
		t.Fatal("no second event")
	})

	t.Run("range", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/files/song.bin", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Range", "bytes=1000-1999")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		if r.StatusCode != 206 || !bytes.Equal(b, file[1000:2000]) ||
			r.Header.Get("Content-Range") != fmt.Sprintf("bytes 1000-1999/%d", len(file)) || r.Header.Get("Accept-Ranges") != "bytes" {
			t.Fatalf("range: %d %d bytes %v", r.StatusCode, len(b), r.Header)
		}
		// the whole file too
		r = do(t, "GET", srv.URL+"/files/song.bin", token, nil)
		b, _ = io.ReadAll(r.Body)
		r.Body.Close()
		if !bytes.Equal(b, file) {
			t.Fatal("full file differs")
		}
	})

	t.Run("upload", func(t *testing.T) {
		data := make([]byte, 20<<20)
		rand.Read(data)
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		mw.WriteField("title", "demo")
		fw, _ := mw.CreateFormFile("file", "voice.wav")
		fw.Write(data)
		mw.Close()
		req, _ := http.NewRequest("POST", srv.URL+"/api/upload", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+token)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		decode(t, r, &got)
		h := sha256.Sum256(data)
		if r.StatusCode != 200 || got["name"] != "voice.wav" || got["title"] != "demo" ||
			got["size"] != float64(len(data)) || got["sha"] != hex.EncodeToString(h[:]) {
			t.Fatalf("upload: %d %v", r.StatusCode, got)
		}
	})
}

func TestWorkerEvents(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	token := signIn(t, srv.URL, "x")
	r := do(t, "GET", srv.URL+"/api/worker/events", token, nil)
	defer r.Body.Close()
	if r.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %q", r.Header.Get("Content-Type"))
	}
	events := make(chan workerState, 8)
	go func() {
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			if s, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var st workerState
				json.Unmarshal([]byte(s), &st)
				events <- st
			}
		}
		close(events)
	}()
	next := func() workerState {
		t.Helper()
		select {
		case st := <-events:
			return st
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
		}
		return workerState{}
	}
	if st := next(); st.Connected {
		t.Fatalf("first event %+v", st)
	}
	disconnect := connectWorker(t, srv.URL, http.NewServeMux())
	st := next()
	if !st.Connected || st.Name != "test-mac" || st.Since == "" {
		t.Fatalf("after connect %+v", st)
	}
	if _, err := time.Parse(time.RFC3339, st.Since); err != nil {
		t.Fatalf("since %q", st.Since)
	}
	disconnect()
	if st := next(); st.Connected {
		t.Fatalf("after disconnect %+v", st)
	}
}
