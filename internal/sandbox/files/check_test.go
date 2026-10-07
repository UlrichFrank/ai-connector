package files

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/agent"
)

func TestSanitizeName(t *testing.T) {
	ok := map[string]string{
		"report.pdf":                 "report.pdf",
		"Report.PDF":                 "Report.pdf",
		"../../etc/passwd.txt":       "passwd.txt",
		`C:\Users\x\chart.png`:       "chart.png",
		"Quartalsbericht Größe.xlsx": "Quartalsbericht_Größe.xlsx",
		".hidden.csv":                "hidden.csv",
		"a  b??c.md":                 "a_b_c.md",
		"name;rm -rf.html":           "name_rm_-rf.html",
		"x.tar.svg":                  "x.tar.svg",
		"/out/sub/data.csv":          "data.csv",
		"émoji 🎉 file.docx":          "émoji_file.docx",
	}
	for in, want := range ok {
		got, err := SanitizeName(in)
		if err != nil || got != want {
			t.Errorf("SanitizeName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", ".", "..", "/", "noext", "script.py", "a.exe", "x.pdf.sh", ".pdf", "___.pdf", "evil.PHP", "file."} {
		if got, err := SanitizeName(bad); err == nil {
			t.Errorf("SanitizeName(%q) = %q, want an error", bad, got)
		}
	}
	long := strings.Repeat("a", 300) + ".pdf"
	got, err := SanitizeName(long)
	if err != nil || len([]rune(got)) != maxNameRunes+4 {
		t.Errorf("long name: %q, %v", got, err)
	}
}

func ooxml(t *testing.T, names ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("<x/>"))
	}
	zw.Close()
	return b.Bytes()
}

func TestCheckContent(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"), make([]byte, 20)...)
	xlsx := ooxml(t, "[Content_Types].xml", "xl/workbook.xml")
	docx := ooxml(t, "[Content_Types].xml", "word/document.xml")
	svg := []byte(`<?xml version="1.0"?><!DOCTYPE svg PUBLIC "-//W3C//DTD SVG 1.1//EN" "x"><svg xmlns="http://www.w3.org/2000/svg"><rect width="1"/></svg>`)
	good := []struct {
		ext  string
		data []byte
	}{
		{"pdf", []byte("%PDF-1.7\n...")},
		{"png", png},
		{"xlsx", xlsx},
		{"docx", docx},
		{"csv", []byte("a,b\n1,ä\n")},
		{"txt", []byte("\xef\xbb\xbfhello")},
		{"md", []byte("# Title\n\n| a | b |\n")},
		{"svg", svg},
		{"html", []byte("<!doctype html><html><body><h1>Hi</h1><p style=\"color:red\">x</p></body></html>")},
	}
	for _, g := range good {
		if err := CheckContent(g.ext, g.data); err != nil {
			t.Errorf("%s: unexpected error %v", g.ext, err)
		}
	}
	bad := []struct {
		ext  string
		data []byte
	}{
		{"pdf", []byte("not a pdf")},
		{"pdf", png},
		{"png", []byte("%PDF-1.4")},
		{"png", []byte("\x89PNG\r\n\x1a\n")},
		{"xlsx", docx},
		{"docx", xlsx},
		{"docx", []byte("PK\x03\x04garbage")},
		{"xlsx", ooxml(t, "[Content_Types].xml", "xl/workbook.xml", "xl/vbaProject.bin")},
		{"csv", []byte("a\x00b")},
		{"txt", []byte("\xff\xfe\xfd")},
		{"svg", []byte("hello")},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"></svg>`)},
		{"svg", []byte(`<svg><a href="javascript:alert(1)">x</a></svg>`)},
		{"svg", []byte(`<!DOCTYPE svg [<!ENTITY a "aaaa">]><svg>&a;</svg>`)},
		{"html", []byte(`<html><body><SCRIPT src=x></script></body></html>`)},
		{"html", []byte(`<img src=x onerror=alert(1)>`)},
		{"html", []byte(`<iframe src="https://example.com"></iframe>`)},
		{"exe", []byte("MZ")},
		{"pdf", nil},
		{"pdf", append([]byte("%PDF-"), make([]byte, MaxFileBytes)...)},
	}
	for i, b := range bad {
		if err := CheckContent(b.ext, b.data); err == nil {
			t.Errorf("bad #%d (%s): no error", i, b.ext)
		}
	}
}

// fakeTar is a runner stream as sandbox/files/runner.py writes it.
func fakeTar(t *testing.T, res string, files map[string][]byte, extra func(*tar.Writer)) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "result.json", Size: int64(len(res)), Mode: 0o644, Typeflag: tar.TypeReg})
	tw.Write([]byte(res))
	for n, d := range files {
		tw.WriteHeader(&tar.Header{Name: "out/" + n, Size: int64(len(d)), Mode: 0o644, Typeflag: tar.TypeReg})
		tw.Write(d)
	}
	if extra != nil {
		extra(tw)
	}
	tw.Close()
	return b.Bytes()
}

func TestReadTar(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "report.pdf"), []byte("%PDF-old"), 0o644) // taken: must not be overwritten
	s, err := New(Options{OutDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	stream := fakeTar(t, `{"exit":0,"files":[{"name":"report.pdf","size":9,"pages":2}]}`, map[string][]byte{
		"report.pdf":      []byte("%PDF-1.7\n"),
		"../../evil.txt":  []byte("hi"),
		"fake.png":        []byte("%PDF-1.7"),
		"script.sh":       []byte("#!/bin/sh"),
		"sub/table.csv":   []byte("a,b\n"),
		"bad\x00name.csv": []byte("a"),
	}, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: "out/link.pdf", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink})
	})
	res, files, rejected, err := s.readTar(bytes.NewReader(stream))
	if err != nil || res == nil {
		t.Fatalf("readTar: %v %v", res, err)
	}
	got := map[string]bool{}
	for _, f := range files {
		if filepath.Dir(f.Path) != dir {
			t.Errorf("file outside the output dir: %s", f.Path)
		}
		got[filepath.Base(f.Path)] = true
	}
	for _, want := range []string{"report-2.pdf", "evil.txt", "table.csv"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if old, _ := os.ReadFile(filepath.Join(dir, "report.pdf")); string(old) != "%PDF-old" {
		t.Error("an existing file was overwritten")
	}
	joined := strings.Join(rejected, "\n")
	for _, want := range []string{"fake.png", "script.sh", "link.pdf"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s not rejected: %s", want, joined)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "fake.png")); err == nil {
		t.Error("fake.png was written")
	}
}

func TestReadTarTooMany(t *testing.T) {
	s, _ := New(Options{OutDir: t.TempDir()})
	files := map[string][]byte{}
	for i := 0; i < MaxFiles+3; i++ {
		files[string(rune('a'+i))+".txt"] = []byte("x")
	}
	_, made, rejected, err := s.readTar(bytes.NewReader(fakeTar(t, `{"exit":0}`, files, nil)))
	if err != nil || len(made) != MaxFiles || len(rejected) != 3 {
		t.Fatalf("made %d, rejected %v, err %v", len(made), rejected, err)
	}
}

func TestReport(t *testing.T) {
	r := report("make_file", "x.pdf", time.Minute, "2g", &runResult{Exit: 1, Log: "Traceback (most recent call last):\nNameError: name 'foo' is not defined"}, nil, nil)
	if !r.IsError || !strings.Contains(r.Text, "NameError") {
		t.Errorf("failure: %+v", r)
	}
	r = report("make_file", "x.pdf", time.Minute, "2g", &runResult{TimedOut: true}, nil, nil)
	if !r.IsError || !strings.Contains(r.Text, "60 s") {
		t.Errorf("timeout: %+v", r)
	}
	r = report("make_file", "x.pdf", time.Minute, "2g", &runResult{}, nil, []string{"big.pdf: too large"})
	if !r.IsError || !strings.Contains(r.Text, "big.pdf: too large") {
		t.Errorf("nothing delivered: %+v", r)
	}
}

func TestToolsAndArgs(t *testing.T) {
	s, _ := New(Options{OutDir: t.TempDir(), CLI: "/nonexistent/container"})
	tools, _ := s.Tools(context.Background())
	if len(tools) != 2 {
		t.Fatal(tools)
	}
	for _, tl := range tools {
		if tl.Zone != agent.ZoneFiles || tl.Changes || tl.Untrusted || !json.Valid(tl.Parameters) {
			t.Errorf("tool %s: %+v", tl.Name, tl)
		}
	}
	// argument errors come back to the model, before any container is started
	for _, c := range []struct{ tool, args string }{
		{"make_file", `{"filename":"x.exe","code":"print(1)"}`},
		{"make_file", `{"filename":"x.pdf","code":""}`},
		{"make_file", `{"filename":"x.pdf","code":"1","language":"bash"}`},
		{"markdown_to_pdf", `{"filename":"x.pdf","markdown":" "}`},
		{"make_file", `not json`},
	} {
		r, err := s.Call(context.Background(), c.tool, json.RawMessage(c.args))
		if err != nil || !r.IsError {
			t.Errorf("%s %s: %+v %v", c.tool, c.args, r, err)
		}
	}
	if !strings.HasPrefix(DefaultImage(), "mlxlab-sandbox-files:") {
		t.Error(DefaultImage())
	}
	args := strings.Join(s.runArgs("mlxlab-files-x"), " ")
	for _, want := range []string{"--network none", "--read-only", "--tmpfs /out", "--rm", "--cap-drop ALL", "--memory", "--cpus"} {
		if !strings.Contains(args, want) {
			t.Errorf("run args lack %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--volume") || strings.Contains(args, "--mount") || strings.Contains(args, " -v ") {
		t.Errorf("run args mount something: %s", args)
	}
}
