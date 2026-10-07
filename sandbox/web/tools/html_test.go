package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestHTMLToText(t *testing.T) {
	page := `<!DOCTYPE html><html><head><title> The &amp; Title </title><style>body{color:red}</style>
<script>var x = "<p>not text</p>";</script></head>
<body><nav><a href="/">Home</a> <a href="/x">Menu</a></nav>
<h1>Heading</h1><p>First <b>para</b>graph &mdash; with&nbsp;entities.</p>
<!-- a comment <p>hidden</p> -->
<ul><li>one</li><li>two</li></ul>
<table><tr><td>a</td><td>b</td></tr></table>
<p data-x="a>b">attr with &gt; inside</p>
<footer>Copyright</footer></body></html>`
	title, text := HTMLToText(page)
	if title != "The & Title" {
		t.Errorf("title %q", title)
	}
	for _, want := range []string{"# Heading", "First paragraph — with entities.", "- one", "- two", "a | b", "attr with > inside"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	for _, bad := range []string{"color:red", "not text", "hidden", "Menu", "Copyright", "<", "var x"} {
		if strings.Contains(text, bad) {
			t.Errorf("unexpected %q in:\n%s", bad, text)
		}
	}
}

func TestHTMLToTextPrefersArticle(t *testing.T) {
	body := strings.Repeat("Real content sentence. ", 30)
	page := `<html><body><div>Sidebar junk</div><article><p>` + body + `</p></article><div>More junk</div></body></html>`
	_, text := HTMLToText(page)
	if strings.Contains(text, "junk") || !strings.Contains(text, "Real content") {
		t.Errorf("article not preferred:\n%s", text)
	}
}

func TestCheckURL(t *testing.T) {
	for _, s := range []string{"file:///etc/passwd", "data:text/html,hi", "ftp://x.com/", "javascript:alert(1)",
		"http://user:pw@example.com/", "http:///nohost", "gopher://x.com"} {
		u, err := url.Parse(s)
		if err == nil && checkURL(u) == nil {
			t.Errorf("%s allowed", s)
		}
	}
	for _, s := range []string{"http://example.com/", "https://example.com/a?b=c"} {
		u, _ := url.Parse(s)
		if err := checkURL(u); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
}

func TestDecodeLatin1(t *testing.T) {
	if got := decode([]byte{'G', 0xfc, 'n', 't', 'e', 'r'}, "ISO-8859-1"); got != "Günter" {
		t.Errorf("got %q", got)
	}
}
