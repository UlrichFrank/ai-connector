//go:build sandbox

// Integration tests against the real Apple `container` runtime: go test -tags sandbox ./internal/sandbox/files
// (make sandbox-files-test). The first run builds the image (a few minutes).
package files

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func newSandbox(t *testing.T, timeout time.Duration) *Sandbox {
	t.Helper()
	s, err := New(Options{OutDir: t.TempDir(), Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if err := s.EnsureImage(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func call(t *testing.T, s *Sandbox, tool string, args map[string]string) (r resultT) {
	t.Helper()
	b, _ := json.Marshal(args)
	t0 := time.Now()
	res, err := s.Call(context.Background(), tool, b)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	t.Logf("%s %s (%.1fs, error=%v):\n%s", tool, args["filename"], time.Since(t0).Seconds(), res.IsError, res.Text)
	return resultT{Text: res.Text, Files: res.Files, IsError: res.IsError, Took: time.Since(t0)}
}

type resultT struct {
	Text    string
	Files   []string
	IsError bool
	Took    time.Duration
}

func wantFile(t *testing.T, r resultT, ext string, magic string) []byte {
	t.Helper()
	if r.IsError || len(r.Files) == 0 {
		t.Fatalf("no file: %s", r.Text)
	}
	f := r.Files[0]
	if Ext(f) != ext {
		t.Fatalf("got %s, want .%s", f, ext)
	}
	data, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), magic) {
		t.Fatalf("%s does not start with %q", f, magic)
	}
	return data
}

func TestSandbox(t *testing.T) {
	s := newSandbox(t, 60*time.Second)

	t.Run("pdf", func(t *testing.T) {
		r := call(t, s, "make_file", map[string]string{"filename": "Bericht.pdf", "language": "python", "code": `
import os
from reportlab.lib.pagesizes import A4
from reportlab.pdfbase import pdfmetrics
from reportlab.pdfbase.ttfonts import TTFont
from reportlab.pdfgen import canvas
pdfmetrics.registerFont(TTFont("DejaVu", "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"))
c = canvas.Canvas(os.environ["OUT_FILE"], pagesize=A4)
c.setFont("DejaVu", 14)
c.drawString(72, 760, "Grüße aus der Sandbox: äöü ÄÖÜ ß €")
c.showPage(); c.save()
`})
		wantFile(t, r, "pdf", "%PDF-")
		if !strings.Contains(r.Text, "1 page") {
			t.Errorf("no page count: %s", r.Text)
		}
	})

	t.Run("chart png", func(t *testing.T) {
		r := call(t, s, "make_file", map[string]string{"filename": "chart.png", "code": `
import os
import matplotlib.pyplot as plt
fig, ax = plt.subplots(figsize=(6, 3))
ax.plot([1, 2, 3, 4], [3, 1, 4, 1], marker="o")
ax.set_title("Temperatur (°C) – Übersicht")
fig.savefig(os.environ["OUT_FILE"], dpi=120)
`})
		wantFile(t, r, "png", "\x89PNG")
	})

	t.Run("xlsx", func(t *testing.T) {
		r := call(t, s, "make_file", map[string]string{"filename": "zahlen.xlsx", "code": `
import os
from openpyxl import Workbook
wb = Workbook(); ws = wb.active; ws.title = "Daten"
ws.append(["Monat", "Umsatz"])
for m, v in [("Jan", 10), ("Feb", 12), ("Mär", 9)]:
    ws.append([m, v])
ws["D1"] = "=SUM(B2:B4)"
wb.save(os.environ["OUT_FILE"])
`})
		data := wantFile(t, r, "xlsx", "PK")
		if err := CheckContent("xlsx", data); err != nil {
			t.Error(err)
		}
	})

	t.Run("docx and csv", func(t *testing.T) {
		r := call(t, s, "make_file", map[string]string{"filename": "brief.docx", "code": `
import os, pandas as pd
from docx import Document
d = Document(); d.add_heading("Hallo Welt", 1); d.add_paragraph("Größe: 42"); d.save(os.environ["OUT_FILE"])
pd.DataFrame({"a": [1, 2], "b": ["x", "ü"]}).to_csv("/out/tabelle.csv", index=False)
`})
		if r.IsError || len(r.Files) != 2 {
			t.Fatalf("want 2 files: %s", r.Text)
		}
	})

	t.Run("markdown_to_pdf", func(t *testing.T) {
		r := call(t, s, "markdown_to_pdf", map[string]string{"filename": "notes", "title": "Notizen",
			"markdown": "## Liste\n\n- eins\n- zwei 🎉\n\n| a | b |\n|---|---|\n| 1 | ä |\n\n```go\nfunc main() {}\n```\n\n![x](https://example.com/x.png)\n"})
		wantFile(t, r, "pdf", "%PDF-")
	})

	t.Run("failing script returns the traceback", func(t *testing.T) {
		r := call(t, s, "make_file", map[string]string{"filename": "x.pdf", "code": "def f():\n    return 1 / 0\n\nf()\n"})
		if !r.IsError || !strings.Contains(r.Text, "Traceback") || !strings.Contains(r.Text, "ZeroDivisionError") || len(r.Files) != 0 {
			t.Fatalf("want the traceback: %+v", r)
		}
	})

	t.Run("no network", func(t *testing.T) {
		r := call(t, s, "make_file", map[string]string{"filename": "net.txt", "code": `
import os, socket, urllib.request
results = []
def attempt(label, fn):
    try:
        fn()
        results.append("REACHED " + label)
    except Exception as e:
        results.append("blocked %s: %s" % (label, type(e).__name__ + ": " + str(e)[:80]))
attempt("http://1.1.1.1", lambda: urllib.request.urlopen("http://1.1.1.1", timeout=5))
attempt("http://192.168.0.1", lambda: urllib.request.urlopen("http://192.168.0.1", timeout=5))
attempt("tcp 192.168.64.1:22 (container gateway / Mac)", lambda: socket.create_connection(("192.168.64.1", 22), timeout=5))
attempt("tcp 8.8.8.8:53", lambda: socket.create_connection(("8.8.8.8", 53), timeout=5))
attempt("dns example.com", lambda: socket.getaddrinfo("example.com", 80))
def udp():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(3)
    s.sendto(b"x", ("1.1.1.1", 53)); s.recvfrom(10)
attempt("udp 1.1.1.1:53", udp)
results.append("interfaces: " + ", ".join(n for _, n in socket.if_nameindex()))
open(os.environ["OUT_FILE"], "w").write("\n".join(results) + "\n")
`})
		data := wantFile(t, r, "txt", "")
		t.Logf("network probe:\n%s", data)
		if strings.Contains(string(data), "REACHED") {
			t.Fatalf("the sandbox reached the network:\n%s", data)
		}
		if strings.Count(string(data), "blocked") != 6 {
			t.Fatalf("unexpected probe output:\n%s", data)
		}
	})

	t.Run("100 MB file is rejected", func(t *testing.T) {
		r := call(t, s, "make_file", map[string]string{"filename": "big.pdf", "code": `
import os
with open(os.environ["OUT_FILE"], "wb") as f:
    f.write(b"%PDF-1.4\n")
    chunk = b"0" * (1 << 20)
    for _ in range(100):
        f.write(chunk)
os.symlink("/etc/passwd", "/out/passwd.txt")
open("/out/run.sh", "w").write("echo hi")
open("/out/fake.png", "w").write("not a png")
`})
		if !r.IsError || len(r.Files) != 0 || !strings.Contains(r.Text, "too large") {
			t.Fatalf("want rejection: %+v", r)
		}
		for _, w := range []string{"passwd.txt", "run.sh", "fake.png"} {
			if !strings.Contains(r.Text, w) {
				t.Errorf("%s not mentioned as rejected", w)
			}
		}
		if entries, _ := os.ReadDir(s.opt.OutDir); len(entries) > 0 {
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "big") || strings.HasPrefix(e.Name(), "fake") || strings.HasPrefix(e.Name(), "passwd") {
					t.Errorf("rejected file in the output dir: %s", e.Name())
				}
			}
		}
	})

	t.Run("read-only root, non-root user", func(t *testing.T) {
		r := call(t, s, "make_file", map[string]string{"filename": "env.txt", "code": `
import os
lines = ["uid=%d" % os.getuid()]
for p in ["/etc/x", "/usr/x", "/home/sandbox/x", "/opt/sandbox/x"]:
    try:
        open(p, "w").write("x"); lines.append("WROTE " + p)
    except OSError as e:
        lines.append("denied %s: %s" % (p, e.strerror))
open(os.environ["OUT_FILE"], "w").write("\n".join(lines))
`})
		data := string(wantFile(t, r, "txt", "uid="))
		t.Logf("\n%s", data)
		if strings.HasPrefix(data, "uid=0") || strings.Contains(data, "WROTE") {
			t.Fatalf("root or writable root fs:\n%s", data)
		}
	})

	t.Run("memory hog", func(t *testing.T) {
		r := call(t, s, "make_file", map[string]string{"filename": "mem.txt", "code": "x = []\nwhile True:\n    x.append(bytearray(64 << 20))\n"})
		if !r.IsError || len(r.Files) != 0 {
			t.Fatalf("want a failure: %+v", r)
		}
	})

	assertNoContainersLeft(t)
}

func TestSandboxTimeout(t *testing.T) {
	s := newSandbox(t, 10*time.Second)
	r := call(t, s, "make_file", map[string]string{"filename": "loop.pdf", "code": "import os\nopen('/out/partial.txt','w').write('x')\nwhile True:\n    pass\n"})
	if !r.IsError || !strings.Contains(r.Text, "stopped after 10 s") || len(r.Files) != 0 {
		t.Fatalf("want a timeout: %+v", r)
	}
	if r.Took > 10*time.Second+30*time.Second {
		t.Errorf("took %v", r.Took)
	}
	// a child that ignores the parent and forks away is killed with the VM
	r = call(t, s, "make_file", map[string]string{"filename": "fork.txt", "code": "import os, time\nif os.fork() == 0:\n    os.setsid()\n    while True: time.sleep(1)\nopen(os.environ['OUT_FILE'],'w').write('parent done')\n"})
	if r.IsError || r.Took > 30*time.Second {
		t.Fatalf("detached child kept the run alive: %+v", r)
	}
	assertNoContainersLeft(t)
}

func assertNoContainersLeft(t *testing.T) {
	t.Helper()
	out, err := exec.Command("container", "list", "--all", "--quiet").Output()
	if err != nil {
		t.Logf("container list: %v", err)
		return
	}
	for _, id := range strings.Fields(string(out)) {
		if strings.HasPrefix(id, namePref) {
			t.Errorf("container left behind: %s", id)
		}
	}
}
