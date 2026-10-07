package main

import (
	"html"
	"strings"
)

// skipContent: elements whose content is never text for a reader.
var skipContent = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true, "svg": true, "math": true, "iframe": true,
	"object": true, "canvas": true, "select": true, "nav": true, "footer": true, "button": true, "form": true,
	"head": true, "dialog": true,
}

// blockTags start a new line.
var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "section": true, "article": true, "main": true, "header": true,
	"aside": true, "blockquote": true, "pre": true, "table": true, "tr": true, "ul": true, "ol": true, "dl": true,
	"dt": true, "dd": true, "hr": true, "figure": true, "figcaption": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "address": true, "details": true, "summary": true, "body": true,
}

// HTMLToText turns a page into its title and readable text: scripts, styles, navigation and markup go, block
// elements become lines, list items "- ", headings "# ". If the page has an <article> or <main> with enough text,
// only that is used.
func HTMLToText(src string) (title, text string) {
	title = extractTitle(src)
	full := renderText(src)
	for _, tag := range []string{"article", "main"} {
		if region, ok := region(src, tag); ok {
			if t := renderText(region); len(t) >= 400 {
				return title, t
			}
		}
	}
	return title, full
}

func extractTitle(src string) string {
	lower := strings.ToLower(src)
	i := strings.Index(lower, "<title")
	if i < 0 {
		return ""
	}
	j := strings.IndexByte(lower[i:], '>')
	if j < 0 {
		return ""
	}
	start := i + j + 1
	end := strings.Index(lower[start:], "</title")
	if end < 0 {
		return ""
	}
	return collapseSpace(html.UnescapeString(src[start : start+end]))
}

// region returns the source from the first <tag ...> to the last </tag>.
func region(src, tag string) (string, bool) {
	lower := strings.ToLower(src)
	i := indexTag(lower, tag)
	if i < 0 {
		return "", false
	}
	j := strings.LastIndex(lower, "</"+tag)
	if j <= i {
		return "", false
	}
	return src[i:j], true
}

// indexTag finds "<tag" followed by '>' or whitespace (so "<main" does not match "<mainframe").
func indexTag(lower, tag string) int {
	off := 0
	for {
		i := strings.Index(lower[off:], "<"+tag)
		if i < 0 {
			return -1
		}
		k := off + i + 1 + len(tag)
		if k >= len(lower) || strings.ContainsRune(" \t\r\n/>", rune(lower[k])) {
			return off + i
		}
		off = k
	}
}

func renderText(src string) string {
	var b strings.Builder
	lower := strings.ToLower(src)
	i := 0
	for i < len(src) {
		lt := strings.IndexByte(src[i:], '<')
		if lt < 0 {
			b.WriteString(html.UnescapeString(src[i:]))
			break
		}
		b.WriteString(html.UnescapeString(src[i : i+lt]))
		i += lt
		switch {
		case strings.HasPrefix(src[i:], "<!--"):
			end := strings.Index(src[i+4:], "-->")
			if end < 0 {
				i = len(src)
			} else {
				i += 4 + end + 3
			}
			continue
		case strings.HasPrefix(src[i:], "<!") || strings.HasPrefix(src[i:], "<?"):
			end := strings.IndexByte(src[i:], '>')
			if end < 0 {
				i = len(src)
			} else {
				i += end + 1
			}
			continue
		}
		name, closing, end := parseTag(src, i)
		if name == "" { // a lone '<'
			b.WriteByte('<')
			i++
			continue
		}
		i = end
		if !closing && skipContent[name] && !selfClosing(src, end) {
			// skip to the matching close tag (no nesting needed for these)
			k := strings.Index(lower[i:], "</"+name)
			if k < 0 {
				i = len(src)
				continue
			}
			i += k
			if e := strings.IndexByte(src[i:], '>'); e >= 0 {
				i += e + 1
			} else {
				i = len(src)
			}
			continue
		}
		switch {
		case name == "li":
			if !closing {
				b.WriteString("\n- ")
			}
		case len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6':
			if closing {
				b.WriteString("\n\n")
			} else {
				b.WriteString("\n\n" + strings.Repeat("#", int(name[1]-'0')) + " ")
			}
		case name == "td" || name == "th":
			if !closing {
				b.WriteString(" | ")
			}
		case name == "p":
			b.WriteString("\n\n")
		case blockTags[name]:
			b.WriteString("\n")
		case name == "img":
			// alt text carries meaning on some pages; ignore for now
		}
	}
	return tidy(b.String())
}

func selfClosing(src string, end int) bool {
	return end >= 2 && src[end-2] == '/'
}

// parseTag reads the tag at src[i] ('<'): its lowercase name, whether it is a closing tag, and the index after '>'.
func parseTag(src string, i int) (name string, closing bool, end int) {
	j := i + 1
	if j < len(src) && src[j] == '/' {
		closing = true
		j++
	}
	k := j
	for k < len(src) && (src[k] >= 'a' && src[k] <= 'z' || src[k] >= 'A' && src[k] <= 'Z' || src[k] >= '0' && src[k] <= '9' || src[k] == '-') {
		k++
	}
	if k == j {
		return "", false, i + 1
	}
	name = strings.ToLower(src[j:k])
	// skip attributes, respecting quotes
	var quote byte
	for k < len(src) {
		c := src[k]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return name, closing, k + 1
		}
		k++
	}
	return name, closing, len(src)
}

func htmlUnescape(s string) string { return html.UnescapeString(s) }

func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// tidy collapses spaces within lines, trims lines, and keeps at most one empty line in a row.
func tidy(s string) string {
	s = strings.ReplaceAll(s, " ", " ")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := true
	for _, l := range lines {
		l = collapseSpace(l)
		l = strings.Trim(l, "| ")
		if l == "" || l == "-" {
			if !blank {
				out = append(out, "")
			}
			blank = true
			continue
		}
		out = append(out, l)
		blank = false
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
