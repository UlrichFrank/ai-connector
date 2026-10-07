package files

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Allowed are the extensions a sandbox run may hand out.
var Allowed = []string{"pdf", "png", "svg", "csv", "xlsx", "docx", "md", "txt", "html"}

const (
	MaxFileBytes = 25 << 20 // per file
	MaxFiles     = 10       // per call
	maxNameRunes = 100
)

func allowedExt(ext string) bool {
	for _, a := range Allowed {
		if a == ext {
			return true
		}
	}
	return false
}

// SanitizeName turns a name from the model (or from inside the sandbox) into a safe base name: no directories, only
// letters, digits, '-', '_' and '.', no leading dot, at most 100 characters, a lower-case extension from Allowed.
func SanitizeName(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(strings.TrimSpace(name))
	if name == "." || name == "/" || name == "" {
		return "", errors.New("empty file name")
	}
	var b strings.Builder
	last := rune(0)
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '.' {
			r = '_'
		}
		if r == '_' && last == '_' {
			continue
		}
		b.WriteRune(r)
		last = r
	}
	name = strings.TrimLeft(b.String(), "._-")
	name = strings.TrimRight(name, "._-")
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || i == len(name)-1 {
		return "", fmt.Errorf("file name %q needs a name and an extension (%s)", name, strings.Join(Allowed, ", "))
	}
	stem, ext := strings.TrimRight(name[:i], "._-"), strings.ToLower(name[i+1:])
	if !allowedExt(ext) {
		return "", fmt.Errorf("file type .%s is not allowed (allowed: %s)", ext, strings.Join(Allowed, ", "))
	}
	if stem == "" {
		return "", errors.New("file name has no name before the extension")
	}
	if r := []rune(stem); len(r) > maxNameRunes {
		stem = strings.TrimRight(string(r[:maxNameRunes]), "._-")
	}
	return stem + "." + ext, nil
}

// Ext is the lower-case extension of a (sanitized) name.
func Ext(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return strings.ToLower(name[i+1:])
	}
	return ""
}

var (
	pngMagic = []byte("\x89PNG\r\n\x1a\n")
	pdfMagic = []byte("%PDF-")
	zipMagic = []byte("PK\x03\x04")
	bom      = []byte("\xef\xbb\xbf")
	// active content that must not reach a browser from a file the sandbox made (svg and html are served by the web
	// UI): scripts, event handlers, javascript: urls, embedded foreign documents, entity declarations
	activeContent = regexp.MustCompile(`(?i)<\s*script|javascript\s*:|\son[a-z]+\s*=|<\s*foreignobject|<\s*iframe|<\s*object|<\s*embed|<!\s*entity|<\s*meta[^>]+http-equiv`)
)

// CheckContent checks that data is what its extension says.
func CheckContent(ext string, data []byte) error {
	if len(data) == 0 {
		return errors.New("empty file")
	}
	if len(data) > MaxFileBytes {
		return fmt.Errorf("too large (%s, max %s)", humanSize(int64(len(data))), humanSize(MaxFileBytes))
	}
	switch ext {
	case "pdf":
		if !bytes.HasPrefix(data, pdfMagic) {
			return errors.New("not a PDF (no %PDF- header)")
		}
	case "png":
		if !bytes.HasPrefix(data, pngMagic) || len(data) < 24 || string(data[12:16]) != "IHDR" {
			return errors.New("not a PNG image")
		}
	case "xlsx":
		return checkOOXML(data, "xl/", "an Excel workbook")
	case "docx":
		return checkOOXML(data, "word/", "a Word document")
	case "csv", "md", "txt":
		return checkText(data)
	case "svg":
		if err := checkText(data); err != nil {
			return err
		}
		head := strings.ToLower(string(data[:min(len(data), 4096)]))
		if !strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(head, string(bom))), "<") || !strings.Contains(head, "<svg") {
			return errors.New("not an SVG image")
		}
		if activeContent.Match(data) {
			return errors.New("SVG with scripts or other active content is not allowed")
		}
	case "html":
		if err := checkText(data); err != nil {
			return err
		}
		if activeContent.Match(data) {
			return errors.New("HTML with scripts, event handlers or embedded objects is not allowed")
		}
	default:
		return fmt.Errorf("file type .%s is not allowed", ext)
	}
	return nil
}

func checkText(data []byte) error {
	data = bytes.TrimPrefix(data, bom)
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return errors.New("not a UTF-8 text file")
	}
	return nil
}

// checkOOXML: a zip with [Content_Types].xml and the part directory of its kind, without macros.
func checkOOXML(data []byte, dir, what string) error {
	if !bytes.HasPrefix(data, zipMagic) {
		return fmt.Errorf("not %s (no zip header)", what)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("not %s (%v)", what, err)
	}
	var types, part bool
	for _, f := range zr.File {
		switch {
		case f.Name == "[Content_Types].xml":
			types = true
		case strings.HasPrefix(f.Name, dir):
			part = true
		}
		if strings.Contains(strings.ToLower(f.Name), "vbaproject") {
			return errors.New("documents with macros are not allowed")
		}
	}
	if !types || !part {
		return fmt.Errorf("not %s", what)
	}
	return nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
