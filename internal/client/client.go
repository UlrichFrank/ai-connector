// Package client talks to mlxlab over HTTP: to the hub (which signs people in and forwards the worker's API) or
// straight to `mlxlab serve`. The TUI is built on it; see docs/architecture.md for the contract.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
)

// ServerEnv names the server when neither --server nor the saved config does (there is no built-in default).
const ServerEnv = "MLXLAB_SERVER"

var (
	// ErrSignIn: the server wants a device token (none was sent, or it was revoked).
	ErrSignIn = errors.New("not signed in")
	// ErrOffline: the hub has no worker, the Mac is offline.
	ErrOffline = errors.New("the Mac is offline")
	// ErrPassword: the sign-in password was wrong.
	ErrPassword = errors.New("wrong password")
)

// Client is one server and the device token for it ("" against `mlxlab serve`).
type Client struct {
	Server string
	Token  string
	HTTP   *http.Client

	// OnStream, if set, hears how the event streams fare (path: /api/events, /api/worker/events): nil when one
	// (re)connects, the error when it drops.
	OnStream func(path string, err error)

	retry time.Duration // the first wait before a reconnect (tests make it short)
	idle  time.Duration // a stream without a byte for this long is dead (the worker pings every 15 s)
}

// New makes a client for a server ("https://ai.example.org", "http://127.0.0.1:8484").
func New(server, token string) *Client {
	return &Client{Server: strings.TrimRight(server, "/"), Token: token, HTTP: &http.Client{}, retry: time.Second,
		idle: 45 * time.Second}
}

// Info is GET /api/info: who answers. The hub says role "hub" and whether the worker is connected; the worker
// (`mlxlab serve`) says nothing of a role.
type Info struct {
	App      string       `json:"app"`
	Role     string       `json:"role"`
	Name     string       `json:"name"`
	SignedIn bool         `json:"signedIn"`
	Worker   *WorkerState `json:"worker,omitempty"`
}

// Hub: the server signs people in and forwards to a worker.
func (i *Info) Hub() bool { return i.Role == "hub" }

// WorkerState is whether the Mac is connected to the hub.
type WorkerState struct {
	Connected bool   `json:"connected"`
	Name      string `json:"name,omitempty"`
	Since     string `json:"since,omitempty"`
}

// Catalog is GET /api/models.
type Catalog struct {
	Models   []*engine.Model                  `json:"models"`
	Loaded   map[string]string                `json:"loaded"` // backend → the model it has in memory
	Kinds    []engine.Kind                    `json:"kinds"`
	Examples map[engine.Kind][]engine.Example `json:"examples"`
}

// Device is one signed-in device of the hub.
type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Created  string `json:"created"`
	LastSeen string `json:"lastSeen"`
}

// Login signs in at the hub as user (who uses this device) and returns the new device token.
func Login(ctx context.Context, server, password, device, user string) (string, error) {
	c := New(server, "")
	var out struct {
		Token  string `json:"token"`
		Device Device `json:"device"`
	}
	err := c.do(ctx, http.MethodPost, "/api/login", map[string]string{"password": password, "device": device, "user": user}, &out)
	if errors.Is(err, ErrSignIn) || isStatus(err, http.StatusUnauthorized) {
		return "", ErrPassword
	}
	if err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("the server sent no token")
	}
	return out.Token, nil
}

func (c *Client) Info(ctx context.Context) (*Info, error) {
	var i Info
	return &i, c.do(ctx, http.MethodGet, "/api/info", nil, &i)
}

func (c *Client) Models(ctx context.Context) (*Catalog, error) {
	var cat Catalog
	return &cat, c.do(ctx, http.MethodGet, "/api/models", nil, &cat)
}

func (c *Client) Jobs(ctx context.Context) ([]*jobs.Job, error) {
	var js []*jobs.Job
	return js, c.do(ctx, http.MethodGet, "/api/jobs", nil, &js)
}

func (c *Client) Job(ctx context.Context, id string) (*jobs.Job, error) {
	var j jobs.Job
	return &j, c.do(ctx, http.MethodGet, "/api/jobs/"+url.PathEscape(id), nil, &j)
}

// Submit queues a request. Reference images (Images) must be /files/ URLs: Upload turns a local file into one.
func (c *Client) Submit(ctx context.Context, r engine.Request) (*jobs.Job, error) {
	var j jobs.Job
	return &j, c.do(ctx, http.MethodPost, "/api/jobs", r, &j)
}

func (c *Client) Cancel(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/jobs/"+url.PathEscape(id)+"/cancel", nil, nil)
}

func (c *Client) Retry(ctx context.Context, id string) (*jobs.Job, error) {
	var j jobs.Job
	return &j, c.do(ctx, http.MethodPost, "/api/jobs/"+url.PathEscape(id)+"/retry", nil, &j)
}

// Delete removes a job and the files it made.
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/jobs/"+url.PathEscape(id), nil, nil)
}

// Unload frees the models' memory on the Mac.
func (c *Client) Unload(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/unload", nil, nil)
}

// Upload sends a local file (a reference image) to the Mac and returns its /files/ URL.
func (c *Client) Upload(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := c.request(ctx, http.MethodPost, "/api/upload", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var out struct {
		URL string `json:"url"`
	}
	return out.URL, c.send(req, &out)
}

// FileURL is the full URL of an output (a /files/ path of a job), with the token in the query so a browser or a
// player that cannot send headers gets it too.
func (c *Client) FileURL(path string) string {
	u := c.Server + path
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		u = path
	}
	if c.Token == "" {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "token=" + url.QueryEscape(c.Token)
}

// ---------- requests ----------

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Server+path, body)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	// a plain call never takes long; only the event streams stay open
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.send(req, out)
}

func (c *Client) send(req *http.Request, out any) error {
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if err := check(res); err != nil {
		return err
	}
	if out == nil || res.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// StatusError is an answer other than 2xx that is neither ErrSignIn nor ErrOffline.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string { return fmt.Sprintf("%d: %s", e.Code, e.Message) }

func isStatus(err error, code int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == code
}

// check turns a failed answer into an error: the hub's {"error":"signin"|"offline"} into ErrSignIn and ErrOffline,
// anything else (the worker answers with plain text) into a StatusError.
func check(res *http.Response) error {
	if res.StatusCode/100 == 2 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(b, &e)
	switch {
	case res.StatusCode == http.StatusUnauthorized && e.Error == "signin":
		return ErrSignIn
	case res.StatusCode == http.StatusServiceUnavailable && e.Error == "offline":
		return ErrOffline
	}
	msg := e.Message
	if msg == "" {
		msg = e.Error
	}
	if msg == "" {
		msg = strings.TrimSpace(string(b))
	}
	if msg == "" {
		msg = http.StatusText(res.StatusCode)
	}
	return &StatusError{Code: res.StatusCode, Message: msg}
}
