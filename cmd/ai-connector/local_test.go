package main

import (
	"bytes"
	"errors"
	"flag"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseLocal(t *testing.T) {
	for _, c := range []struct {
		args []string
		addr string // what it listens on; "" an error
	}{
		{nil, "127.0.0.1:0"},
		{[]string{"--addr", "127.0.0.1:8485"}, "127.0.0.1:8485"},
		{[]string{"--addr", "localhost:9000"}, "localhost:9000"},
		{[]string{"--addr", "[::1]:9000"}, "[::1]:9000"},
		{[]string{"--lan"}, ":8485"},
		{[]string{"--lan", "--addr", ":9000"}, ":9000"},
		{[]string{"--lan", "--addr", "192.168.1.5:9000"}, "192.168.1.5:9000"},
		// without --lan: nothing other devices reach
		{[]string{"--addr", ":8485"}, ""},
		{[]string{"--addr", "0.0.0.0:8485"}, ""},
		{[]string{"--addr", "192.168.1.5:8485"}, ""},
		{[]string{"--addr", "8485"}, ""},
		{[]string{"stray"}, ""},
		{[]string{"--install", "--uninstall"}, ""},
		{[]string{"--nope"}, ""},
	} {
		o, err := parseLocal(c.args, &bytes.Buffer{})
		if c.addr == "" {
			if err == nil {
				t.Errorf("%q: want an error", c.args)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.args, err)
			continue
		}
		if got, _ := o.listenAddr(); got != c.addr {
			t.Errorf("%q: listens on %q, want %q", c.args, got, c.addr)
		}
	}
	o, err := parseLocal([]string{"--lan", "--name", "Studio", "--console", "127.0.0.1:8486", "--state", "/s", "--print-token", "--install"}, nil)
	if err != nil || !o.lan || o.name != "Studio" || o.console != "127.0.0.1:8486" || o.state != "/s" || !o.printToken || !o.install {
		t.Fatalf("%+v %v", o, err)
	}
}

// --help prints the usage (with --install: OpenCAD looks for it) and starts nothing.
func TestParseLocalHelp(t *testing.T) {
	for _, h := range []string{"-h", "--help", "-help"} {
		var out bytes.Buffer
		if _, err := parseLocal([]string{"--lan", h}, &out); !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("%s: %v", h, err)
		}
		if !strings.HasPrefix(out.String(), "ai-connector local") || !strings.Contains(out.String(), "--install") {
			t.Fatalf("%s: %q", h, out.String())
		}
	}
}

func TestStandaloneArgs(t *testing.T) {
	in := []string{"--lan", "--standalone", "--name", "x"}
	if rest, ok := standaloneArgs(in); !ok || !slices.Equal(rest, []string{"--lan", "--name", "x"}) || len(in) != 4 {
		t.Fatalf("%q %v (in %q)", rest, ok, in)
	}
	if _, ok := standaloneArgs([]string{"--addr", "127.0.0.1:8484"}); ok {
		t.Fatal("plain serve taken for --standalone")
	}
}

func TestLocalKind(t *testing.T) {
	k := localKind(localOptions{})
	if k.label != "de.rnd7.ai-connector.local" || k.unit != "ai-connector-local.service" || k.task != "ai-connector local" ||
		!slices.Equal(k.args, []string{"local"}) {
		t.Fatalf("%+v", k)
	}
	o, err := parseLocal([]string{"--lan", "--addr", ":9000", "--name", "Studio Mac", "--state", "rel/state", "--print-token", "--install"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	k = localKind(o)
	want := []string{"local", "--lan", "--addr", ":9000", "--name", "Studio Mac", "--state", absPath("rel/state")}
	if !slices.Equal(k.args, want) {
		t.Fatalf("args %q, want %q", k.args, want)
	}
	// in the service's files as the worker's are
	s, _ := testService(t, "linux", nil)
	s.kind = &k
	if !strings.Contains(s.content(), `local "--lan" "--addr" ":9000" "--name" "Studio Mac"`) {
		t.Fatalf("unit:\n%s", s.content())
	}
	if !strings.HasSuffix(s.file(), "ai-connector-local.service") {
		t.Fatal(s.file())
	}
}

func TestLocalBase(t *testing.T) {
	for in, want := range map[string]string{"[::]:8485": "http://127.0.0.1:8485", "0.0.0.0:8485": "http://127.0.0.1:8485",
		"127.0.0.1:5000": "http://127.0.0.1:5000", "[::1]:5000": "http://[::1]:5000"} {
		a, _ := net.ResolveTCPAddr("tcp", in)
		if got := localBase(a); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	a, _ := net.ResolveTCPAddr("tcp", "[::]:8485")
	if urls := lanURLs(a, "Studio"); len(urls) == 0 || urls[0] != "http://studio.local:8485" {
		t.Fatalf("%q", urls)
	}
}

func TestConsolePasswordKept(t *testing.T) {
	dir := t.TempDir()
	a, err := consolePassword(dir)
	if err != nil || len(a) != 24 {
		t.Fatalf("%q %v", a, err)
	}
	if b, _ := consolePassword(dir); b != a {
		t.Fatal("a second password")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "console-password")); fi.Mode().Perm() != 0o600 {
		t.Fatal(fi.Mode())
	}
}
