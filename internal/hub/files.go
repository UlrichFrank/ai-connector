package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// Files a job names (images, audio) are /files/ urls of a worker — or, from an application that cannot upload to a
// worker, data: URLs: the hub uploads them to the worker the job goes to (docs/api.md "Native jobs API").

const maxInlineFile = 16 << 20 // one file of a job as a data: URL (decoded)

// inlineTypes are the types a job's file may come as, with the extension it is uploaded under.
var inlineTypes = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp",
	"audio/wav": ".wav", "audio/x-wav": ".wav", "audio/mpeg": ".mp3", "audio/mp4": ".m4a", "audio/x-m4a": ".m4a",
	"audio/aac": ".aac", "audio/flac": ".flac", "audio/ogg": ".ogg",
}

// inlineFile reads a data: URL (false: u is not one): its extension and bytes.
func inlineFile(u string) (ext string, data []byte, ok bool, err error) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return "", nil, false, nil
	}
	head, enc, found := strings.Cut(rest, ",")
	mime, b64, _ := strings.Cut(head, ";")
	mime = strings.ToLower(strings.TrimSpace(mime))
	if mime == "image/jpg" {
		mime = "image/jpeg"
	}
	ext = inlineTypes[mime]
	switch {
	case !found || !strings.EqualFold(strings.TrimSpace(b64), "base64"):
		return "", nil, true, badf("a file is a base64 data: URL (data:image/png;base64,…)")
	case ext == "":
		return "", nil, true, badf("file type %q is not taken (png, jpeg, webp; wav, mp3, m4a, aac, flac, ogg)", mime)
	case base64.StdEncoding.DecodedLen(len(enc)) > maxInlineFile+3:
		return "", nil, true, badf("a file is larger than %d MB", maxInlineFile>>20)
	}
	data, err = base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	if err != nil {
		return "", nil, true, badf("a file's base64 does not decode")
	}
	if len(data) > maxInlineFile {
		return "", nil, true, badf("a file is larger than %d MB", maxInlineFile>>20)
	}
	return ext, data, true, nil
}

func badf(format string, a ...any) error { return &badFile{fmt.Sprintf(format, a...)} }

// placeFile makes a file of a job usable on the Mac to: a data: URL uploaded to it, a /files/ url of another Mac copied
// there (bring), anything else as it is.
func (s *Server) placeFile(ctx context.Context, u, name string, to *node) (string, error) {
	ext, data, ok, err := inlineFile(u)
	if !ok {
		return s.bring(ctx, u, to)
	}
	if err != nil {
		return "", err
	}
	return s.uploadFile(ctx, to, name+ext, data)
}

// uploadFile puts a file into a worker's uploads (POST /api/upload) and returns its /files/ url there.
func (s *Server) uploadFile(ctx context.Context, n *node, name string, data []byte) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", name)
	if err == nil {
		_, err = part.Write(data)
	}
	if err == nil {
		err = mw.Close()
	}
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := s.fleet.request(ctx, n, http.MethodPost, "/api/upload", &body, mw.FormDataContentType())
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct {
		URL string `json:"url"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&out) != nil || out.URL == "" {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return "", fmt.Errorf("%s %s", res.Status, strings.TrimFunc(string(msg), unicode.IsSpace))
	}
	return out.URL, nil
}

// fileError answers a file of a job that could not be placed: a bad data: URL is the caller's (400), else the
// copy or upload failed (502).
func fileError(w http.ResponseWriter, err error) {
	var bad *badFile
	if errors.As(err, &bad) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": bad.msg})
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "copy", "message": err.Error()})
}

// badFile: a data: URL that is not taken.
type badFile struct{ msg string }

func (e *badFile) Error() string { return e.msg }
