package hub

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/jobs"
)

// pngBytes is a tiny image (only its bytes matter here).
var pngBytes = []byte("\x89PNG\r\n\x1a\nfake-red-square")

var pngB64 = base64.StdEncoding.EncodeToString(pngBytes)

// noBase64 checks that the worker's history keeps no image bytes: references only, the files in the outputs.
func noBase64(t *testing.T, mgr *jobs.Manager) {
	t.Helper()
	waitFor(t, "history written", mgr.Settled)
	b, err := os.ReadFile(filepath.Join(mgr.Dir, "history.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(pngB64)) {
		t.Fatalf("history.json has the image's base64")
	}
	if !bytes.Contains(b, []byte(`"file"`)) || !bytes.Contains(b, []byte("chat-images")) {
		t.Fatalf("history.json has no image reference: %s", b)
	}
}

// /v1/chat/completions: image_url parts with data URLs become images of the message, named in its text by tags; a
// model without the capability, http(s) URLs, other types and too many images are refused. /v1/models says which
// models take images.
func TestChatCompletionsImages(t *testing.T) {
	defer func(d time.Duration) { chatPoll = d }(chatPoll)
	chatPoll = 200 * time.Millisecond
	_, srv := newHub(t, t.TempDir())
	base := srv.URL
	device := signIn(t, base, "laptop")
	_, token := makeClient(t, base, device, "app")
	_, wtok := enroll(t, base, device, "Mac")
	mac := startVboardMac(t, base, wtok, "chat", "vision")

	r := do(t, "GET", base+"/v1/models", token, nil)
	var models struct {
		Data []struct {
			ID     string
			Images *bool
		}
	}
	decode(t, r, &models)
	caps := map[string]bool{}
	for _, m := range models.Data {
		if m.Images == nil {
			t.Fatalf("chat model %s without images", m.ID)
		}
		caps[m.ID] = *m.Images
	}
	if caps["lmstudio/qwen3.6-27b"] != true || caps["qwen3.6-35b-a3b"] != false {
		t.Fatalf("/v1/models images %v", caps)
	}

	part := func(u string) string { return `{"type":"image_url","image_url":{"url":"` + u + `"}}` }
	body := func(model string, parts ...string) string {
		return `{"model":"` + model + `","messages":[{"role":"user","content":[{"type":"text","text":"what colour is"},` +
			strings.Join(parts, ",") + `,{"type":"text","text":"?"}]}]}`
	}
	data := "data:image/png;base64," + pngB64
	c, b := post(t, base+"/v1/chat/completions", token, body("lmstudio/qwen3.6-27b", part(data)))
	if c != 200 {
		t.Fatalf("vision chat %d %s", c, b)
	}
	got := mac.chat.last()
	m := got.Messages[len(got.Messages)-1]
	if m.Content != "what colour is\n[image-1]\n?" || len(m.Images) != 1 || m.Images[0].ID != "image-1" || m.Images[0].MIME != "image/png" ||
		!bytes.Equal(m.Images[0].Data, pngBytes) {
		t.Fatalf("the job's message %+v", m)
	}
	noBase64(t, mac.mgr)

	for _, x := range []struct{ body, want string }{
		{body("qwen3.6-35b-a3b", part(data)), "does not take images"},
		{body("lmstudio/qwen3.6-27b", part("https://example.com/a.png")), "not supported"},
		{body("lmstudio/qwen3.6-27b", part("data:image/bmp;base64,"+pngB64)), "not taken"},
		{body("lmstudio/qwen3.6-27b", part("data:image/png;base64,!!!")), "does not decode"},
		{body("lmstudio/qwen3.6-27b", strings.TrimSuffix(strings.Repeat(part(data)+",", 9), ",")), "at most 8"},
		{`{"model":"lmstudio/qwen3.6-27b","messages":[{"role":"assistant","content":[` + part(data) + `]}]}`, "user messages"},
	} {
		c, b := post(t, base+"/v1/chat/completions", token, x.body)
		if c != 400 || !strings.Contains(string(b), x.want) {
			t.Fatalf("%.80s: %d %s", x.body, c, b)
		}
		if typ, _ := openAIError(t, b); typ != "invalid_request_error" {
			t.Fatalf("error type %s", typ)
		}
	}
}

// A connector advertises images per agent (and hello.images for a default that takes them), passes a user
// message's images into the chat job of an agent that takes them — Claude Code gets them as image blocks where the
// tags are — and never to one that does not: there the tags become "(image omitted)".
func TestVboardImages(t *testing.T) {
	fastBridges(t)
	s, srv := newHub(t, t.TempDir())
	t.Cleanup(s.Close)
	base := srv.URL
	alice := signInAs(t, base, "Alice's laptop", "Alice")
	_, qwenTok := enroll(t, base, alice, "Studio")
	_, aliceTok := enroll(t, base, alice, "Alice's Mac")
	qmac := startVboardMac(t, base, qwenTok, "chat", "vision")
	mac := startFakeClaudeMac(t, base, aliceTok)
	v := newFakeVboard(t, stageImages)
	addConnector(t, base, alice, `{"name":"Alice AI","url":"`+v.srv.URL+`","token":"vbp_secret",
		"agents":[{"model":"qwen3.6-35b-a3b"},{"id":"vision","model":"lmstudio/qwen3.6-27b"},{"id":"claude","model":"claude-code/haiku"}]}`)
	b := v.next(t)
	for len(b.agents) < 3 {
		b.agents = nextUpdate(t, b)
	}
	flags := map[string]bool{}
	for _, a := range b.agents {
		flags[a.ID] = a.Images
	}
	if flags["qwen3.6-35b-a3b"] || !flags["vision"] || !flags["claude"] || b.hello.Images {
		t.Fatalf("advertised %+v hello %+v", b.agents, b.hello)
	}
	if !strings.Contains(string(b.raw), `"images":true`) {
		t.Fatalf("hello %s", b.raw)
	}

	img := `"images":[{"id":"image-1","mime":"image/png","data":"` + pngB64 + `"}]`
	msgs := `[{"role":"user","content":"what colour is [image-1]?",` + img + `}]`

	// a model that takes images
	_, ch := b.chatWith(msgs, "", `"agent":"vision"`)
	if evs := until(t, ch); evs[len(evs)-1].Type != "done" {
		t.Fatalf("vision chat %+v", evs)
	}
	m := qmac.chat.last().Messages[0]
	if m.Content != "what colour is [image-1]?" || len(m.Images) != 1 || !bytes.Equal(m.Images[0].Data, pngBytes) {
		t.Fatalf("vision job message %+v", m)
	}
	noBase64(t, qmac.mgr)
	// the job as the worker lists it: the image by its /files/ url, no bytes
	for _, j := range qmac.mgr.Jobs() {
		if j.Model != "lmstudio/qwen3.6-27b" {
			continue
		}
		im := j.Request.Messages[0].Images[0]
		if len(im.Data) != 0 || !strings.Contains(im.File, "chat-images") {
			t.Fatalf("kept image %+v", im)
		}
	}

	// a model that does not: no images, the tags say so
	_, ch = b.chatWith(msgs, "", `"agent":"qwen3.6-35b-a3b"`)
	if evs := until(t, ch); evs[len(evs)-1].Type != "done" {
		t.Fatalf("text chat %+v", evs)
	}
	if m := qmac.chat.last().Messages[0]; m.Content != "what colour is (image omitted)?" || len(m.Images) != 0 {
		t.Fatalf("text job message %+v", m)
	}

	// Claude Code (an agent on the board): the image as a block where its tag is (the fake echoes it)
	cx := `"agent":"claude","context":{"boardId":"b_1","userId":"u_1","userName":"Anna","conversationId":"conv-img"}`
	_, ch = b.chatWith(msgs, "", cx)
	evs := until(t, ch)
	done := evs[len(evs)-1]
	if done.Type != "done" || !strings.Contains(done.Content, "what colour is [image-1]<image image/png "+itoa(len(pngBytes))+">?") {
		t.Fatalf("claude turn %+v", evs)
	}
	noBase64(t, mac.mgr)

	// a broken image is refused
	_, ch = b.chatWith(`[{"role":"user","content":"x [image-1]","images":[{"id":"image-1","mime":"image/bmp","data":"`+pngB64+`"}]}]`, "", `"agent":"vision"`)
	if evs := until(t, ch); evs[0].Type != "error" || !strings.Contains(evs[0].Error, "not taken") {
		t.Fatalf("broken image %+v", evs)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// hello.images: the default (the first agent, a model) takes images; a vboard reading only hello learns it there.
func TestHelloImages(t *testing.T) {
	vis := AdvertisedAgent{ID: "v", Kind: kindModel, Images: true}
	txt := AdvertisedAgent{ID: "t", Kind: kindModel}
	if !(bridgeCaps{agents: []AdvertisedAgent{vis, txt}}).images() || (bridgeCaps{agents: []AdvertisedAgent{txt, vis}}).images() ||
		(bridgeCaps{}).images() {
		t.Fatal("images of the default")
	}
	if (bridgeCaps{agents: []AdvertisedAgent{vis}}).sameHello(bridgeCaps{agents: []AdvertisedAgent{txt}}) {
		t.Fatal("a changed hello.images is a new hello")
	}
}
