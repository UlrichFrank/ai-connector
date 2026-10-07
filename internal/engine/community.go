package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/styles"
)

// Community styles (docs/music.md): a style that is not trained here but is a LoRA from a Hugging Face repository,
// pinned to a commit. Adding one reads its adapter_config.json (it must be a PEFT LoRA) and, when there is one, its
// machine-readable sidecar (<name>.metadata.json, schema v1: trigger word, strength, recommended steps, guidance and
// shift, example prompts, base model); the weights are fetched through the model store (pinned, checked) by the first
// song that uses the style. The song worker merges them like a trained style's (workers/acestep_worker.py).

// CommunityStyle is a LoRA of the curated list (offered as "Add" in the Styles page), pinned.
type CommunityStyle struct {
	Repo      string `json:"repo"`
	Rev       string `json:"rev"`
	Name      string `json:"name"`
	About     string `json:"about"`
	BaseModel string `json:"baseModel"`
	Trigger   string `json:"trigger"`
	Licence   string `json:"licence"` // "" none stated
	Size      int64  `json:"size"`
}

// CommunityStyles is the curated list: LoRAs checked with mlxlab's song models.
var CommunityStyles = []CommunityStyle{
	{Repo: "ryanontheinside/deep_house-acestep1.5-v1", Rev: "746cf4500a242c741d28577b0d85fe579b830133", Name: "Deep House",
		About: "RyanOnTheInside's deep house LoRA for ACE-Step 1.5 turbo (rank 64 on q/k/v/o, 15 tracks): four on the floor, " +
			"warm sub bass, jazzy keys and organ chords, soulful vocals; 118–124 BPM", BaseModel: "ace-step-1.5-turbo",
		Trigger: "roti-d33ph0us", Size: 88130248},
}

func curated(repo string) *CommunityStyle {
	for i := range CommunityStyles {
		if strings.EqualFold(CommunityStyles[i].Repo, repo) {
			return &CommunityStyles[i]
		}
	}
	return nil
}

// CommunityModel says whether a model takes community LoRAs (PEFT LoRAs of ACE-Step 1.5's DiT).
func CommunityModel(model string) bool {
	t, ok := trainable[model]
	return ok && !t.sa3
}

// CommunityLookup is what a LoRA repository says about itself (GET /api/community-styles?repo=…).
type CommunityLookup struct {
	Repo        string             `json:"repo"`
	Rev         string             `json:"rev"` // the commit (pinned)
	URL         string             `json:"url"`
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Author      string             `json:"author,omitempty"`
	Licence     string             `json:"licence"` // "" none stated
	Gated       bool               `json:"gated,omitempty"`
	Trigger     string             `json:"trigger"`            // "" the user names it
	Triggers    []string           `json:"triggers,omitempty"` // more trigger words the metadata names
	BaseText    string             `json:"baseText,omitempty"` // the base model as the repository names it
	BaseModel   string             `json:"baseModel"`          // our model id ("" not known: the user picks)
	Strength    float64            `json:"strength"`
	Tested      []float64          `json:"tested,omitempty"` // the strengths it was tested with (min, max)
	Recommended styles.Recommended `json:"recommended"`
	Examples    []styles.Example   `json:"examples,omitempty"`
	WeightsFile string             `json:"weightsFile"`
	Size        int64              `json:"size"`
	Rank        int                `json:"rank"`
	Alpha       int                `json:"alpha"`
	Targets     []string           `json:"targets,omitempty"`
	Metadata    string             `json:"metadata,omitempty"` // the sidecar read ("" none)
	Curated     bool               `json:"curated,omitempty"`
}

// CommunityAdd is POST /api/styles with a "repo": the LoRA, and what the user names when the repository does not.
type CommunityAdd struct {
	Repo      string  `json:"repo"`
	Rev       string  `json:"rev,omitempty"`
	Name      string  `json:"name,omitempty"`
	Trigger   string  `json:"trigger,omitempty"`
	BaseModel string  `json:"baseModel,omitempty"`
	Strength  float64 `json:"strength,omitempty"`
}

var repoID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ParseRepo reads "owner/name", "owner/name@rev" or a huggingface.co link (…/tree/<rev>).
func ParseRepo(s string) (repo, rev string, err error) {
	s = strings.TrimSpace(s)
	for _, p := range []string{"https://", "http://"} {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "www."), "huggingface.co/")
	s = strings.TrimPrefix(s, "hf.co/")
	s = strings.Trim(s, "/")
	if r, v, ok := strings.Cut(s, "@"); ok {
		s, rev = r, v
	}
	parts := strings.Split(s, "/")
	if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob" || parts[2] == "resolve") {
		rev = parts[3]
	}
	if len(parts) >= 2 {
		s = parts[0] + "/" + parts[1]
	}
	if !repoID.MatchString(s) || strings.Contains(s, "..") {
		return "", "", fmt.Errorf("a Hugging Face repository like owner/name, not %q", s)
	}
	if rev != "" && !regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`).MatchString(rev) {
		return "", "", fmt.Errorf("the revision %q", rev)
	}
	return s, rev, nil
}

// loraMeta is the sidecar <name>.metadata.json (schema v1, as RyanOnTheInside's LoRAs ship it).
type loraMeta struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Author        string `json:"author"`
	Model         struct {
		Type           string `json:"type"`
		BaseModel      string `json:"base_model"`
		BaseModelScale string `json:"base_model_scale"`
	} `json:"model"`
	Inference struct {
		PrimaryTrigger string    `json:"primary_trigger_word"`
		Triggers       []string  `json:"trigger_words"`
		Strength       float64   `json:"recommended_strength"`
		Tested         []float64 `json:"tested_strength_range"`
		Steps          float64   `json:"recommended_steps"`
		Shift          float64   `json:"recommended_shift"`
		Guidance       float64   `json:"recommended_guidance"`
		Examples       []struct {
			Caption string  `json:"caption"`
			BPM     float64 `json:"bpm"`
			Key     string  `json:"key"`
		} `json:"example_prompts"`
	} `json:"inference"`
	Compatibility struct {
		BaseModelRequired string `json:"base_model_required"`
	} `json:"compatibility"`
	Files struct {
		Weights   string `json:"weights"`
		SizeBytes int64  `json:"size_bytes"`
		SHA256    string `json:"sha256"`
	} `json:"files"`
	Distribution struct {
		License *string `json:"license"`
	} `json:"distribution"`
}

// peftConfig is what an adapter_config.json must say for the worker to merge it.
type peftConfig struct {
	PeftType  string   `json:"peft_type"`
	R         int      `json:"r"`
	Alpha     float64  `json:"lora_alpha"`
	Targets   []string `json:"target_modules"`
	UseDoRA   bool     `json:"use_dora"`
	BaseModel string   `json:"base_model_name_or_path"`
}

// CommunityBase maps how a LoRA names its base model ("AceStep v1.5 Turbo (2B)", a checkpoint path like
// ".../acestep-v15-turbo") to the catalogue's id ("" when it is not an ACE-Step 1.5 DiT we have).
func CommunityBase(names ...string) string {
	for _, n := range names {
		s := strings.ToLower(n)
		s = strings.NewReplacer("ace-step", "acestep", "ace step", "acestep", "_", "-", "v1.5", "v15", "1.5", "v15").Replace(s)
		if !strings.Contains(s, "acestep") || !strings.Contains(s, "v15") {
			continue
		}
		xl := strings.Contains(s, "xl") || strings.Contains(s, "4b")
		var kind string
		switch {
		case strings.Contains(s, "turbo"):
			kind = "turbo"
		case strings.Contains(s, "base") || strings.Contains(s, "sft"):
			kind = "base"
		default:
			continue
		}
		id := "ace-step-1.5-" + kind
		if xl {
			id = "ace-step-1.5-xl-" + kind
		}
		if CommunityModel(id) {
			return id
		}
	}
	return ""
}

// LookupCommunityStyle reads a LoRA repository (spec: owner/name[@rev] or a link): its commit (the curated list's
// pin, else the one rev — default the main branch — names now), its PEFT config and sidecar metadata. Only the small
// files are fetched (into the model store, checked).
func (b *MusicBackend) LookupCommunityStyle(ctx context.Context, spec string) (CommunityLookup, error) {
	repo, rev, err := ParseRepo(spec)
	if err != nil {
		return CommunityLookup{}, err
	}
	cur := curated(repo)
	if rev == "" && cur != nil {
		rev = cur.Rev
	}
	info, err := b.Store.HubInfo(ctx, repo, rev)
	if errors.Is(err, models.ErrGated) {
		return CommunityLookup{}, GatedError(repo)
	}
	if err != nil {
		return CommunityLookup{}, err
	}
	repo = info.Repo
	l := CommunityLookup{Repo: repo, Rev: info.SHA, URL: "https://huggingface.co/" + repo, Licence: info.License, Gated: info.Gated,
		Author: info.Author, Curated: cur != nil && cur.Rev == info.SHA, Strength: 1}
	hm, err := b.Store.FromHub(ctx, repo, repo, info.SHA, nil)
	if errors.Is(err, models.ErrGated) {
		return CommunityLookup{}, GatedError(repo)
	}
	if err != nil {
		return CommunityLookup{}, err
	}
	var cfgFile, metaFile *models.File
	var weights []models.File
	for i, f := range hm.Files {
		switch {
		case f.Path == "adapter_config.json":
			cfgFile = &hm.Files[i]
		case strings.HasSuffix(f.Path, ".metadata.json") && !strings.Contains(f.Path, "/") && metaFile == nil:
			metaFile = &hm.Files[i]
		case strings.HasSuffix(f.Path, ".safetensors") && !strings.Contains(f.Path, "/"):
			weights = append(weights, f)
		}
	}
	if cfgFile == nil {
		return l, fmt.Errorf("%s has no adapter_config.json at its top: not a PEFT LoRA mlxlab can merge", repo)
	}
	small := models.Model{Name: repo, Repo: repo, Rev: info.SHA, Files: []models.File{*cfgFile}}
	if metaFile != nil {
		small.Files = append(small.Files, *metaFile)
	}
	dir, err := b.Store.Ensure(ctx, small)
	if err != nil {
		return l, err
	}
	var cfg peftConfig
	raw, err := os.ReadFile(filepath.Join(dir, "adapter_config.json"))
	if err == nil {
		err = json.Unmarshal(raw, &cfg)
	}
	if err != nil {
		return l, fmt.Errorf("%s: adapter_config.json: %w", repo, err)
	}
	switch {
	case !strings.EqualFold(cfg.PeftType, "LORA"):
		return l, fmt.Errorf("%s is a PEFT %q adapter, not a LoRA", repo, cfg.PeftType)
	case cfg.UseDoRA:
		return l, fmt.Errorf("%s is a DoRA: mlxlab merges plain LoRAs only", repo)
	case cfg.R <= 0:
		return l, fmt.Errorf("%s: adapter_config.json names no rank", repo)
	}
	l.Rank, l.Alpha, l.Targets = cfg.R, int(cfg.Alpha), cfg.Targets
	var meta loraMeta
	if metaFile != nil {
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(metaFile.Path)))
		if err == nil {
			err = json.Unmarshal(raw, &meta)
		}
		if err != nil {
			return l, fmt.Errorf("%s: %s: %w", repo, metaFile.Path, err)
		}
		l.Metadata = metaFile.Path
	}
	// the weights: the sidecar names them; else PEFT's own name; else the one safetensors file there is
	pick := func(name string) *models.File {
		for i := range weights {
			if weights[i].Path == name {
				return &weights[i]
			}
		}
		return nil
	}
	w := pick(meta.Files.Weights)
	if w == nil {
		w = pick("adapter_model.safetensors")
	}
	if w == nil && len(weights) == 1 {
		w = &weights[0]
	}
	if w == nil {
		var names []string
		for _, f := range weights {
			names = append(names, f.Path)
		}
		if len(names) == 0 {
			return l, fmt.Errorf("%s has no .safetensors weights at its top", repo)
		}
		return l, fmt.Errorf("%s has several weight files (%s) and no metadata naming one", repo, strings.Join(names, ", "))
	}
	if meta.Files.SHA256 != "" && w.SHA256 != "" && !strings.EqualFold(meta.Files.SHA256, w.SHA256) {
		return l, fmt.Errorf("%s: %s is not the file its metadata describes (SHA-256 %s, metadata %s)", repo, w.Path, w.SHA256, meta.Files.SHA256)
	}
	if w.Size > 4<<30 {
		return l, fmt.Errorf("%s: %s is %s — too big for a LoRA", repo, w.Path, models.Human(w.Size))
	}
	l.WeightsFile, l.Size = w.Path, w.Size

	l.Name = strings.TrimSpace(meta.Name)
	if l.Name == "" && cur != nil {
		l.Name = cur.Name
	}
	if l.Name == "" {
		l.Name = path.Base(repo)
	}
	l.Description = strings.TrimSpace(meta.Description)
	if meta.Author != "" {
		l.Author = meta.Author
	}
	if l.Licence == "" && meta.Distribution.License != nil {
		l.Licence = strings.TrimSpace(*meta.Distribution.License)
	}
	in := meta.Inference
	l.Trigger = strings.TrimSpace(in.PrimaryTrigger)
	for _, t := range in.Triggers {
		if t = strings.TrimSpace(t); t != "" && t != l.Trigger {
			l.Triggers = append(l.Triggers, t)
		}
	}
	if l.Trigger == "" && len(l.Triggers) > 0 {
		l.Trigger, l.Triggers = l.Triggers[0], l.Triggers[1:]
	}
	if l.Trigger == "" && cur != nil {
		l.Trigger = cur.Trigger
	}
	if in.Strength > 0 && in.Strength <= 2 {
		l.Strength = in.Strength
	}
	if len(in.Tested) == 2 {
		l.Tested = in.Tested
	}
	l.Recommended = styles.Recommended{Steps: int(in.Steps), Guidance: in.Guidance, Shift: in.Shift}
	if l.Recommended.Steps < 0 || l.Recommended.Steps > 200 {
		l.Recommended.Steps = 0
	}
	if l.Recommended.Shift < 0 || l.Recommended.Shift > 20 {
		l.Recommended.Shift = 0
	}
	if l.Recommended.Guidance < 0 || l.Recommended.Guidance > 30 {
		l.Recommended.Guidance = 0
	}
	for _, e := range in.Examples {
		if c := strings.TrimSpace(e.Caption); c != "" {
			l.Examples = append(l.Examples, styles.Example{Prompt: c, BPM: e.BPM, Key: e.Key})
		}
	}
	l.BaseText = strings.TrimSpace(meta.Compatibility.BaseModelRequired)
	if l.BaseText == "" && meta.Model.BaseModel != "" {
		l.BaseText = strings.TrimSpace(meta.Model.BaseModel + " " + meta.Model.BaseModelScale)
	}
	l.BaseModel = CommunityBase(l.BaseText, meta.Model.BaseModel+" "+meta.Model.BaseModelScale, cfg.BaseModel)
	if l.BaseModel == "" && cur != nil {
		l.BaseModel = cur.BaseModel
	}
	if l.BaseText == "" {
		l.BaseText = cfg.BaseModel
	}
	return l, nil
}

// AddCommunityStyle adds a LoRA repository as a style (its weights are fetched by the first song that uses it). The
// trigger word and base model come from the repository's metadata; the request names them when it has none.
func (b *MusicBackend) AddCommunityStyle(ctx context.Context, a CommunityAdd) (styles.Style, error) {
	spec := a.Repo
	if a.Rev != "" && !strings.Contains(spec, "@") {
		spec += "@" + a.Rev
	}
	l, err := b.LookupCommunityStyle(ctx, spec)
	if err != nil {
		return styles.Style{}, err
	}
	base := l.BaseModel
	if a.BaseModel != "" {
		if base != "" && a.BaseModel != base {
			return styles.Style{}, fmt.Errorf("%s is a LoRA of %s (%s), not of %s", l.Repo, base, l.BaseText, a.BaseModel)
		}
		base = a.BaseModel
	}
	if base == "" {
		return styles.Style{}, fmt.Errorf("which model is %s for? Its repository names %q: pick the ACE-Step 1.5 model it was trained on", l.Repo, l.BaseText)
	}
	if !CommunityModel(base) {
		var ids []string
		for id := range trainable {
			if CommunityModel(id) {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		return styles.Style{}, fmt.Errorf("community LoRAs work with %s, not %s", strings.Join(ids, ", "), base)
	}
	trigger := l.Trigger
	if strings.TrimSpace(a.Trigger) != "" {
		trigger = a.Trigger
	}
	if strings.TrimSpace(trigger) == "" {
		return styles.Style{}, fmt.Errorf("%s names no trigger word in a metadata file: give the one its model card names", l.Repo)
	}
	name := l.Name
	if strings.TrimSpace(a.Name) != "" {
		name = a.Name
	}
	strength := l.Strength
	if a.Strength > 0 {
		strength = a.Strength
	}
	var rec *styles.Recommended
	if l.Recommended != (styles.Recommended{}) {
		r := l.Recommended
		rec = &r
	}
	st := styles.Style{Name: name, BaseModel: base, Trigger: trigger, Description: l.Description, Strength: strength,
		Weights: b.Store.Path(models.Model{Repo: l.Repo, Rev: l.Rev}), WeightsFile: l.WeightsFile, Recommended: rec, Examples: l.Examples,
		Source: &styles.Source{Repo: l.Repo, Rev: l.Rev, URL: l.URL, Author: l.Author, Licence: l.Licence, BaseText: l.BaseText,
			Size: l.Size, Rank: l.Rank, Alpha: l.Alpha}}
	return b.StyleLibrary().AddCommunity(st)
}

// communityWeights fetches a community style's LoRA through the model store (pinned to its commit, checked) and
// returns its folder.
func (b *MusicBackend) communityWeights(ctx context.Context, st styles.Style, emit Emit) (string, error) {
	src := st.Source
	keep := func(p string) bool { return p == "adapter_config.json" || p == st.WeightsFile }
	hm, err := b.Store.FromHub(ctx, src.Repo, src.Repo, src.Rev, keep)
	if errors.Is(err, models.ErrGated) {
		return "", GatedError(src.Repo)
	}
	if err != nil {
		return "", err
	}
	if len(hm.Files) < 2 {
		return "", fmt.Errorf("%s@%s no longer has %s and adapter_config.json", src.Repo, src.Rev[:min(7, len(src.Rev))], st.WeightsFile)
	}
	return fetch(ctx, b.Store, hm, emit)
}
