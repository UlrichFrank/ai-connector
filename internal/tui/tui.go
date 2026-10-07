// Package tui is mlxlab's terminal UI: the models on the left, on the right a chat (one conversation per model), a
// prompt for images and videos, or texts for the embedder and the classifier. It is a client of the HTTP API (the
// hub, or `mlxlab serve`), so jobs started here show up in the web app and on the phone, and the other way round.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/philipparndt/ai-connector/internal/client"
	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/llm"
	"github.com/philipparndt/ai-connector/internal/models"
)

var (
	accent  = lipgloss.AdaptiveColor{Light: "#6b4dff", Dark: "#9b85ff"}
	muted   = lipgloss.AdaptiveColor{Light: "#6c6c7a", Dark: "#8a8a99"}
	okColor = lipgloss.AdaptiveColor{Light: "#1c9a5b", Dark: "#3fc684"}
	bad     = lipgloss.AdaptiveColor{Light: "#d23b3b", Dark: "#ff6b6b"}

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(accent)
	mutedStyle = lipgloss.NewStyle().Foreground(muted)
	okStyle    = lipgloss.NewStyle().Foreground(okColor)
	badStyle   = lipgloss.NewStyle().Foreground(bad)
	userStyle  = lipgloss.NewStyle().Foreground(accent).Bold(true)
	paneStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(muted).Padding(0, 1)
	focusStyle = paneStyle.BorderForeground(accent)
	selStyle   = lipgloss.NewStyle().Foreground(accent).Bold(true)
	kindStyle  = lipgloss.NewStyle().Foreground(muted).Bold(true)
)

type turn struct {
	role, content string
	job           string // assistant: the job answering ("" until it is submitted)
}

type model struct {
	c       *client.Client
	hub     bool // c talks to the hub (sign-in, a worker that comes and goes); else to `mlxlab serve`
	updates <-chan jobs.Update
	workers <-chan client.WorkerState
	streams <-chan streamMsg

	worker    client.WorkerState // hub: is the Mac connected
	streamErr error              // the job stream is down (it reconnects by itself)

	list     []*engine.Model
	loaded   map[string]string // backend → the model it has in memory
	examples map[engine.Kind][]engine.Example
	cursor   int
	focus    int // 0 list, 1 input
	input    textarea.Model
	view     viewport.Model
	w, h     int
	chats    map[string][]turn
	jobs     map[string]*jobs.Job
	showLog  bool
	flash    string
	status   string              // a hint for the footer
	example  map[engine.Kind]int // the next example to fill in per kind
	ticks    int
}

type (
	updateMsg jobs.Update
	workerMsg client.WorkerState
	streamMsg struct {
		path string
		err  error
	}
	catalogMsg struct {
		cat *client.Catalog
		err error
	}
	jobMsg       struct{ job *jobs.Job }
	errMsg       struct{ err error }
	submittedMsg struct {
		model, text string
		turn        int // chat: the assistant turn waiting for the job (-1: none)
		job         *jobs.Job
		err         error
	}
	tickMsg struct{}
)

func newModel(c *client.Client, hub bool) *model {
	ta := textarea.New()
	ta.Placeholder = "Type here — Enter sends, Alt+Enter for a new line"
	ta.ShowLineNumbers = false
	ta.SetHeight(3)
	ta.CharLimit = 0
	ta.KeyMap.InsertNewline.SetKeys("alt+enter", "ctrl+j")
	return &model{c: c, hub: hub, input: ta, view: viewport.New(80, 20), chats: map[string][]turn{},
		jobs: map[string]*jobs.Job{}, example: map[engine.Kind]int{}, examples: engine.Examples, loaded: map[string]string{}}
}

// run shows the TUI over a client (it returns when the user quits).
func run(c *client.Client, info *client.Info) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streams := make(chan streamMsg, 16)
	c.OnStream = func(path string, err error) {
		select {
		case streams <- streamMsg{path, err}:
		default:
		}
	}
	updates, err := c.Events(ctx)
	if err != nil {
		return explain(c.Server, err)
	}
	mo := newModel(c, info.Hub())
	mo.updates, mo.streams = updates, streams
	if mo.hub {
		if info.Worker != nil {
			mo.worker = *info.Worker
		}
		if ws, err := c.WorkerEvents(ctx); err == nil {
			mo.workers = ws
		}
	}
	if cat, err := c.Models(ctx); err == nil {
		mo.setCatalog(cat)
	} else if !errors.Is(err, client.ErrOffline) {
		mo.flash = err.Error()
	}
	_, err = tea.NewProgram(mo, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

// setCatalog takes the models from the server, staying on the model that was selected.
func (mo *model) setCatalog(cat *client.Catalog) {
	cur := ""
	if md := mo.cur(); md != nil {
		cur = md.ID
	}
	kinds := cat.Kinds
	if len(kinds) == 0 {
		kinds = engine.Kinds
	}
	mo.list = mo.list[:0]
	for _, k := range kinds {
		for _, md := range cat.Models {
			if md.Kind == k {
				mo.list = append(mo.list, md)
			}
		}
	}
	mo.loaded = cat.Loaded
	if cat.Examples != nil {
		mo.examples = cat.Examples
	}
	mo.cursor = 0
	for i, md := range mo.list {
		if md.ID == cur {
			mo.cursor = i
			break
		}
		// the first time: a chat model that is on the Mac
		if cur == "" && md.Kind == engine.Chat && md.Here {
			mo.cursor = i
			break
		}
	}
	mo.refresh()
}

func waitFor[T any](ch <-chan T, wrap func(T) tea.Msg) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		v, ok := <-ch
		if !ok {
			return nil
		}
		return wrap(v)
	}
}

func (mo *model) waitUpdate() tea.Cmd {
	return waitFor(mo.updates, func(u jobs.Update) tea.Msg { return updateMsg(u) })
}

func (mo *model) waitWorker() tea.Cmd {
	return waitFor(mo.workers, func(s client.WorkerState) tea.Msg { return workerMsg(s) })
}

func (mo *model) waitStream() tea.Cmd {
	return waitFor(mo.streams, func(s streamMsg) tea.Msg { return s })
}

// call runs a request off the UI loop; a failure shows in the footer.
func (mo *model) call(f func(ctx context.Context) error) tea.Cmd {
	if mo.c == nil {
		return nil
	}
	return func() tea.Msg {
		if err := f(context.Background()); err != nil {
			return errMsg{err}
		}
		return nil
	}
}

func (mo *model) fetchModels() tea.Cmd {
	if mo.c == nil {
		return nil
	}
	c := mo.c
	return func() tea.Msg {
		cat, err := c.Models(context.Background())
		return catalogMsg{cat, err}
	}
}

func (mo *model) fetchJob(id string) tea.Cmd {
	if mo.c == nil {
		return nil
	}
	c := mo.c
	return func() tea.Msg {
		j, err := c.Job(context.Background(), id)
		if err != nil {
			return nil // gone already, or the next full update brings it
		}
		return jobMsg{j}
	}
}

func tick() tea.Cmd { return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} }) }

func (mo *model) Init() tea.Cmd {
	return tea.Batch(mo.waitUpdate(), mo.waitWorker(), mo.waitStream(), tick(), textarea.Blink)
}

func (mo *model) cur() *engine.Model {
	if mo.cursor < 0 || mo.cursor >= len(mo.list) {
		return nil
	}
	return mo.list[mo.cursor]
}

// apply takes an update of the job stream: a job's new state, one of its events, or (neither) its deletion.
func (mo *model) apply(u jobs.Update) tea.Cmd {
	var cmd tea.Cmd
	switch {
	case u.Job != nil:
		prev := mo.jobs[u.JobID]
		mo.jobs[u.JobID] = u.Job
		// a job starting or ending changes what is in memory
		if prev != nil && prev.State != u.Job.State {
			cmd = mo.fetchModels()
		}
	case u.Event != nil:
		if j := mo.jobs[u.JobID]; j != nil {
			client.Apply(j, *u.Event)
		} else {
			cmd = mo.fetchJob(u.JobID)
		}
	default:
		delete(mo.jobs, u.JobID)
	}
	mo.refresh()
	return cmd
}

func (mo *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		mo.w, mo.h = msg.Width, msg.Height
		mo.layout()
	case updateMsg:
		cmds = append(cmds, mo.apply(jobs.Update(msg)), mo.waitUpdate())
	case workerMsg:
		was := mo.worker.Connected
		mo.worker = client.WorkerState(msg)
		if mo.worker.Connected && !was {
			cmds = append(cmds, mo.fetchModels()) // the Mac is back: its models (maybe a new version)
		}
		cmds = append(cmds, mo.waitWorker())
	case streamMsg:
		if msg.path == "/api/events" {
			mo.streamErr = msg.err
		}
		cmds = append(cmds, mo.waitStream())
	case catalogMsg:
		if msg.err == nil {
			mo.setCatalog(msg.cat)
		}
	case jobMsg:
		if _, ok := mo.jobs[msg.job.ID]; !ok {
			mo.jobs[msg.job.ID] = msg.job
			mo.refresh()
		}
	case submittedMsg:
		mo.submitted(msg)
	case errMsg:
		mo.flash = describe(msg.err)
	case tickMsg:
		mo.ticks++
		// what is in memory changes without a job (an unload, the idle timeout): look now and then
		if mo.ticks%15 == 0 || (len(mo.list) == 0 && mo.ticks%3 == 0) {
			cmds = append(cmds, mo.fetchModels())
		}
		mo.refresh()
		cmds = append(cmds, tick())
	case tea.KeyMsg:
		if cmd, done := mo.key(msg); done {
			return mo, cmd
		}
	case tea.MouseMsg:
		var cmd tea.Cmd
		mo.view, cmd = mo.view.Update(msg)
		return mo, cmd
	}
	if mo.focus == 1 {
		var cmd tea.Cmd
		mo.input, cmd = mo.input.Update(msg)
		cmds = append(cmds, cmd)
	}
	return mo, tea.Batch(cmds...)
}

// describe is an error for the footer, the two the hub sends in words.
func describe(err error) string {
	switch {
	case errors.Is(err, client.ErrOffline):
		return "the Mac is offline"
	case errors.Is(err, client.ErrSignIn):
		return "signed out: quit and start mlxlab tui again to sign in"
	}
	return err.Error()
}

// key handles the keys of the app; done: the key is used up.
func (mo *model) key(k tea.KeyMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "ctrl+c":
		return tea.Quit, true
	case "tab":
		mo.setFocus(1 - mo.focus)
		return nil, true
	case "pgup", "pgdown":
		var cmd tea.Cmd
		mo.view, cmd = mo.view.Update(k)
		return cmd, true
	}
	if mo.focus == 1 {
		switch k.String() {
		case "esc":
			mo.setFocus(0)
			return nil, true
		case "enter":
			return mo.send(), true
		case "ctrl+x":
			return mo.cancelRunning(), true
		case "ctrl+e":
			mo.nextExample()
			return nil, true
		}
		return nil, false
	}
	switch k.String() {
	case "q":
		return tea.Quit, true
	case "up", "k":
		if mo.cursor > 0 {
			mo.cursor--
		}
		mo.refresh()
	case "down", "j":
		if mo.cursor < len(mo.list)-1 {
			mo.cursor++
		}
		mo.refresh()
	case "enter", "right", "l", "i":
		mo.setFocus(1)
	case "u":
		if mo.c == nil {
			return nil, true
		}
		mo.flash = "unloading every model …"
		c := mo.c
		return tea.Batch(mo.call(func(ctx context.Context) error { return c.Unload(ctx) }),
			tea.Tick(2*time.Second, func(time.Time) tea.Msg { return mo.fetchModels()() })), true
	case "w":
		if mo.c != nil {
			_ = exec.Command("open", mo.c.Server).Start()
		}
	case "o":
		mo.openLast()
	case "L":
		mo.showLog = !mo.showLog
		mo.refresh()
	case "c":
		if md := mo.cur(); md != nil {
			delete(mo.chats, md.ID)
			mo.refresh()
		}
	case "x":
		return mo.cancelRunning(), true
	case "e":
		mo.setFocus(1)
		mo.nextExample()
	}
	return nil, true
}

// nextExample fills the input with the next example for the current model's kind.
func (mo *model) nextExample() {
	md := mo.cur()
	if md == nil {
		return
	}
	ex := mo.examples[md.Kind]
	if len(ex) == 0 {
		return
	}
	i := mo.example[md.Kind] % len(ex)
	mo.example[md.Kind] = i + 1
	mo.input.SetValue(exampleText(md.Kind, ex[i]))
	mo.flash = ""
	mo.status = fmt.Sprintf("example %d/%d: %s", i+1, len(ex), ex[i].Title)
}

// exampleText is an example as it would be typed (the parsers read it back).
func exampleText(kind engine.Kind, e engine.Example) string {
	var text string
	switch kind {
	case engine.Chat:
		text = e.Prompt
	case engine.Image, engine.Video:
		text = e.Prompt
		if e.Size != "" {
			text += " --size " + e.Size
		}
		if e.Frames > 0 {
			text += " --frames " + strconv.Itoa(e.Frames)
		}
		for i, c := range e.Script {
			text += "\n"
			if i > 0 && c.Continue {
				text += "+ "
			}
			text += c.Prompt
		}
	case engine.Embed:
		text = strings.Join(e.Texts, "\n")
	case engine.Music:
		text = e.Prompt
		if e.Duration > 0 {
			text += fmt.Sprintf(" --duration %.0f", e.Duration)
		}
		if e.Lyrics == "" {
			text += " --instrumental"
		} else {
			text += "\n" + e.Lyrics
		}
	case engine.Classify:
		ls := []string{e.Question}
		for _, o := range e.Options {
			l := "- " + o.Label
			if o.Description != "" {
				l += ": " + o.Description
			}
			ls = append(ls, l)
		}
		text = strings.Join(append(ls, e.Texts...), "\n")
	}
	return text
}

func (mo *model) setFocus(f int) {
	mo.focus = f
	if f == 1 {
		mo.input.Focus()
	} else {
		mo.input.Blur()
	}
}

func (mo *model) cancelRunning() tea.Cmd {
	if mo.c == nil {
		return nil
	}
	c := mo.c
	var cmds []tea.Cmd
	for _, j := range mo.jobs {
		if j.State == jobs.Running {
			id := j.ID
			cmds = append(cmds, mo.call(func(ctx context.Context) error { return c.Cancel(ctx, id) }))
		}
	}
	return tea.Batch(cmds...)
}

// openLast opens the newest file of the current model in the browser (it streams from the Mac, through the hub).
func (mo *model) openLast() {
	j := mo.lastJob()
	if j == nil || mo.c == nil {
		return
	}
	f := j.Preview
	if len(j.Files) > 0 {
		f = j.Files[len(j.Files)-1]
	}
	if f == "" {
		return
	}
	_ = exec.Command("open", mo.c.FileURL(f)).Start()
}

func (mo *model) lastJob() *jobs.Job {
	md := mo.cur()
	if md == nil {
		return nil
	}
	var last *jobs.Job
	for _, j := range mo.jobs {
		if j.Model == md.ID && (last == nil || j.Created.After(last.Created)) {
			last = j
		}
	}
	return last
}

// send submits what is in the input for the current model (the request runs off the UI loop; submitted takes
// the answer).
func (mo *model) send() tea.Cmd {
	md := mo.cur()
	text := strings.TrimSpace(mo.input.Value())
	if md == nil || text == "" {
		return nil
	}
	r := engine.Request{Model: md.ID}
	waiting := -1
	switch md.Kind {
	case engine.Chat:
		mo.chats[md.ID] = append(mo.chats[md.ID], turn{role: "user", content: text})
		r.Messages = mo.history(md.ID)
		r.Temperature, r.MaxTokens = md.Defaults.Temperature, 2048
		// the answer's place, so turns sent in quick succession keep their order
		waiting = len(mo.chats[md.ID])
		mo.chats[md.ID] = append(mo.chats[md.ID], turn{role: "assistant"})
	case engine.Image, engine.Video:
		var err error
		if md.Kind == engine.Video && strings.Contains(text, "\n") {
			r, err = parseMovie(md, text)
		} else {
			r, err = parsePrompt(md, text)
		}
		if err != nil {
			mo.flash = err.Error()
			return nil
		}
	case engine.Music:
		var err error
		if r, err = parseMusic(md, text); err != nil {
			mo.flash = err.Error()
			return nil
		}
	case engine.Embed:
		r.Texts = lines(text)
	case engine.Classify:
		r.Instructions, r.Options, r.States = parseQuestion(text)
	}
	mo.flash, mo.status = "", "sending …"
	mo.refresh()
	mo.view.GotoBottom()
	if mo.c == nil {
		return nil
	}
	c, id := mo.c, md.ID
	return func() tea.Msg {
		ctx := context.Background()
		// reference images are files here: the Mac gets them as uploads first
		for i, p := range r.Images {
			u, err := c.Upload(ctx, p)
			if err != nil {
				return submittedMsg{model: id, turn: waiting, err: fmt.Errorf("%s: %w", filepath.Base(p), err)}
			}
			r.Images[i] = u
		}
		j, err := c.Submit(ctx, r)
		return submittedMsg{model: id, text: text, turn: waiting, job: j, err: err}
	}
}

func (mo *model) submitted(s submittedMsg) {
	mo.status = ""
	ts := mo.chats[s.model]
	if s.err != nil {
		mo.flash = describe(s.err)
		if s.turn >= 0 && s.turn < len(ts) {
			ts[s.turn].content = "not sent: " + mo.flash
		}
		mo.refresh()
		return
	}
	if _, ok := mo.jobs[s.job.ID]; !ok { // the stream may have told of it already, and newer
		mo.jobs[s.job.ID] = s.job
	}
	if s.turn >= 0 && s.turn < len(ts) {
		ts[s.turn].job = s.job.ID
	}
	// texts to compare or classify stay, to change and run again
	if strings.TrimSpace(mo.input.Value()) == s.text && s.job.Kind != engine.Embed && s.job.Kind != engine.Classify {
		mo.input.Reset()
	}
	mo.refresh()
	mo.view.GotoBottom()
}

func (mo *model) history(id string) []llm.Message {
	var out []llm.Message
	for _, t := range mo.chats[id] {
		c := t.content
		if t.role == "assistant" {
			j := mo.jobs[t.job]
			if j == nil {
				continue // not sent (yet)
			}
			c = j.Text
			if i := strings.Index(c, "</think>"); i >= 0 {
				c = strings.TrimSpace(c[i+len("</think>"):])
			}
			if c == "" {
				continue
			}
		}
		out = append(out, llm.Message{Role: t.role, Content: c})
	}
	return out
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// parseQuestion reads "question\n- option: description\n…\ntext\n…".
func parseQuestion(s string) (string, []engine.Option, []string) {
	ls := lines(s)
	if len(ls) == 0 {
		return "", nil, nil
	}
	var opts []engine.Option
	var texts []string
	for _, l := range ls[1:] {
		if o, ok := strings.CutPrefix(l, "- "); ok {
			label, desc, _ := strings.Cut(o, ":")
			opts = append(opts, engine.Option{Label: strings.TrimSpace(label), Description: strings.TrimSpace(desc)})
		} else {
			texts = append(texts, l)
		}
	}
	return ls[0], opts, texts
}

// parseMovie reads a movie script: the first line is what every clip shares (characters, look; options at its
// end as for a video), every further line one clip — "+ …" continues from the last frame, anything else is a new
// scene; "--frames N" at a clip's end sets its length.
func parseMovie(md *engine.Model, s string) (engine.Request, error) {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	r, err := parsePrompt(md, ls[0])
	if err != nil {
		return r, err
	}
	for _, l := range ls[1:] {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		c := engine.Clip{}
		if rest, ok := strings.CutPrefix(l, "+"); ok {
			c.Continue, l = len(r.Script) > 0, strings.TrimSpace(rest)
		}
		if i := strings.LastIndex(l, "--frames "); i >= 0 {
			n, err := strconv.Atoi(strings.TrimSpace(l[i+len("--frames "):]))
			if err != nil {
				return r, fmt.Errorf("clip %d: --frames N", len(r.Script)+1)
			}
			c.Frames, l = n, strings.TrimSpace(l[:i])
		}
		c.Prompt = l
		r.Script = append(r.Script, c)
	}
	if len(r.Script) == 0 {
		return r, fmt.Errorf("a movie needs clips: one per line after the first")
	}
	return r, nil
}

// parseMusic reads a song: the first line is the style (options at its end: --duration 60 --seed 1 --steps 8
// --instrumental), the lines after it the lyrics.
func parseMusic(md *engine.Model, s string) (engine.Request, error) {
	first, lyrics, _ := strings.Cut(strings.TrimSpace(s), "\n")
	r := engine.Request{Model: md.ID, Lyrics: strings.TrimSpace(lyrics)}
	words := strings.Fields(first)
	var style []string
	for i := 0; i < len(words); i++ {
		name, ok := strings.CutPrefix(words[i], "--")
		if !ok {
			style = append(style, words[i])
			continue
		}
		if name == "instrumental" {
			r.Instrumental = true
			continue
		}
		if i+1 >= len(words) {
			return r, fmt.Errorf("--%s needs a value", name)
		}
		n, err := strconv.Atoi(words[i+1])
		i++
		if err != nil {
			return r, fmt.Errorf("--%s: a number", name)
		}
		switch name {
		case "duration", "d":
			r.Duration = float64(n)
		case "seed":
			r.Seed = int64(n)
		case "steps":
			r.Steps = n
		default:
			return r, fmt.Errorf("unknown option --%s", name)
		}
	}
	r.Prompt = strings.Join(style, " ")
	return r, nil
}

// parsePrompt reads a prompt with options at the end: "a cat --size 768x768 --steps 8 --seed 3 --frames 49 --q 4".
func parsePrompt(md *engine.Model, s string) (engine.Request, error) {
	r := engine.Request{Model: md.ID}
	words := strings.Fields(s)
	var prompt []string
	for i := 0; i < len(words); i++ {
		w := words[i]
		if !strings.HasPrefix(w, "--") {
			prompt = append(prompt, w)
			continue
		}
		name := strings.TrimPrefix(w, "--")
		flag := func() bool { return name == "lowram" || name == "noaudio" }
		if flag() {
			r.LowRAM = r.LowRAM || name == "lowram"
			r.NoAudio = r.NoAudio || name == "noaudio"
			continue
		}
		if i+1 >= len(words) {
			return r, fmt.Errorf("--%s needs a value", name)
		}
		v := words[i+1]
		i++
		n, err := strconv.Atoi(v)
		switch name {
		case "size":
			a, b, ok := strings.Cut(v, "x")
			wv, e1 := strconv.Atoi(a)
			hv, e2 := strconv.Atoi(b)
			if !ok || e1 != nil || e2 != nil {
				return r, fmt.Errorf("--size WxH")
			}
			r.Width, r.Height = wv, hv
			continue
		case "image", "ref":
			p, err := filepath.Abs(v)
			if err != nil {
				return r, err
			}
			if _, err := os.Stat(p); err != nil {
				return r, fmt.Errorf("--%s: %w", name, err)
			}
			r.Images = append(r.Images, p)
			continue
		}
		if err != nil {
			return r, fmt.Errorf("--%s: a number", name)
		}
		switch name {
		case "w", "width":
			r.Width = n
		case "h", "height":
			r.Height = n
		case "steps":
			r.Steps = n
		case "seed":
			r.Seed = int64(n)
		case "frames", "f":
			r.Frames = n
		case "fps":
			r.FPS = n
		case "q", "quantize":
			r.Quantize = n
		case "guidance", "g":
			r.Guidance = float64(n)
		default:
			return r, fmt.Errorf("unknown option --%s", name)
		}
	}
	r.Prompt = strings.Join(prompt, " ")
	if md.Kind == engine.Video && r.Frames == 0 {
		r.Frames = md.Defaults.Frames
	}
	return r, nil
}

// ---------- layout & rendering ----------

const listWidth = 40

func (mo *model) layout() {
	rw := max(20, mo.w-listWidth-6)
	mo.input.SetWidth(rw)
	mo.view.Width = rw
	mo.view.Height = max(3, mo.h-mo.input.Height()-8)
	mo.refresh()
}

func (mo *model) refresh() {
	atBottom := mo.view.AtBottom()
	mo.view.SetContent(mo.content())
	if atBottom {
		mo.view.GotoBottom()
	}
}

func (mo *model) View() string {
	if mo.w == 0 {
		return "…"
	}
	loaded := []string{}
	for _, v := range mo.loaded {
		if v != "" {
			loaded = append(loaded, v)
		}
	}
	sort.Strings(loaded)
	head := titleStyle.Render("mlxlab") + "  " + mo.serverLine()
	if len(loaded) > 0 {
		head += mutedStyle.Render("  · in memory: " + strings.Join(loaded, ", "))
	}

	ls := paneStyle
	rs := focusStyle
	if mo.focus == 0 {
		ls, rs = focusStyle, paneStyle
	}
	bodyH := mo.h - 4
	left := ls.Width(listWidth).Height(bodyH).Render(mo.listView(bodyH))
	right := rs.Width(mo.view.Width + 2).Height(bodyH).Render(mo.rightView())
	return lipgloss.JoinVertical(lipgloss.Left, head, lipgloss.JoinHorizontal(lipgloss.Top, left, right), mo.footer())
}

// serverLine is where the TUI is connected, and (through the hub) whether the Mac is.
func (mo *model) serverLine() string {
	server := "?"
	if mo.c != nil {
		server = mo.c.Server
	}
	s := mutedStyle.Render(server)
	switch {
	case !mo.hub:
		s += mutedStyle.Render(" (mlxlab serve)")
	case mo.worker.Connected:
		name := ""
		if mo.worker.Name != "" {
			name = " " + mo.worker.Name
		}
		s += mutedStyle.Render("  · ") + okStyle.Render("● Mac online") + mutedStyle.Render(name)
	default:
		s += mutedStyle.Render("  · ") + badStyle.Render("○ Mac offline")
	}
	if mo.streamErr != nil && !errors.Is(mo.streamErr, client.ErrOffline) {
		s += mutedStyle.Render("  · ") + badStyle.Render("reconnecting …")
	}
	return s
}

func (mo *model) listView(height int) string {
	var b strings.Builder
	var kind engine.Kind
	rows := 0
	for i, md := range mo.list {
		if md.Kind != kind {
			kind = md.Kind
			if rows > 0 {
				b.WriteString("\n")
			}
			b.WriteString(kindStyle.Render(strings.ToUpper(string(kind))) + "\n")
			rows += 2
		}
		mark := mutedStyle.Render("○")
		if md.Here {
			mark = okStyle.Render("●")
		}
		for _, v := range mo.loaded {
			if strings.Contains(v, md.ID) {
				mark = selStyle.Render("◉")
			}
		}
		// the pane's inner width: listWidth minus the padding; "▸ ● name size"
		nameW := listWidth - 2 - 2 - 2 - 1 - 8
		size := models.Human(md.Size)
		if !md.Here {
			size = "↓" + size
		}
		line := fmt.Sprintf("%s %s %s", mark, lipgloss.NewStyle().Width(nameW).Render(trunc(md.Name, nameW)), mutedStyle.Render(fmt.Sprintf("%8s", size)))
		if i == mo.cursor {
			line = selStyle.Render("▸ ") + line
		} else {
			line = "  " + line
		}
		b.WriteString(line + "\n")
		rows++
	}
	_ = height
	return b.String()
}

func (mo *model) rightView() string {
	md := mo.cur()
	if md == nil {
		msg := "loading the models …"
		if mo.hub && !mo.worker.Connected {
			msg = "The Mac is offline: its models show up here when it connects to the hub."
		}
		return mutedStyle.Width(mo.view.Width).Render(msg)
	}
	head := titleStyle.Render(md.Name) + "  " + mutedStyle.Render(md.Backend)
	about := mutedStyle.Width(mo.view.Width).Render(md.About)
	return lipgloss.JoinVertical(lipgloss.Left, head, about, mo.view.View(), mo.input.View())
}

func (mo *model) footer() string {
	var run *jobs.Job
	queued := 0
	for _, j := range mo.jobs {
		switch j.State {
		case jobs.Running:
			if run == nil || shownBefore(j, run) {
				run = j
			}
		case jobs.Queued:
			queued++
		}
	}
	s := ""
	if run != nil {
		s = fmt.Sprintf("▶ %s: %s", run.Name, run.Status)
		if run.Total > 0 {
			s += " " + bar(run.Step, run.Total, 20)
		}
		s += " " + jobs.Duration(run.Seconds())
		if queued > 0 {
			s += fmt.Sprintf(" · %d queued", queued)
		}
		s += "  "
	}
	if mo.flash != "" {
		s += badStyle.Render(mo.flash) + "  "
	} else if mo.status != "" {
		s += selStyle.Render(mo.status) + "  "
	}
	keys := "↑↓ model · enter/tab type · e example · esc back · o open file · L log · c clear chat · x cancel · u unload · w web · q quit"
	if mo.focus == 1 {
		keys = "enter send · alt+enter new line · ctrl+e example · ctrl+x cancel · esc back · pgup/pgdn scroll"
	}
	return s + mutedStyle.Render(keys)
}

// shownBefore says which running job the footer shows when several run at once (a chat beside a video): the one
// with progress, then the oldest (not the map's order).
func shownBefore(a, b *jobs.Job) bool {
	if (a.Total > 0) != (b.Total > 0) {
		return a.Total > 0
	}
	if !a.Started.Equal(b.Started) {
		return a.Started.Before(b.Started)
	}
	return a.ID < b.ID
}

func bar(step, total, width int) string {
	n := 0
	if total > 0 {
		n = step * width / total
	}
	return selStyle.Render(strings.Repeat("█", n)) + mutedStyle.Render(strings.Repeat("░", width-n)) + fmt.Sprintf(" %d/%d", step, total)
}

// content is the scrollable part of the right pane.
func (mo *model) content() string {
	md := mo.cur()
	if md == nil {
		return ""
	}
	w := max(20, mo.view.Width)
	wrap := lipgloss.NewStyle().Width(w)
	var b strings.Builder
	switch md.Kind {
	case engine.Chat:
		turns := mo.chats[md.ID]
		if len(turns) == 0 {
			b.WriteString(mutedStyle.Render(hereLine(md)) + "\n\n" + mutedStyle.Render("Ask something. Each model keeps its own conversation; switch models to compare."))
		}
		for _, t := range turns {
			if t.role == "user" {
				b.WriteString(userStyle.Render("you") + "\n" + wrap.Render(t.content) + "\n\n")
				continue
			}
			j := mo.jobs[t.job]
			b.WriteString(selStyle.Render(md.ID) + "\n")
			switch {
			case j == nil && t.content != "":
				b.WriteString(badStyle.Render(wrap.Render(t.content)) + "\n")
			case j == nil:
				b.WriteString(mutedStyle.Render("sending …") + "\n")
			case j.State == jobs.Failed:
				b.WriteString(badStyle.Render(wrap.Render(j.Error)) + "\n")
			case j.Text == "":
				b.WriteString(mutedStyle.Render(j.Status) + "\n")
			default:
				think, rest := splitThink(j.Text)
				if think != "" {
					b.WriteString(mutedStyle.Width(w).Render(think) + "\n")
				}
				b.WriteString(wrap.Render(rest) + "\n")
				if !j.Active() {
					b.WriteString(mutedStyle.Render(chatStats(j)) + "\n")
				}
			}
			b.WriteString("\n")
		}
	default:
		b.WriteString(mutedStyle.Render(hereLine(md)) + "\n")
		b.WriteString(mutedStyle.Render(usage(md)) + "\n\n")
		var js []*jobs.Job
		for _, j := range mo.jobs {
			if j.Model == md.ID {
				js = append(js, j)
			}
		}
		sort.Slice(js, func(a, c int) bool { return js[a].Created.After(js[c].Created) })
		for i, j := range js {
			if i >= 8 {
				break
			}
			b.WriteString(mo.jobView(j, w) + "\n")
		}
	}
	return b.String()
}

func hereLine(md *engine.Model) string {
	if md.Here {
		return "on the Mac: " + md.Where
	}
	s := "downloads " + models.Human(md.Size) + " on first use"
	if md.Gated {
		s += " (gated: export HF_TOKEN and accept the licence on huggingface.co)"
	}
	return s
}

func usage(md *engine.Model) string {
	switch md.Kind {
	case engine.Image:
		return "prompt [--size 768x768] [--steps 4] [--seed 1] [--q 4|8|-1] [--image ref.png]"
	case engine.Video:
		return "prompt [--size 704x448] [--frames 97] [--fps 24] [--seed 1] [--image first.png] [--lowram] [--noaudio]"
	case engine.Music:
		return "style [--duration 60] [--seed 1] [--instrumental], then the lyrics on the next lines (alt+enter); o plays the song"
	case engine.Embed:
		return "one text per line (alt+enter): their cosine similarities"
	case engine.Classify:
		return "first line the question, then \"- label: description\" options, then one text per line"
	}
	return ""
}

func (mo *model) jobView(j *jobs.Job, w int) string {
	r := j.Request
	var b strings.Builder
	state := string(j.State)
	switch j.State {
	case jobs.Done:
		state = okStyle.Render("done " + jobs.Duration(j.Seconds()))
	case jobs.Failed:
		state = badStyle.Render("failed")
	case jobs.Running:
		state = selStyle.Render(j.Status)
	}
	title := r.Prompt
	switch j.Kind {
	case engine.Embed:
		title = fmt.Sprintf("%d texts", len(r.Texts))
	case engine.Classify:
		title = r.Instructions
	}
	b.WriteString(fmt.Sprintf("%s  %s  %s\n", mutedStyle.Render(j.Created.Format("15:04:05")), lipgloss.NewStyle().Width(w-30).MaxHeight(2).Render(title), state))
	if j.State == jobs.Running && j.Total > 0 {
		b.WriteString("  " + bar(j.Step, j.Total, 30) + "\n")
	}
	if j.Error != "" {
		b.WriteString(badStyle.Width(w).Render("  "+j.Error) + "\n")
	}
	for _, f := range j.Files {
		b.WriteString("  " + okStyle.Render("→ ") + f + "\n")
	}
	if s := j.Stats; s != nil && j.State == jobs.Done {
		var bits []string
		for _, k := range []string{"width", "height", "steps", "frames", "duration", "seed", "peakGB"} {
			if v, ok := s[k]; ok {
				bits = append(bits, fmt.Sprintf("%s %v", k, num(v)))
			}
		}
		if len(bits) > 0 {
			b.WriteString(mutedStyle.Render("  "+strings.Join(bits, " · ")) + "\n")
		}
	}
	if res := j.Result; res != nil {
		b.WriteString(resultView(j, w))
	}
	if mo.showLog && len(j.Logs) > 0 {
		logs := j.Logs
		if len(logs) > 12 {
			logs = logs[len(logs)-12:]
		}
		b.WriteString(mutedStyle.Width(w).Render("  "+strings.Join(logs, "\n  ")) + "\n")
	}
	return b.String()
}

func num(v any) string {
	if f, ok := v.(float64); ok {
		if f == float64(int64(f)) {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	return fmt.Sprint(v)
}

func resultView(j *jobs.Job, w int) string {
	var b strings.Builder
	switch j.Kind {
	case engine.Embed:
		var res struct{ Similarity [][]float64 }
		decode(j.Result, &res)
		sim, texts := res.Similarity, j.Request.Texts
		for i, row := range sim {
			b.WriteString(fmt.Sprintf("  %-3s", fmt.Sprintf("#%d", i+1)))
			for k, v := range row {
				cell := fmt.Sprintf("%5.2f", v)
				switch {
				case i == k:
					cell = mutedStyle.Render(cell)
				case v > 0.7:
					cell = selStyle.Render(cell)
				}
				b.WriteString(" " + cell)
			}
			if i < len(texts) {
				b.WriteString("  " + mutedStyle.Render(trunc(texts[i], w-12-6*len(row))))
			}
			b.WriteString("\n")
		}
	case engine.Classify:
		var res struct {
			Answers []struct {
				Text, Choice string
				Confidence   float64
			}
		}
		decode(j.Result, &res)
		for _, r := range res.Answers {
			b.WriteString(fmt.Sprintf("  %s %s  %s\n", selStyle.Render(r.Choice), mutedStyle.Render(fmt.Sprintf("%3.0f%%", r.Confidence*100)), trunc(r.Text, w-30)))
		}
	}
	return b.String()
}

// decode reads a result map into a typed value (the map holds Go values while the job runs, JSON values after a
// restart).
func decode(m map[string]any, v any) {
	b, _ := json.Marshal(m)
	_ = json.Unmarshal(b, v)
}

func trunc(s string, n int) string {
	if n < 4 {
		return ""
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func splitThink(s string) (think, rest string) {
	i := strings.Index(s, "<think>")
	if i < 0 {
		return "", s
	}
	j := strings.Index(s, "</think>")
	if j < 0 {
		return strings.TrimSpace(s[i+7:]), ""
	}
	return strings.TrimSpace(s[i+7 : j]), strings.TrimLeft(s[j+8:], "\n ")
}

func chatStats(j *jobs.Job) string {
	s := j.Stats
	if s == nil {
		return ""
	}
	f := func(k string) float64 { v, _ := s[k].(float64); return v }
	i := func(k string) int {
		switch v := s[k].(type) {
		case int:
			return v
		case float64:
			return int(v)
		}
		return 0
	}
	out := fmt.Sprintf("%.1f tok/s · %d tokens · prompt %d", f("tokensPerSecond"), i("tokens"), i("promptTokens"))
	if c := i("cachedTokens"); c > 0 {
		out += fmt.Sprintf(" (%d cached)", c)
	}
	out += fmt.Sprintf(" · prefill %.0f tok/s · first token %.2fs", f("prefillPerSecond"), f("firstTokenSeconds"))
	if p := f("peakGB"); p > 0 {
		out += fmt.Sprintf(" · peak %.1f GB", p)
	}
	return out
}
