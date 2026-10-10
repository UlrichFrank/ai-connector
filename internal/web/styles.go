package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/styles"
)

// The styles library (train your own style, docs/architecture.md): styles of the song models trained on the
// user's own tracks. Their audio (tracks, samples) is served as /files/styles/<id>/….

// styleLib is the music backend's library (one per worker); without a music backend, <Home>/styles.
func (s *Server) styleLib() *styles.Library {
	if s.Styles != nil {
		return s.Styles
	}
	if b, ok := s.Jobs.Backends["music"].(interface{ StyleLibrary() *styles.Library }); ok {
		return b.StyleLibrary()
	}
	return styles.Open(filepath.Join(s.home(), "styles"))
}

// home is the shared config dir (Home, else the jobs' dir).
func (s *Server) home() string {
	if s.Home != "" {
		return s.Home
	}
	return s.Jobs.Dir
}

// styleView is a style as the page sees it: files as /files/ urls, the status from its training job, an estimate
// of a training's time, the segments.
type styleView struct {
	styles.Style
	Seconds  float64        `json:"seconds"`  // all tracks
	Segments int            `json:"segments"` // what the tracks give
	Estimate float64        `json:"estimate"` // seconds a training (or what is left of it) takes, about
	Running  *jobs.Job      `json:"running,omitempty"`
	JobID    string         `json:"jobId,omitempty"` // the last training job (as "jobId": the route layer prefixes it)
	Tracks   []trackView    `json:"tracks"`
	Samples  []sampleView   `json:"samples"`
	Losses   []styles.Point `json:"losses"`
}

type trackView struct {
	styles.Track
	URL string `json:"url"`
}

type sampleView struct {
	Step int    `json:"step"`
	URL  string `json:"url"`
}

func (s *Server) styleView(st styles.Style) styleView {
	v := styleView{Style: st, Seconds: st.Seconds(), Segments: st.SegmentCount(), Tracks: []trackView{}, Samples: []sampleView{},
		Losses: st.Progress.Losses}
	if v.Losses == nil {
		v.Losses = []styles.Point{}
	}
	// the job decides: a style "training" whose job is gone (mlxlab stopped) is paused at its checkpoint
	if j, ok := s.Jobs.Job(st.Job); ok && j.Active() {
		if j.State == jobs.Queued {
			v.Status = styles.Queued
		} else {
			v.Status = styles.Training
		}
		v.Running = s.view(j)
	} else if st.Status == styles.Training || st.Status == styles.Queued {
		switch {
		case st.Progress.Checkpoint > 0:
			v.Status = styles.Paused
		case st.Weights != "":
			v.Status = styles.Trained
		default:
			v.Status = styles.Ready
		}
		v.Progress.Phase, v.Progress.ETA = "", 0
	}
	if v.Status == styles.New && len(st.Tracks) > 0 {
		v.Status = styles.Ready
	}
	for _, t := range st.Tracks {
		tv := trackView{Track: t, URL: s.url(t.File)}
		tv.File = ""
		v.Tracks = append(v.Tracks, tv)
	}
	for _, sm := range st.Progress.Samples {
		if u := s.url(sm.File); u != "" && fileExists(sm.File) {
			v.Samples = append(v.Samples, sampleView{Step: sm.Step, URL: u})
		}
	}
	v.Style.Tracks, v.Style.Progress.Samples, v.Style.Progress.Losses = nil, nil, nil
	v.Weights, v.JobID, v.Job = "", st.Job, ""
	if st.Weights != "" {
		v.Weights = "lora"
	}
	v.Estimate = engine.StyleEstimate(st)
	return v
}

func styleError(w http.ResponseWriter, err error) {
	if errors.Is(err, styles.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}

// trainableModels are the song models styles can be trained for, with a training step's typical seconds.
func (s *Server) trainableModels() []map[string]any {
	out := []map[string]any{}
	for _, m := range s.Jobs.Catalog.OfKind(engine.Music) {
		if m.Styles {
			out = append(out, map[string]any{"id": m.ID, "name": m.Name, "here": m.Here, "stepSeconds": engine.StepSeconds(m.ID),
				"described": engine.StyleDescribed(m.ID), "defaults": engine.StyleDefaults(m.ID)})
		}
	}
	return out
}

func (s *Server) listStyles(w http.ResponseWriter, r *http.Request) {
	all, err := s.styleLib().List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]styleView, len(all))
	for i, st := range all {
		out[i] = s.styleView(st)
	}
	writeJSON(w, map[string]any{"styles": out, "models": s.trainableModels(), "defaults": styles.Defaults(),
		"community": engine.CommunityStyles})
}

func (s *Server) getStyle(w http.ResponseWriter, r *http.Request) {
	st, err := s.styleLib().Get(r.PathValue("id"))
	if err != nil {
		styleError(w, err)
		return
	}
	writeJSON(w, s.styleView(st))
}

// communityBackend is the music backend's side of community styles (LoRAs from Hugging Face).
type communityBackend interface {
	LookupCommunityStyle(ctx context.Context, spec string) (engine.CommunityLookup, error)
	AddCommunityStyle(ctx context.Context, a engine.CommunityAdd) (styles.Style, error)
}

func (s *Server) community(w http.ResponseWriter) communityBackend {
	if b, ok := s.Jobs.Backends["music"].(communityBackend); ok {
		return b
	}
	http.Error(w, "this Mac makes no songs (no music backend): add community styles on a worker", http.StatusBadRequest)
	return nil
}

// communityStyles: GET /api/community-styles — the curated LoRAs; with ?repo=owner/name[@rev] what that repository
// says about itself (its commit, trigger word, base model, recommended settings …) before it is added.
func (s *Server) communityStyles(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeJSON(w, map[string]any{"community": engine.CommunityStyles})
		return
	}
	b := s.community(w)
	if b == nil {
		return
	}
	l, err := b.LookupCommunityStyle(r.Context(), repo)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, models.ErrNotFound) {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, l)
}

func (s *Server) createStyle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string `json:"name"`
		BaseModel string `json:"baseModel"`
		// a community style: a LoRA repository on Hugging Face (owner/name[@rev]); trigger when its metadata has none
		Repo     string  `json:"repo"`
		Rev      string  `json:"rev"`
		Trigger  string  `json:"trigger"`
		Strength float64 `json:"strength"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Repo != "" {
		b := s.community(w)
		if b == nil {
			return
		}
		st, err := b.AddCommunityStyle(r.Context(), engine.CommunityAdd{Repo: body.Repo, Rev: body.Rev, Name: body.Name,
			Trigger: body.Trigger, BaseModel: body.BaseModel, Strength: body.Strength})
		if err != nil {
			styleError(w, err)
			return
		}
		writeJSON(w, s.styleView(st))
		return
	}
	if body.BaseModel == "" {
		body.BaseModel = "ace-step-1.5-turbo"
	}
	m := s.Jobs.Catalog.Find(body.BaseModel)
	if m == nil || !m.Styles {
		http.Error(w, "styles can be trained for: "+strings.Join(trainableIDs(s), ", "), http.StatusBadRequest)
		return
	}
	st, err := s.styleLib().Create(body.Name, m.ID, engine.StyleDefaults(m.ID))
	if err != nil {
		styleError(w, err)
		return
	}
	writeJSON(w, s.styleView(st))
}

func trainableIDs(s *Server) []string {
	var ids []string
	for _, m := range s.trainableModels() {
		ids = append(ids, m["id"].(string))
	}
	return ids
}

// busy: the style's training job is queued or running (its tracks and settings stay as they are meanwhile).
func (s *Server) styleBusy(id string) bool {
	st, err := s.styleLib().Get(id)
	if err != nil {
		return false
	}
	j, ok := s.Jobs.Job(st.Job)
	return ok && j.Active()
}

func (s *Server) editStyle(w http.ResponseWriter, r *http.Request) {
	var p styles.Patch
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if p.Settings != nil && s.styleBusy(r.PathValue("id")) {
		http.Error(w, "the style is training: stop it first", http.StatusConflict)
		return
	}
	st, err := s.styleLib().Update(r.PathValue("id"), p)
	if err != nil {
		styleError(w, err)
		return
	}
	writeJSON(w, s.styleView(st))
}

func (s *Server) deleteStyle(w http.ResponseWriter, r *http.Request) {
	if s.styleBusy(r.PathValue("id")) {
		http.Error(w, "the style is training: stop it first", http.StatusConflict)
		return
	}
	if err := s.styleLib().Delete(r.PathValue("id")); err != nil {
		styleError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// addTracks: multipart, one or more "file" fields (any audio ffmpeg reads; "vocals": "1" when they have vocals).
func (s *Server) addTracks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.styleBusy(id) {
		http.Error(w, "the style is training: stop it first", http.StatusConflict)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "tracks (multipart, field \"file\"): "+err.Error(), http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		http.Error(w, "tracks (multipart, field \"file\")", http.StatusBadRequest)
		return
	}
	vocals := r.FormValue("vocals") == "1" || r.FormValue("vocals") == "true"
	lib := s.styleLib()
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, _, err = lib.AddTrack(r.Context(), id, fh.Filename, f, vocals)
		f.Close()
		if err != nil {
			styleError(w, errors.Join(errors.New(fh.Filename+":"), err))
			return
		}
	}
	st, err := lib.Get(id)
	if err != nil {
		styleError(w, err)
		return
	}
	writeJSON(w, s.styleView(st))
}

func (s *Server) deleteTrack(w http.ResponseWriter, r *http.Request) {
	if s.styleBusy(r.PathValue("id")) {
		http.Error(w, "the style is training: stop it first", http.StatusConflict)
		return
	}
	st, err := s.styleLib().DeleteTrack(r.PathValue("id"), r.PathValue("track"))
	if err != nil {
		styleError(w, err)
		return
	}
	writeJSON(w, s.styleView(st))
}

// trainStyle submits the style's training job: {"steps"?, "settings"?, "fresh"?: start over instead of resuming}.
func (s *Server) trainStyle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Steps    int              `json:"steps"`
		Settings *styles.Settings `json:"settings"`
		Fresh    bool             `json:"fresh"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if s.styleBusy(id) {
		http.Error(w, "the style is already training", http.StatusConflict)
		return
	}
	lib := s.styleLib()
	st, err := lib.Get(id)
	if err != nil {
		styleError(w, err)
		return
	}
	if st.Community() {
		http.Error(w, styles.ErrCommunity.Error(), http.StatusBadRequest)
		return
	}
	if len(st.Tracks) == 0 {
		http.Error(w, "add tracks first", http.StatusBadRequest)
		return
	}
	if body.Settings != nil || body.Steps > 0 {
		set := st.Settings
		if body.Settings != nil {
			set = *body.Settings
		}
		if body.Steps > 0 {
			set.Steps = body.Steps
		}
		if st, err = lib.Update(id, styles.Patch{Settings: &set}); err != nil {
			styleError(w, err)
			return
		}
	}
	if body.Fresh {
		if st, err = lib.Reset(id); err != nil {
			styleError(w, err)
			return
		}
	}
	j, err := s.Jobs.Submit(engine.Request{Model: st.BaseModel, Task: engine.TrainStyle, Style: st.ID, Title: "Style: " + st.Name})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	st, err = lib.Mutate(id, func(x *styles.Style) error {
		x.Job = j.ID // its state is the style's (styleView)
		return nil
	})
	if err != nil {
		styleError(w, err)
		return
	}
	writeJSON(w, map[string]any{"style": s.styleView(st), "job": s.view(j)})
}

// styleAudio: a style's file the page may fetch as /files/styles/<id>/… (audio only: tracks, segments, samples).
func styleAudio(rest string) bool {
	switch strings.ToLower(filepath.Ext(rest)) {
	case ".wav", ".mp3", ".m4a", ".flac":
		return true
	}
	return false
}
