package hub

import (
	"crypto/subtle"
	"html/template"
	"log"
	"net/http"
	"strings"

	"github.com/philipparndt/ai-connector/internal/models"
)

// The setup page of the standalone hub (`ai-connector vboard`): what must not go through vboard — today the Hugging
// Face token for gated models. vboard links to it (hello.setup.setupUrl); it is served on the hub's loopback address
// only (a browser on this computer), and only with the code made per process (other web pages do not know it). The
// token is stored where `hf auth login` keeps it, so it never passes vboard.

type setupPage struct {
	base string // http://127.0.0.1:<port>
	code string
}

// EnableSetupPage serves /setup on the hub, whose loopback address is base (e.g. http://127.0.0.1:53412), and
// returns its address (with the code). Call it before Serve.
func (s *Server) EnableSetupPage(base string) string {
	s.setup = &setupPage{base: strings.TrimRight(base, "/"), code: randomString(24)}
	return s.setupURL()
}

// setupURL is the setup page's address ("" none: a server, see docs/api.md).
func (s *Server) setupURL() string {
	if s.setup == nil {
		return ""
	}
	return s.setup.base + "/setup?code=" + s.setup.code
}

func (p *setupPage) valid(code string) bool {
	return subtle.ConstantTimeCompare([]byte(code), []byte(p.code)) == 1
}

// hfToken checks a pasted token: one word of printable ASCII (Hugging Face's start with hf_).
func hfToken(t string) (string, bool) {
	t = strings.TrimSpace(t)
	if len(t) < 8 || len(t) > 400 {
		return "", false
	}
	for _, r := range t {
		if r <= ' ' || r > '~' {
			return "", false
		}
	}
	return t, true
}

var setupTmpl = template.Must(template.New("setup").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>ai-connector setup</title>
<style>
:root{color-scheme:light dark;--bg:#f6f6f4;--fg:#1d1d1b;--muted:#6b6b66;--card:#fff;--line:#d9d9d4;--accent:#2f6fde;--ok:#1f7a3d;--bad:#b3261e}
@media (prefers-color-scheme:dark){:root{--bg:#161615;--fg:#ececea;--muted:#a3a39d;--card:#20201f;--line:#3a3a37;--accent:#6c9cf0;--ok:#5fc27e;--bad:#f2867d}}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
main{max-width:34rem;margin:3rem auto;padding:0 16px}
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:1.5rem}
h1{font-size:1.3rem;margin:0 0 .5rem}p{margin:.5rem 0}.muted{color:var(--muted);font-size:.9rem}
label{display:block;font-weight:600;margin:1rem 0 .3rem}
input[type=password]{box-sizing:border-box;width:100%;padding:.6rem;border:1px solid var(--line);border-radius:8px;background:var(--bg);color:var(--fg);font:inherit}
button{margin-top:1rem;padding:.6rem 1.2rem;border:0;border-radius:8px;background:var(--accent);color:#fff;font:inherit;cursor:pointer}
a{color:var(--accent)}.ok{color:var(--ok);font-weight:600}.bad{color:var(--bad);font-weight:600}
ol{padding-left:1.2rem}
</style></head><body><main><div class="card">
{{if .Bad}}<h1>This link does not work</h1><p class="bad">{{.Bad}}</p><p>Open the setup again from vboard ("Connect Hugging Face").</p>
{{else if .Saved}}<h1>Hugging Face is connected</h1><p class="ok">The token is saved on this computer.</p><p>Go back to vboard: it can download the model now. You can close this tab.</p>
{{else}}<h1>Connect Hugging Face</h1>
<p>Some models (FLUX.2 [klein] 9B, LTX-2.5) are gated: Hugging Face lets this computer download them only with a token of your account, once you have accepted their licence.</p>
<ol class="muted"><li>Make a token on <a href="https://huggingface.co/settings/tokens" target="_blank" rel="noreferrer noopener">huggingface.co/settings/tokens</a> (the type "Read" is enough).</li>
<li>Paste it here. It is stored on this computer only ({{.Path}}), where <code>hf auth login</code> keeps it — it does not go through vboard.</li></ol>
{{if .Has}}<p class="muted">A token is set already; a new one replaces it.</p>{{end}}
{{if .Wrong}}<p class="bad">{{.Wrong}}</p>{{end}}
<form method="post" action="/setup" autocomplete="off"><input type="hidden" name="code" value="{{.Code}}">
<label for="token">Hugging Face token</label><input type="password" id="token" name="token" placeholder="hf_…" required autofocus>
<button type="submit">Save</button></form>{{end}}
</div></main></body></html>
`))

type setupView struct {
	Code, Path, Bad, Wrong string
	Has, Saved             bool
}

func (s *Server) serveSetupPage(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer") // the code is in the address
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	p := s.setup
	v := setupView{Path: models.HFTokenPath(), Has: models.HFToken() != ""}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if !p.valid(r.URL.Query().Get("code")) {
			w.WriteHeader(http.StatusForbidden)
			v.Bad = "The code in the address is missing or old (it changes when ai-connector restarts)."
		}
		v.Code = r.URL.Query().Get("code")
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if err := r.ParseForm(); err != nil || !p.valid(r.PostForm.Get("code")) {
			w.WriteHeader(http.StatusForbidden)
			v.Bad = "The code is missing or old (it changes when ai-connector restarts)."
			break
		}
		v.Code = r.PostForm.Get("code")
		token, ok := hfToken(r.PostForm.Get("token"))
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			v.Wrong = "That does not look like a Hugging Face token (one word, like hf_…)."
			break
		}
		if err := models.SaveHFToken(token); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			v.Wrong = "The token could not be saved: " + err.Error()
			break
		}
		log.Printf("setup: a Hugging Face token was saved in %s", models.HFTokenPath())
		v.Saved = true
		s.fleet.notify() // the connectors tell vboard: setup.huggingFace.token is true now
	default:
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	_ = setupTmpl.Execute(w, v)
}
