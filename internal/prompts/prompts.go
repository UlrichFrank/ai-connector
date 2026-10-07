// Package prompts is the library of system prompts: what a chat model is told before the conversation, and how a
// chat model turns a short idea into a full prompt for an image, video or song model ("enhance"). The built-in ones
// can be edited and reset to their defaults; your own (e.g. pasted from other models) can be added and deleted. They
// live in ~/.config/mlxlab/prompts.json and are shared by the web app, the TUI and the iOS app.
package prompts

import (
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Use is what a prompt is for.
type Use string

const (
	Chat         Use = "chat"          // a chat's system prompt
	EnhanceImage Use = "enhance-image" // rewrites an idea into an image prompt
	EnhanceVideo Use = "enhance-video"
	EnhanceMusic Use = "enhance-music"
	WriteSong    Use = "write-song" // a whole song (title, style, lyrics) from an idea
	CoverArt     Use = "cover-art"  // an image prompt for a song's cover
)

// Uses in the order the UIs list them.
var Uses = []Use{Chat, EnhanceImage, EnhanceVideo, EnhanceMusic, WriteSong, CoverArt}

// Prompt is one system prompt.
type Prompt struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Use      Use       `json:"use"`
	Text     string    `json:"text"`
	Source   string    `json:"source,omitempty"` // where it comes from
	Builtin  bool      `json:"builtin"`          // comes with mlxlab: can be reset, not deleted
	Default  string    `json:"default,omitempty"`
	Modified bool      `json:"modified"` // a built-in one that differs from its default
	Updated  time.Time `json:"updated,omitzero"`
}

//go:embed ltx_t2v.txt
var ltxT2V string

// builtins are the prompts mlxlab comes with.
func builtins() []Prompt {
	return []Prompt{
		{ID: "chat-assistant", Name: "Helpful assistant", Use: Chat, Text: "You are a helpful assistant."},
		{ID: "chat-concise", Name: "Concise expert", Use: Chat, Text: "You are an expert who answers precisely and briefly. " +
			"Lead with the answer, then give only the reasoning that matters. Use plain language; say when you are unsure. " +
			"Answer in the language of the question."},
		{ID: "chat-coder", Name: "Coding assistant", Use: Chat, Text: "You are a senior software engineer. Write correct, idiomatic, " +
			"minimal code that matches the surrounding style. Explain briefly what you changed and why. Point out risks and " +
			"edge cases; ask when a requirement is ambiguous."},
		{ID: "enhance-image", Name: "Image prompt (FLUX)", Use: EnhanceImage, Text: "You turn a user's short idea for an image into one " +
			"detailed prompt for a text-to-image model. Keep every element the user asked for and add nothing that contradicts it. " +
			"Describe, in this order: the main subject (appearance, pose, expression, clothing, materials), the setting and " +
			"background, composition and camera (shot type, angle, lens, depth of field), lighting (direction, quality, time of " +
			"day), color palette and mood, and the medium or style (photo, film stock, illustration, 3D render …). Quote any text " +
			"that should appear in the image exactly. Write one flowing paragraph of 60–120 words in English, concrete and " +
			"visual, without lists, labels or a preamble. Output only the prompt."},
		{ID: "enhance-video", Name: "Video prompt (LTX-2.5 text-to-video)", Use: EnhanceVideo, Text: strings.TrimSpace(ltxT2V),
			Source: "LTX-2.5's own text-to-video system prompt (Gemma 4), from ltx-2-mlx"},
		{ID: "enhance-music", Name: "Song style", Use: EnhanceMusic, Text: "You turn a user's short idea for a song into a style " +
			"description for a music generation model. Output one line of comma-separated tags, most important first: genre and " +
			"subgenre, mood, tempo in bpm, key if it matters, the lead instruments, the production style, and the vocal (gender, " +
			"timbre, delivery) or \"instrumental\". Keep everything the user asked for. 8–16 tags, English, no sentences, no " +
			"preamble. Output only the tags."},
		{ID: "cover-art", Name: "Album cover", Use: CoverArt, Text: "You are an art director designing the cover of a single. " +
			"From the song's title, style and lyrics, write one prompt for a text-to-image model that makes a striking, square " +
			"album cover. Choose one strong visual idea that captures the song's mood and imagery (not a literal illustration " +
			"of every line), a clear composition, a fitting colour palette and an artistic medium that matches the genre " +
			"(photography, illustration, collage, 3D, painting …). Put the song's title on the cover as tasteful typography, " +
			"quoted exactly, and describe its placement and style. No other text, no logos, no frames, no mockups. One " +
			"paragraph of 50–90 words in English. Output only the prompt."},
		{ID: "write-song", Name: "Song from an idea", Use: WriteSong, Text: "You are a professional songwriter. From the user's " +
			"idea, write a complete song for a music generation model. Answer in exactly this format and nothing else:\n\n" +
			"TITLE: <a short, catchy title>\nSTYLE: <8–16 comma-separated tags: genre and subgenre, mood, tempo in bpm, lead " +
			"instruments, production, and the vocal (gender, timbre) or \"instrumental\">\nLYRICS:\n<the lyrics>\n\n" +
			"Structure the lyrics with section tags on their own lines, chosen to fit the genre: [intro], [verse], " +
			"[pre-chorus], [chorus], [bridge], [breakdown], [build-up], [drop], [instrumental], [outro]. Write singable " +
			"lines with natural rhythm and rhyme, 4–8 lines per section, a chorus that repeats its hook. For electronic " +
			"genres keep the lyrics short (a hook or two) and use [build-up] and [drop]; for an instrumental write only " +
			"section tags.\n\nThe lyrics contain only section tags and the words to be sung: the model sings every other " +
			"line. Put sounds, instruments and performance directions in the section tag after a dash, a few words at " +
			"most (e.g. [drop - heavy bass, mechanical breathing], [bridge - whispered]), or in the STYLE; never write " +
			"them as lines of their own in parentheses, brackets or asterisks. Parentheses inside a sung line are " +
			"backing vocals, e.g. \"we rise (we rise)\".\n\nWrite the lyrics in the language of the idea unless it asks " +
			"for another."},
	}
}

// Library is the prompts on disk.
type Library struct {
	path string
	mu   sync.Mutex
	own  map[string]Prompt // changed built-ins and your own prompts, by id
}

// Open reads dir/prompts.json (missing: only the built-ins).
func Open(dir string) *Library {
	l := &Library{path: filepath.Join(dir, "prompts.json"), own: map[string]Prompt{}}
	if b, err := os.ReadFile(l.path); err == nil {
		var ps []Prompt
		if json.Unmarshal(b, &ps) == nil {
			for _, p := range ps {
				l.own[p.ID] = p
			}
		}
	}
	return l
}

// All lists the prompts: the built-ins (as edited), then your own, by use.
func (l *Library) All() []Prompt {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Prompt
	seen := map[string]bool{}
	for _, b := range builtins() {
		p := b
		p.Builtin, p.Default = true, b.Text
		if o, ok := l.own[b.ID]; ok {
			p.Name, p.Text, p.Updated = o.Name, o.Text, o.Updated
		}
		p.Modified = p.Text != b.Text || p.Name != b.Name
		out = append(out, p)
		seen[p.ID] = true
	}
	var mine []Prompt
	for _, p := range l.own {
		if !seen[p.ID] {
			p.Builtin, p.Default, p.Modified = false, "", false
			mine = append(mine, p)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Name < mine[j].Name })
	out = append(out, mine...)
	order := map[Use]int{}
	for i, u := range Uses {
		order[u] = i
	}
	sort.SliceStable(out, func(i, j int) bool { return order[out[i].Use] < order[out[j].Use] })
	return out
}

// Get is one prompt.
func (l *Library) Get(id string) (Prompt, bool) {
	for _, p := range l.All() {
		if p.ID == id {
			return p, true
		}
	}
	return Prompt{}, false
}

var nonID = regexp.MustCompile(`[^a-z0-9]+`)

// Save stores a prompt: a built-in one keeps its id and use; a new one (no id) gets an id from its name.
func (l *Library) Save(p Prompt) (Prompt, error) {
	p.Name, p.Text = strings.TrimSpace(p.Name), strings.TrimSpace(p.Text)
	if p.Name == "" {
		return p, errors.New("a prompt needs a name")
	}
	valid := false
	for _, u := range Uses {
		valid = valid || p.Use == u
	}
	if !valid {
		return p, errors.New("unknown use " + string(p.Use))
	}
	if p.ID == "" {
		base := "my-" + strings.Trim(nonID.ReplaceAllString(strings.ToLower(p.Name), "-"), "-")
		p.ID = base
		for i := 2; ; i++ {
			if _, taken := l.Get(p.ID); !taken {
				break
			}
			p.ID = base + "-" + itoa(i)
		}
	}
	for _, b := range builtins() {
		if b.ID == p.ID {
			p.Use, p.Source = b.Use, b.Source // a built-in stays what it is
		}
	}
	p.Updated = time.Now()
	l.mu.Lock()
	l.own[p.ID] = Prompt{ID: p.ID, Name: p.Name, Use: p.Use, Text: p.Text, Source: p.Source, Updated: p.Updated}
	err := l.write()
	l.mu.Unlock()
	if err != nil {
		return p, err
	}
	out, _ := l.Get(p.ID)
	return out, nil
}

// Reset puts a built-in prompt back to its default; Delete removes one of your own (a built-in one is reset).
func (l *Library) Reset(id string) error { return l.Delete(id) }

// Delete removes your own prompt, or resets a built-in one.
func (l *Library) Delete(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.own[id]; !ok {
		return nil
	}
	delete(l.own, id)
	return l.write()
}

func (l *Library) write() error {
	ps := make([]Prompt, 0, len(l.own))
	for _, p := range l.own {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].ID < ps[j].ID })
	b, err := json.MarshalIndent(ps, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

func itoa(i int) string { return strconv.Itoa(i) }
