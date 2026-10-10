// Package web is the worker's API: JSON over the job manager, server-sent events for live updates, the generated
// files, and the web UI. It has no sign-in: it is served through the tunnel to the hub (which signs people in, see
// docs/architecture.md) or, by mlxlab serve, on localhost only.
package web

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/philipparndt/ai-connector/internal/agent"
	"github.com/philipparndt/ai-connector/internal/agents"
	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/styles"
	"github.com/philipparndt/ai-connector/internal/voices"
	"github.com/philipparndt/ai-connector/internal/webui"
)

//go:embed static
var static embed.FS

// Server serves the app.
type Server struct {
	Jobs    *jobs.Manager
	Prompts *prompts.Library
	// Voices is the voice library (nil: <Home>/voices, transcribed by the "speech" backend)
	Voices *voices.Library
	// Styles is the library of trained styles (nil: the music backend's, else <Home>/styles)
	Styles *styles.Library
	// Home is the config dir the voices and styles are shared in ("": Jobs.Dir — they differ when the worker
	// keeps its jobs in a state dir of its own, mlxlab worker --state).
	Home string
	// HomeTools lists the home zone's tools now, by MCP server (a server that does not answer has its Error set);
	// err: the servers cannot be listed at all (mcp.json unreadable). nil: no home tools on this worker.
	HomeTools func(ctx context.Context) ([]ToolServer, error)
	// Version is mlxlab's build (GET /api/info).
	Version string
	// Agents are the Claude Code sessions (agents.go; nil: none on this worker).
	Agents *agents.Manager
}

// ToolServer is one MCP server of the home zone and its tools.
type ToolServer struct {
	Name  string
	Tools []agent.Tool
	Error string // it did not answer
}

// Handler is the app's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	classic, _ := fs.Sub(static, "static")
	classicFiles := http.StripPrefix("/classic", http.FileServerFS(classic))
	mux.Handle("GET /classic/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		classicFiles.ServeHTTP(w, r)
	}))
	// the React app (web/, built into internal/webui); until it is built, the classic page
	if webui.Built() {
		mux.Handle("GET /", webui.Handler())
	} else {
		mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-cache")
			http.FileServerFS(classic).ServeHTTP(w, r)
		}))
	}
	mux.HandleFunc("GET /api/models", s.models)
	mux.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		all := s.Jobs.Jobs()
		out := make([]*jobs.Job, len(all))
		for i, j := range all {
			out[i] = s.view(j)
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, ok := s.Jobs.Job(r.PathValue("id"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, s.view(j))
	})
	mux.HandleFunc("POST /api/jobs", s.submit)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Jobs.Cancel(r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// a chat's tool call that changes something waits for the user (agent.ToolEvent state "approval")
	mux.HandleFunc("POST /api/jobs/{id}/approve", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Call   string `json:"call"`
			Allow  bool   `json:"allow"`
			Always bool   `json:"always"` // and allow the tool always (approvals.json)
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.Call == "" || (body.Always && !body.Allow) {
			http.Error(w, `{"call": "<tool call id>", "allow": true|false, "always": true (with allow)}`, http.StatusBadRequest)
			return
		}
		if _, ok := s.Jobs.Job(r.PathValue("id")); !ok {
			http.NotFound(w, r)
			return
		}
		var err error
		if body.Always {
			err = s.Jobs.AllowAlways(r.PathValue("id"), body.Call)
		} else {
			err = s.Jobs.Approve(r.PathValue("id"), body.Call, body.Allow)
		}
		switch {
		case errors.Is(err, jobs.ErrNotAlways):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case err != nil:
			http.Error(w, err.Error(), http.StatusConflict) // decided, timed out, or the job is over
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	// a chat as text, for copying (docs/chat-tools.md): ?thinking=1, ?full=1
	mux.HandleFunc("GET /api/jobs/{id}/transcript", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.Jobs.Job(r.PathValue("id")); !ok {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		text, err := s.Jobs.Transcript(r.PathValue("id"), jobs.TranscriptOptions{Thinking: q.Get("thinking") == "1", Full: q.Get("full") == "1"})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, text)
	})
	// tools allowed always: home tools that change something and run without asking (docs/chat-tools.md)
	mux.HandleFunc("GET /api/approvals", s.listApprovals)
	mux.HandleFunc("PUT /api/approvals/{tool}", s.allowAlways)
	mux.HandleFunc("DELETE /api/approvals/{tool}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Jobs.ApprovalStore().Remove(r.PathValue("tool")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.listApprovals(w, r)
	})
	mux.HandleFunc("GET /api/tools", s.tools)
	mux.HandleFunc("POST /api/jobs/{id}/title", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Title string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		j, err := s.Jobs.Rename(r.PathValue("id"), body.Title)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, s.view(j))
	})
	mux.HandleFunc("POST /api/jobs/{id}/folder", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Folder string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		moved, err := s.Jobs.Move(r.PathValue("id"), body.Folder)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]int{"moved": len(moved)})
	})
	// rename a folder (to "": remove it — its songs move up)
	mux.HandleFunc("POST /api/folders/rename", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ From, To string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n, err := s.Jobs.RenameFolder(body.From, body.To)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]int{"moved": n})
	})
	mux.HandleFunc("POST /api/jobs/{id}/star", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Starred bool }
		if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		j, err := s.Jobs.Star(r.PathValue("id"), body.Starred)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, s.view(j))
	})
	// a song's cover: an image of the outputs (a /files/ path; "" removes it)
	mux.HandleFunc("POST /api/jobs/{id}/cover", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Image string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		file := ""
		if body.Image != "" {
			local, ok := s.local(body.Image)
			if !ok {
				http.Error(w, "unknown image "+body.Image, http.StatusBadRequest)
				return
			}
			file = local
		}
		j, err := s.Jobs.SetCover(r.PathValue("id"), file)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, s.view(j))
	})
	mux.HandleFunc("POST /api/jobs/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		j, err := s.Jobs.Retry(r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, s.view(j))
	})
	// a job goes, with the files it made (?files=0: only the entry in the list)
	mux.HandleFunc("DELETE /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("files") == "0" {
			s.Jobs.Delete(r.PathValue("id"))
		} else if err := s.Jobs.DeleteWithFiles(r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/unload", func(w http.ResponseWriter, r *http.Request) {
		go s.Jobs.Unload() // a backend busy with a job frees its memory when the job is done
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /api/upload", s.upload)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /files/{name...}", s.file)
	mux.HandleFunc("GET /api/prompts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"prompts": s.Prompts.All(), "uses": prompts.Uses})
	})
	// save: an existing id updates (a built-in one too), no id makes a new prompt
	mux.HandleFunc("POST /api/prompts", func(w http.ResponseWriter, r *http.Request) {
		var p prompts.Prompt
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		saved, err := s.Prompts.Save(p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, saved)
	})
	// your own prompt goes; a built-in one goes back to its default
	mux.HandleFunc("DELETE /api/prompts/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Prompts.Delete(r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// the voice library of the speech models (docs/voice-avatar.md)
	mux.HandleFunc("GET /api/voices", s.listVoices)
	mux.HandleFunc("POST /api/voices", s.addVoice)
	mux.HandleFunc("PATCH /api/voices/{id}", s.editVoice)
	mux.HandleFunc("DELETE /api/voices/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.voices().Delete(r.PathValue("id")); err != nil {
			voiceError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// trained styles of the song models (styles.go)
	mux.HandleFunc("GET /api/styles", s.listStyles)
	mux.HandleFunc("POST /api/styles", s.createStyle)
	mux.HandleFunc("GET /api/community-styles", s.communityStyles)
	mux.HandleFunc("GET /api/styles/{id}", s.getStyle)
	mux.HandleFunc("PATCH /api/styles/{id}", s.editStyle)
	mux.HandleFunc("DELETE /api/styles/{id}", s.deleteStyle)
	mux.HandleFunc("POST /api/styles/{id}/tracks", s.addTracks)
	mux.HandleFunc("DELETE /api/styles/{id}/tracks/{track}", s.deleteTrack)
	mux.HandleFunc("POST /api/styles/{id}/train", s.trainStyle)
	mux.HandleFunc("GET /api/info", s.info)
	// embedding and classifying for the server's client APIs, beside the queue (direct.go)
	mux.HandleFunc("POST /api/direct/embed", s.directEmbed)
	mux.HandleFunc("POST /api/direct/classify", s.directClassify)
	s.agentRoutes(mux)
	return s.routed(mux)
}

// SameOrigin refuses writes from other sites, for mlxlab serve: it listens on localhost, but a page in the browser
// could post to it. The tunnel does not need it: only the hub reaches the worker there, and it checks sign-in.
func SameOrigin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" && !strings.HasSuffix(o, "://"+r.Host) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// HostName is this Mac's name, without .local (the worker's name on the hub).
func HostName() string {
	h, _ := os.Hostname()
	return strings.TrimSuffix(h, ".local")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// discoverer is a backend whose models are found at run time (engine.OpenAIBackend: the local runtimes).
type discoverer interface {
	DiscoverNew(ctx context.Context) int
}

// refreshWait bounds what ?refresh=1 waits for the runtimes.
var refreshWait = 1500 * time.Millisecond

// models: GET /api/models. ?refresh=1 asks the local runtimes for their models first (at most 1.5 s; new ones are added) instead of
// waiting for the next scan (every 30 s): the hub asks so when a model it was asked for is listed nowhere (one
// pulled a moment ago).
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") != "" {
		ctx, cancel := context.WithTimeout(r.Context(), refreshWait)
		for _, b := range s.Jobs.Backends {
			if d, ok := b.(discoverer); ok {
				d.DiscoverNew(ctx)
			}
		}
		cancel()
	}
	writeJSON(w, map[string]any{"models": s.Jobs.Catalog.List(), "loaded": s.Jobs.Loaded(), "kinds": engine.Kinds,
		"examples": engine.Examples})
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var req engine.Request
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// reference images must be uploads (or earlier outputs): the page names them by their /files/ path
	for i, p := range req.Images {
		local, ok := s.local(p)
		if !ok {
			http.Error(w, "unknown image "+p, http.StatusBadRequest)
			return
		}
		req.Images[i] = local
	}
	if req.Source != "" {
		local, ok := s.local(req.Source)
		if !ok {
			http.Error(w, "unknown song "+req.Source, http.StatusBadRequest)
			return
		}
		req.Source = local
	}
	if req.Audio != "" { // speech to text, avatar: a /files/ url (an upload, an output, a voice sample)
		local, ok := s.local(req.Audio)
		if !ok {
			http.Error(w, "unknown audio "+req.Audio, http.StatusBadRequest)
			return
		}
		req.Audio = local
	}
	j, err := s.Jobs.Submit(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.view(j))
}

// local maps a /files/… URL to the file in the outputs directory.
func (s *Server) local(url string) (string, bool) {
	name, ok := strings.CutPrefix(url, "/files/")
	if !ok {
		return "", false
	}
	base := s.Jobs.OutDir()
	if rest, ok := strings.CutPrefix(name, "styles/"); ok {
		// a style's audio (tracks, samples), not its weights or style.json
		if !styleAudio(rest) {
			return "", false
		}
		base, name = s.styleLib().Dir, rest
	} else if rest, ok := strings.CutPrefix(name, "voices/"); ok {
		// the voice samples (only they: not voices.json)
		if strings.ToLower(filepath.Ext(rest)) != ".wav" {
			return "", false
		}
		base, name = s.voices().Dir, rest
	}
	p := filepath.Join(base, filepath.FromSlash(name))
	if rel, err := filepath.Rel(base, p); err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return p, true
}

func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	p, ok := s.local("/files/" + r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// generated svg/html (the files sandbox, maybe steered by web content) must not run scripts in the app's origin
	switch strings.ToLower(filepath.Ext(p)) {
	case ".svg", ".html", ".htm":
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline'; font-src data:")
	}
	http.ServeFile(w, r, p)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	f, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer f.Close()
	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	audio := false
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp":
	case ".wav", ".mp3", ".m4a", ".flac", ".ogg", ".aac":
		audio = true
	default:
		http.Error(w, "an image (png, jpg, webp) or a song (wav, mp3, m4a, flac, ogg)", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(s.Jobs.OutDir(), "uploads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var rb [4]byte
	_, _ = rand.Read(rb[:])
	name := time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(rb[:]) + ext
	out, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(out, io.LimitReader(f, 256<<20)); err != nil {
		out.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out.Close()
	url := "/files/uploads/" + name
	if audio && r.FormValue("import") != "" {
		// a song to work on in the studio: it joins the library
		title := strings.TrimSuffix(hdr.Filename, filepath.Ext(hdr.Filename))
		file := filepath.Join(dir, name)
		j := s.Jobs.Import(engine.Music, "Uploaded", file, engine.Request{Model: "upload", Title: title, Duration: audioSeconds(file),
			Folder: jobs.CleanFolder(r.FormValue("folder"))})
		writeJSON(w, map[string]any{"url": url, "job": s.view(j)})
		return
	}
	writeJSON(w, map[string]string{"url": url})
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// mapWith is a copy of m with one more entry.
func mapWith(m map[string]any, k string, v any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for a, b := range m {
		out[a] = b
	}
	out[k] = v
	return out
}

// audioSeconds is a sound file's length (ffprobe; 0 if unknown).
func audioSeconds(path string) float64 {
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return f
}

// url is the /files/ URL of an output file ("" if it is not one).
func (s *Server) url(p string) string {
	if p == "" {
		return ""
	}
	if rel, err := filepath.Rel(s.voices().Dir, p); err == nil && !strings.HasPrefix(rel, "..") && !strings.Contains(rel, string(filepath.Separator)) {
		return "/files/voices/" + rel
	}
	if rel, err := filepath.Rel(s.styleLib().Dir, p); err == nil && !strings.HasPrefix(rel, "..") {
		return "/files/styles/" + filepath.ToSlash(rel)
	}
	rel, err := filepath.Rel(s.Jobs.OutDir(), p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	return "/files/" + filepath.ToSlash(rel)
}

// view is a job as the page sees it: its files (and reference images) as /files/ URLs.
func (s *Server) view(j *jobs.Job) *jobs.Job {
	v := *j
	v.Files = make([]string, len(j.Files))
	for i, f := range j.Files {
		v.Files[i] = s.url(f)
	}
	v.Request.Images = make([]string, len(j.Request.Images))
	for i, f := range j.Request.Images {
		v.Request.Images[i] = s.url(f)
	}
	if j.Request.Source != "" {
		v.Request.Source = s.url(j.Request.Source)
	}
	if j.Request.Audio != "" {
		v.Request.Audio = s.url(j.Request.Audio)
	}
	if j.Preview != "" {
		v.Preview = s.url(j.Preview)
	}
	if j.Cover != "" {
		v.Cover = s.url(j.Cover)
	}
	if j.State == jobs.Failed && j.Problem == nil && j.Error != "" {
		p := jobs.Explain(j.Error)
		v.Problem = &p
	}
	v.Request.ABC = "" // the score is a file next to the song (scores), not repeated with every job
	if j.Kind == engine.Music && len(j.Files) > 0 {
		if abc := strings.TrimSuffix(j.Files[0], filepath.Ext(j.Files[0])) + ".abc"; fileExists(abc) {
			v.Stats = mapWith(v.Stats, "score", s.url(abc))
		}
	}
	return &v
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch, stop := s.Jobs.Subscribe()
	defer stop()
	fmt.Fprint(w, ": hello\n\n")
	fl.Flush()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case u, ok := <-ch:
			if !ok {
				return
			}
			if u.Job != nil {
				u.Job = s.view(u.Job)
			}
			if u.Event != nil && u.Event.File != "" {
				ev := *u.Event
				ev.File = s.url(ev.File)
				u.Event = &ev
			}
			b, _ := json.Marshal(u)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

// voices is the voice library: Voices, else the one in the config folder, transcribing with the speech backend.
func (s *Server) voices() *voices.Library {
	if s.Voices != nil {
		return s.Voices
	}
	lib := voices.Open(filepath.Join(s.home(), "voices"))
	if t, ok := s.Jobs.Backends["speech"].(interface {
		Transcribe(ctx context.Context, file, language string) (string, error)
	}); ok {
		lib.Transcribe = t.Transcribe
	}
	if p, ok := s.Jobs.Backends["speech"].(interface {
		PreviewVoice(ctx context.Context, v engine.Voice, out string) error
	}); ok {
		lib.Preview = p.PreviewVoice
	}
	return lib
}

// voiceView is a voice as the page sees it: the sample as a /files/ url.
func (s *Server) voiceView(v engine.Voice) engine.Voice {
	v.Sample = s.url(v.Sample)
	return v
}

func voiceError(w http.ResponseWriter, err error) {
	if errors.Is(err, voices.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}

func (s *Server) listVoices(w http.ResponseWriter, r *http.Request) {
	vs, err := s.voices().List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]engine.Voice, len(vs))
	for i, v := range vs {
		out[i] = s.voiceView(v)
	}
	writeJSON(w, map[string]any{"voices": out, "presets": engine.SpeechPresets()})
}

// addVoice: multipart (name, sample, optional transcript and language) clones a voice; JSON {name, description,
// language} designs one.
func (s *Server) addVoice(w http.ResponseWriter, r *http.Request) {
	lib := s.voices()
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
		f, _, err := r.FormFile("sample")
		if err != nil {
			http.Error(w, "a sample (audio file) of the voice: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer f.Close()
		v, err := lib.AddClone(r.Context(), r.FormValue("name"), f, r.FormValue("transcript"), r.FormValue("language"))
		if err != nil {
			voiceError(w, err)
			return
		}
		writeJSON(w, s.voiceView(v))
		return
	}
	var body struct{ Name, Description, Language string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v, err := lib.AddDesign(r.Context(), body.Name, body.Description, body.Language)
	if err != nil {
		voiceError(w, err)
		return
	}
	writeJSON(w, s.voiceView(v))
}

func (s *Server) editVoice(w http.ResponseWriter, r *http.Request) {
	var p voices.Patch
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v, err := s.voices().Update(r.Context(), r.PathValue("id"), p)
	if err != nil {
		voiceError(w, err)
		return
	}
	writeJSON(w, s.voiceView(v))
}
