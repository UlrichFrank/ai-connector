// Package files is the files sandbox of the chat (zone files, see docs/chat-tools.md): the model writes Python that
// makes a PDF, a chart, a spreadsheet or a document; it runs in a fresh Apple `container` VM without any network,
// with a read-only root, a tmpfs /out and CPU/memory/time limits. Only files of allowed types and sizes whose
// content matches their extension come out, into the worker's output dir.
package files

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sandboxfiles "github.com/philipparndt/ai-connector/sandbox/files"

	"github.com/philipparndt/ai-connector/internal/agent"
)

// Options configure a Sandbox. Only OutDir is required.
type Options struct {
	OutDir  string        // where the files go (the worker's output dir)
	Image   string        // image tag; default mlxlab-sandbox-files:<hash of the build context>
	Timeout time.Duration // wall-clock limit of the code; default 60 s
	CPUs    int           // default 2
	Memory  string        // default "2g" (/out is a tmpfs inside the VM: it counts against this)
	CLI     string        // the container CLI; default "container"
	// BuildLog receives the output of `container build` (when the image is missing); nil = discarded.
	BuildLog io.Writer
}

// Sandbox is the agent.Toolbox of the files sandbox. Calls run one at a time.
type Sandbox struct {
	opt   Options
	sem   chan struct{}
	build sync.Mutex
	built bool
}

var _ agent.Toolbox = (*Sandbox)(nil)

const (
	label     = "mlxlab.sandbox=files"
	namePref  = "mlxlab-files-"
	imageRepo = "mlxlab-sandbox-files"
	resultMax = 1 << 20
	// the VM boot and the copy out on top of the code's own time limit
	hostSlack = 90 * time.Second
)

// New makes a sandbox; the image is built on first use (or with EnsureImage).
func New(opt Options) (*Sandbox, error) {
	if opt.OutDir == "" {
		return nil, errors.New("files sandbox: no output dir")
	}
	abs, err := filepath.Abs(opt.OutDir)
	if err != nil {
		return nil, err
	}
	opt.OutDir = abs
	if opt.Image == "" {
		opt.Image = DefaultImage()
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 60 * time.Second
	}
	if opt.CPUs <= 0 {
		opt.CPUs = 2
	}
	if opt.Memory == "" {
		opt.Memory = "2g"
	}
	if opt.CLI == "" {
		opt.CLI = "container"
	}
	return &Sandbox{opt: opt, sem: make(chan struct{}, 1)}, nil
}

// DefaultImage is the image tag for the embedded build context: a change of the Containerfile or the scripts gives a
// new tag, so the image is rebuilt.
func DefaultImage() string {
	h := sha256.New()
	_ = fs.WalkDir(sandboxfiles.Context, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := sandboxfiles.Context.ReadFile(p)
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		h.Write(b)
		return nil
	})
	return imageRepo + ":" + hex.EncodeToString(h.Sum(nil))[:12]
}

// Image is the image tag in use.
func (s *Sandbox) Image() string { return s.opt.Image }

// EnsureImage builds the image if it is missing (minutes the first time; then it is reused).
func (s *Sandbox) EnsureImage(ctx context.Context) error {
	s.build.Lock()
	defer s.build.Unlock()
	if s.built {
		return nil
	}
	if exec.CommandContext(ctx, s.opt.CLI, "image", "inspect", s.opt.Image).Run() == nil {
		s.built = true
		return nil
	}
	dir, err := os.MkdirTemp("", "mlxlab-sandbox-files-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.CopyFS(dir, sandboxfiles.Context); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, s.opt.CLI, "build", "--progress", "plain", "-t", s.opt.Image,
		"-f", filepath.Join(dir, "Containerfile"), dir)
	var tailBuf tailWriter
	if s.opt.BuildLog != nil {
		cmd.Stdout = io.MultiWriter(s.opt.BuildLog, &tailBuf)
	} else {
		cmd.Stdout = &tailBuf
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("files sandbox: building %s: %v\n%s", s.opt.Image, err, tailBuf.String())
	}
	s.built = true
	s.pruneOld(ctx)
	return nil
}

// pruneOld deletes earlier builds of the sandbox image (other tags of mlxlab-sandbox-files) — only those.
func (s *Sandbox) pruneOld(ctx context.Context) {
	repo, _, ok := strings.Cut(s.opt.Image, ":")
	if !ok || repo != imageRepo {
		return
	}
	out, err := exec.CommandContext(ctx, s.opt.CLI, "image", "ls", "--format", "json").Output()
	if err != nil {
		return
	}
	var imgs []struct {
		Configuration struct {
			Name string `json:"name"`
		} `json:"configuration"`
	}
	if json.Unmarshal(out, &imgs) != nil {
		return
	}
	for _, im := range imgs {
		n := im.Configuration.Name
		if strings.HasPrefix(n, imageRepo+":") && n != s.opt.Image {
			_ = exec.CommandContext(ctx, s.opt.CLI, "image", "delete", n).Run()
		}
	}
}

// ---------- the tools ----------

const makeFileDesc = `Create a file — PDF report, chart image, Excel spreadsheet, Word document, CSV, SVG, Markdown, HTML — by running Python 3 code in an offline sandbox. The user gets the file(s) as a download.

How it runs: a fresh Linux container with NO network and no access to the user's files; nothing can be installed. Put all data into the code. Write every output file into /out — the requested file name is in the environment variable OUT_FILE (= /out/<filename>), e.g. plt.savefig(os.environ["OUT_FILE"]). Time limit 60 s.

Libraries: reportlab (PDF: platypus, tables, charts), matplotlib (charts; the Agg backend is preset), numpy, pandas, openpyxl and xlsxwriter (xlsx), python-docx (docx), markdown and weasyprint (HTML → PDF: weasyprint.HTML(string=html).write_pdf(path)), Pillow, pypdf.
Fonts: "DejaVu Sans" and "Noto Sans" cover umlauts and most scripts. In reportlab the built-in Helvetica cannot show all characters — register a TTF:
  from reportlab.pdfbase import pdfmetrics; from reportlab.pdfbase.ttfonts import TTFont
  pdfmetrics.registerFont(TTFont("DejaVu", "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"))
  pdfmetrics.registerFont(TTFont("DejaVu-Bold", "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf"))
To put a chart into a PDF, save it first (e.g. /tmp/chart.png) and embed it; only files in /out are delivered.

Delivered: .pdf .png .svg .csv .xlsx .docx .md .txt .html (no scripts in svg/html), at most 10 files, 25 MB each. If the code fails you get the error and the traceback: fix the code and call make_file again. You do not need to print or describe the file's content afterwards; just tell the user what you made. For a plain text document, markdown_to_pdf is simpler.`

const markdownDesc = `Turn Markdown into a nicely typeset PDF (A4, headings, lists, tables, block quotes, code blocks with syntax highlighting, page numbers) — no code needed. Use it for reports, summaries, letters, notes, documentation. Supports GitHub-style tables, fenced code blocks, footnotes and Unicode (umlauts, emoji). No images from the internet (the sandbox is offline). For charts or custom layouts use make_file.`

var makeFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "filename": {"type": "string", "description": "Name of the main output file with extension, e.g. \"report.pdf\", \"sales.xlsx\", \"chart.png\". The code should write it to os.environ[\"OUT_FILE\"]."},
    "code": {"type": "string", "description": "Complete Python 3 program that writes the file(s) into /out."},
    "language": {"type": "string", "enum": ["python"], "description": "Always \"python\"."}
  },
  "required": ["filename", "code"]
}`)

var markdownSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "filename": {"type": "string", "description": "Name of the PDF, e.g. \"summary.pdf\"."},
    "markdown": {"type": "string", "description": "The document in Markdown."},
    "title": {"type": "string", "description": "Optional title, shown as a large heading on top and as the PDF title. Leave out if the Markdown starts with its own # heading."}
  },
  "required": ["filename", "markdown"]
}`)

func (s *Sandbox) Tools(context.Context) ([]agent.Tool, error) {
	return []agent.Tool{
		{Name: "make_file", Description: makeFileDesc, Parameters: makeFileSchema, Zone: agent.ZoneFiles},
		{Name: "markdown_to_pdf", Description: markdownDesc, Parameters: markdownSchema, Zone: agent.ZoneFiles},
	}, nil
}

func (s *Sandbox) Close() error { return nil }

// request is what the in-container runner reads from stdin (sandbox/files/runner.py).
type request struct {
	Mode     string   `json:"mode"` // python | markdown
	Code     string   `json:"code,omitempty"`
	Markdown string   `json:"markdown,omitempty"`
	Title    string   `json:"title,omitempty"`
	Filename string   `json:"filename"`
	Timeout  float64  `json:"timeout"`
	Allowed  []string `json:"allowed"`
	MaxBytes int      `json:"max_bytes"`
	MaxFiles int      `json:"max_files"`
}

// runResult is the runner's result.json.
type runResult struct {
	Exit     int     `json:"exit"`
	TimedOut bool    `json:"timedOut"`
	Seconds  float64 `json:"seconds"`
	Log      string  `json:"log"`
	Files    []struct {
		Name  string `json:"name"`
		Size  int64  `json:"size"`
		Pages *int   `json:"pages"`
	} `json:"files"`
	Skipped []struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	} `json:"skipped"`
}

// made is one delivered file.
type made struct {
	Path  string
	Size  int64
	Pages int
}

func (s *Sandbox) Call(ctx context.Context, name string, args json.RawMessage) (agent.Result, error) {
	var a struct {
		Filename string `json:"filename"`
		Code     string `json:"code"`
		Language string `json:"language"`
		Markdown string `json:"markdown"`
		Title    string `json:"title"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return errResult("invalid arguments: %v", err), nil
	}
	req := request{Allowed: Allowed, MaxBytes: MaxFileBytes, MaxFiles: MaxFiles, Timeout: s.opt.Timeout.Seconds()}
	switch name {
	case "make_file":
		if a.Language != "" && !strings.EqualFold(a.Language, "python") {
			return errResult("only language \"python\" is supported"), nil
		}
		if strings.TrimSpace(a.Code) == "" {
			return errResult("code is empty"), nil
		}
		req.Mode, req.Code = "python", a.Code
	case "markdown_to_pdf":
		if strings.TrimSpace(a.Markdown) == "" {
			return errResult("markdown is empty"), nil
		}
		if Ext(a.Filename) != "pdf" {
			a.Filename = strings.TrimSuffix(a.Filename, path.Ext(a.Filename)) + ".pdf"
		}
		req.Mode, req.Markdown, req.Title = "markdown", a.Markdown, a.Title
	default:
		return agent.Result{}, fmt.Errorf("files sandbox: unknown tool %q", name)
	}
	fn, err := SanitizeName(a.Filename)
	if err != nil {
		return errResult("filename: %v", err), nil
	}
	req.Filename = fn

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return agent.Result{}, ctx.Err()
	}
	if err := s.EnsureImage(ctx); err != nil {
		return agent.Result{}, err
	}
	res, files, rejected, err := s.run(ctx, req)
	if err != nil {
		return agent.Result{}, err
	}
	return report(name, req.Filename, s.opt.Timeout, s.opt.Memory, res, files, rejected), nil
}

// run starts one container, feeds it the request and takes the files out of its tar stream.
func (s *Sandbox) run(ctx context.Context, req request) (*runResult, []made, []string, error) {
	if err := os.MkdirAll(s.opt.OutDir, 0o755); err != nil {
		return nil, nil, nil, err
	}
	in, err := json.Marshal(req)
	if err != nil {
		return nil, nil, nil, err
	}
	name := namePref + randHex(6)
	ctx, cancel := context.WithTimeout(ctx, s.opt.Timeout+hostSlack)
	defer cancel()
	defer s.remove(name)

	cmd := exec.CommandContext(ctx, s.opt.CLI, s.runArgs(name)...)
	cmd.Stdin = bytes.NewReader(in)
	var stderr tailWriter
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("files sandbox: %v", err)
	}
	// never read more than the files could be (plus tar headers); then stop
	limit := int64(MaxFiles)*(MaxFileBytes+64<<10) + resultMax
	res, files, rejected, perr := s.readTar(io.LimitReader(stdout, limit))
	_, _ = io.Copy(io.Discard, io.LimitReader(stdout, limit)) // drain so the CLI can exit
	werr := cmd.Wait()
	if ctx.Err() != nil {
		removeAll(files)
		return nil, nil, nil, fmt.Errorf("files sandbox: the container did not finish in time (%v)", s.opt.Timeout+hostSlack)
	}
	if res == nil {
		removeAll(files)
		msg := strings.TrimSpace(stderr.String())
		if perr != nil {
			msg = perr.Error() + "; " + msg
		}
		if werr != nil {
			msg = werr.Error() + "; " + msg
		}
		// the run broke down (the runner was killed, e.g. by running out of memory): tell the model
		return &runResult{Exit: -1, Log: "the sandbox stopped without a result (out of memory or killed?): " + msg}, nil, nil, nil
	}
	if perr != nil {
		rejected = append(rejected, "(output stream: "+perr.Error()+")")
	}
	return res, files, rejected, nil
}

func (s *Sandbox) runArgs(name string) []string {
	return []string{"run", "--rm", "-i",
		"--name", name,
		"--label", label,
		"--progress", "none",
		"--network", "none", // only a loopback interface in the VM
		"--no-dns",
		"--read-only",
		"--tmpfs", "/out",
		"--tmpfs", "/tmp",
		"--cpus", fmt.Sprint(s.opt.CPUs),
		"--memory", s.opt.Memory,
		"--cap-drop", "ALL",
		"--user", "1000:1000",
		s.opt.Image,
	}
}

// remove deletes our container if it is still there (after a timeout or a crash); --rm handles the normal case.
func (s *Sandbox) remove(name string) {
	if !strings.HasPrefix(name, namePref) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, s.opt.CLI, "delete", "--force", name).Run()
}

// readTar reads result.json and the files; every file is checked (name, type, size, magic bytes) and written to the
// output dir under a new, unique name.
func (s *Sandbox) readTar(r io.Reader) (*runResult, []made, []string, error) {
	var (
		res      *runResult
		files    []made
		rejected []string
		names    = map[string]bool{}
	)
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return res, files, rejected, nil
		}
		if err != nil {
			return res, files, rejected, err
		}
		switch {
		case h.Name == "result.json" && res == nil:
			if h.Size > resultMax {
				return nil, files, rejected, errors.New("result too large")
			}
			var rr runResult
			if err := json.NewDecoder(io.LimitReader(tr, resultMax)).Decode(&rr); err != nil {
				return nil, files, rejected, fmt.Errorf("result: %v", err)
			}
			res = &rr
		case strings.HasPrefix(h.Name, "out/"):
			orig := strings.TrimPrefix(h.Name, "out/")
			if h.Typeflag != tar.TypeReg {
				rejected = append(rejected, orig+": not a regular file")
				continue
			}
			if len(files) >= MaxFiles {
				rejected = append(rejected, fmt.Sprintf("%s: too many files (max %d)", orig, MaxFiles))
				continue
			}
			if h.Size > MaxFileBytes {
				rejected = append(rejected, fmt.Sprintf("%s: too large (%s, max %s)", orig, humanSize(h.Size), humanSize(MaxFileBytes)))
				continue
			}
			fn, err := SanitizeName(orig)
			if err != nil {
				rejected = append(rejected, orig+": "+err.Error())
				continue
			}
			data, err := io.ReadAll(io.LimitReader(tr, MaxFileBytes+1))
			if err != nil {
				return res, files, rejected, err
			}
			if err := CheckContent(Ext(fn), data); err != nil {
				rejected = append(rejected, orig+": "+err.Error())
				continue
			}
			p, err := s.write(fn, data, names)
			if err != nil {
				return res, files, rejected, err
			}
			files = append(files, made{Path: p, Size: int64(len(data))})
		}
	}
}

// write puts data into the output dir under fn, or fn-2, fn-3, … if taken; never overwrites.
func (s *Sandbox) write(fn string, data []byte, names map[string]bool) (string, error) {
	ext := path.Ext(fn)
	stem := strings.TrimSuffix(fn, ext)
	for i := 1; i < 1000; i++ {
		cand := fn
		if i > 1 {
			cand = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		if names[strings.ToLower(cand)] {
			continue
		}
		p := filepath.Join(s.opt.OutDir, cand)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(data)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(p)
			return "", werr
		}
		names[strings.ToLower(cand)] = true
		return p, nil
	}
	return "", fmt.Errorf("no free name for %s", fn)
}

// report is the text for the model.
func report(tool, want string, limit time.Duration, memory string, res *runResult, files []made, rejected []string) agent.Result {
	pages := map[string]int{}
	for _, f := range res.Files {
		if f.Pages != nil {
			pages[path.Base(f.Name)] = *f.Pages
		}
	}
	var skipped []string
	for _, sk := range res.Skipped {
		skipped = append(skipped, sk.Name+": "+sk.Reason)
	}
	skipped = append(skipped, rejected...)
	logTail := func(n int) string {
		l := strings.TrimSpace(res.Log)
		if len(l) > n {
			l = "…" + l[len(l)-n:]
		}
		return l
	}
	again := "Fix the code and call make_file again."
	if tool == "markdown_to_pdf" {
		again = "Fix the Markdown and try again."
	}

	var b strings.Builder
	switch {
	case res.TimedOut:
		fmt.Fprintf(&b, "The code was stopped after %.0f s (time limit); no files were delivered.", limit.Seconds())
		if l := logTail(1500); l != "" {
			fmt.Fprintf(&b, "\nLast output:\n%s", l)
		}
		b.WriteString("\nMake it faster (no endless loops, less data) and call make_file again.")
		return agent.Result{Text: b.String(), IsError: true}
	case res.Exit != 0:
		removeAll(files)
		if res.Exit == -9 {
			b.WriteString("The code was killed (probably out of memory: the sandbox has " + memory + "); no files were delivered.")
		} else {
			fmt.Fprintf(&b, "The code failed (exit status %d); no files were delivered.", res.Exit)
		}
		if l := logTail(2500); l != "" {
			fmt.Fprintf(&b, "\nOutput (end):\n%s", l)
		}
		b.WriteString("\n" + again)
		return agent.Result{Text: b.String(), IsError: true}
	case len(files) == 0:
		fmt.Fprintf(&b, "The code ran but no file was delivered. Write the file to /out/%s (os.environ[\"OUT_FILE\"]).", want)
		if len(skipped) > 0 {
			b.WriteString("\nNot delivered:\n- " + strings.Join(skipped, "\n- "))
		}
		if l := logTail(1500); l != "" {
			fmt.Fprintf(&b, "\nOutput (end):\n%s", l)
		}
		return agent.Result{Text: b.String(), IsError: true}
	}

	r := agent.Result{}
	fmt.Fprintf(&b, "Created %d file", len(files))
	if len(files) > 1 {
		b.WriteString("s")
	}
	b.WriteString(" (the user can download them):")
	for _, f := range files {
		base := filepath.Base(f.Path)
		fmt.Fprintf(&b, "\n- %s (%s", base, humanSize(f.Size))
		if n, ok := pages[base]; ok {
			fmt.Fprintf(&b, ", %d page", n)
			if n != 1 {
				b.WriteString("s")
			}
		}
		b.WriteString(")")
		r.Files = append(r.Files, f.Path)
	}
	if len(skipped) > 0 {
		b.WriteString("\nNot delivered:\n- " + strings.Join(skipped, "\n- "))
	}
	if l := logTail(800); l != "" && tool == "make_file" {
		fmt.Fprintf(&b, "\nOutput (end):\n%s", l)
	}
	r.Text = b.String()
	return r
}

func errResult(format string, a ...any) agent.Result {
	return agent.Result{Text: fmt.Sprintf(format, a...), IsError: true}
}

func removeAll(files []made) {
	for _, f := range files {
		os.Remove(f.Path)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// tailWriter keeps the last 8 KB written to it.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > 8<<10 {
		w.buf = w.buf[len(w.buf)-8<<10:]
	}
	return len(p), nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}
