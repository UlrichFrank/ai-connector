// Command webtools is the tools service of mlxlab's web sandbox: search (Brave Search API through the egress
// proxy, which adds the key; else a SearXNG instance) and fetch (a page through the proxy, as readable text). It
// runs in a VM whose only route out is the proxy; it holds no secrets.
//
// Configuration (environment):
//
//	LISTEN        listen address (default :8080; "unix:/path" for a unix socket)
//	PROXY_URL     the egress proxy, e.g. http://192.168.240.3:3128 (required)
//	SEARCH        "brave" or "searxng" (default searxng)
//	SEARXNG_URL   the SearXNG instance, e.g. http://192.168.240.4:8080
//	MAX_FETCH     bytes read from a page (default 4 MiB)
//	MAX_TEXT      characters of text returned (default 60000)
//
// API: GET /health; POST /search {"query","count"} → {"results":[{title,url,snippet}]}; POST /fetch {"url"} →
// {"url","title","text","contentType","truncated"}. Errors: {"error"} with status 4xx/5xx.
//
// "webtools probe tcp host:port | dns name | get url" is for the isolation check: it exits 0 on success.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type service struct {
	search   string
	searxng  string
	viaProxy *http.Client // to the internet (only through the proxy)
	direct   *http.Client // to SearXNG (same isolated network)
	maxFetch int64
	maxText  int
	log      *slog.Logger
}

type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

type fetchResult struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Text        string `json:"text"`
	ContentType string `json:"contentType"`
	Truncated   bool   `json:"truncated,omitempty"`
}

const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "probe" {
		os.Exit(probe(os.Args[2:]))
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	s, err := newService(log)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(2)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /search", s.handleSearch)
	mux.HandleFunc("POST /fetch", s.handleFetch)
	addr := envOr("LISTEN", ":8080")
	network := "tcp"
	if p, ok := strings.CutPrefix(addr, "unix:"); ok {
		network, addr = "unix", p
		_ = os.Remove(addr)
	}
	l, err := net.Listen(network, addr)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	if network == "unix" {
		_ = os.Chmod(addr, 0o600) // the Mac reaches it through --publish-socket
	}
	log.Info("listening", "addr", addr, "search", s.search)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(l); err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func proxyClient(timeout time.Duration) (*http.Client, error) {
	p, err := url.Parse(os.Getenv("PROXY_URL"))
	if err != nil || p.Host == "" {
		return nil, fmt.Errorf("PROXY_URL %q: not a URL", os.Getenv("PROXY_URL"))
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyURL(p), // everything, regardless of NO_PROXY
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: checkRedirect}, nil
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	return checkURL(req.URL)
}

func newService(log *slog.Logger) (*service, error) {
	c, err := proxyClient(45 * time.Second)
	if err != nil {
		return nil, err
	}
	s := &service{
		search:   envOr("SEARCH", "searxng"),
		searxng:  strings.TrimRight(os.Getenv("SEARXNG_URL"), "/"),
		viaProxy: c,
		direct:   &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}},
		maxFetch: 4 << 20,
		maxText:  60000,
		log:      log,
	}
	if v, err := strconv.ParseInt(os.Getenv("MAX_FETCH"), 10, 64); err == nil && v > 0 {
		s.maxFetch = v
	}
	if v, err := strconv.Atoi(os.Getenv("MAX_TEXT")); err == nil && v > 0 {
		s.maxText = v
	}
	if s.search == "searxng" && s.searxng == "" {
		return nil, errors.New("SEARCH=searxng needs SEARXNG_URL")
	}
	return s, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *service) health(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"search": s.search}
	ok := s.ping(r.Context(), out, "proxy", strings.TrimRight(os.Getenv("PROXY_URL"), "/")+"/healthz")
	if s.search == "searxng" {
		ok = s.ping(r.Context(), out, "searxng", s.searxng+"/healthz") && ok
	}
	out["ok"] = ok
	code := http.StatusOK
	if !ok {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, out)
}

// ping GETs a health URL directly (no proxy) and records the outcome under key.
func (s *service) ping(ctx context.Context, out map[string]any, key, u string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := s.direct.Do(req)
	if err != nil {
		out[key] = err.Error()
		return false
	}
	resp.Body.Close()
	out[key] = resp.Status
	return resp.StatusCode == http.StatusOK
}

func (s *service) handleSearch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query string `json:"query"`
		Count int    `json:"count"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" || len(in.Query) > 400 {
		fail(w, http.StatusBadRequest, errors.New("query must be 1..400 characters"))
		return
	}
	if in.Count <= 0 {
		in.Count = 8
	}
	in.Count = min(in.Count, 20)
	var res []searchResult
	var err error
	if s.search == "brave" {
		res, err = s.searchBrave(r.Context(), in.Query, in.Count)
	} else {
		res, err = s.searchSearxng(r.Context(), in.Query, in.Count)
	}
	s.log.Info("search", "engine", s.search, "query", in.Query, "results", len(res), "err", errString(err))
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var tagRE = regexp.MustCompile(`<[^>]*>`)

func cleanSnippet(s string) string {
	return collapseSpace(htmlUnescape(tagRE.ReplaceAllString(s, "")))
}

func (s *service) getJSON(ctx context.Context, c *http.Client, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("search: %s: %s", resp.Status, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	return json.Unmarshal(body, v)
}

// searchBrave asks the Brave Search API over plain HTTP to the proxy, which sends it on over HTTPS and adds the key.
func (s *service) searchBrave(ctx context.Context, q string, n int) ([]searchResult, error) {
	u := "http://api.search.brave.com/res/v1/web/search?" + url.Values{"q": {q}, "count": {strconv.Itoa(n)}}.Encode()
	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := s.getJSON(ctx, s.viaProxy, u, &out); err != nil {
		return nil, err
	}
	res := []searchResult{}
	for _, r := range out.Web.Results {
		res = append(res, searchResult{Title: cleanSnippet(r.Title), URL: r.URL, Snippet: cleanSnippet(r.Description)})
	}
	return res[:min(len(res), n)], nil
}

func (s *service) searchSearxng(ctx context.Context, q string, n int) ([]searchResult, error) {
	u := s.searxng + "/search?" + url.Values{"q": {q}, "format": {"json"}, "safesearch": {"0"}}.Encode()
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := s.getJSON(ctx, s.direct, u, &out); err != nil {
		return nil, err
	}
	res := []searchResult{}
	seen := map[string]bool{}
	for _, r := range out.Results {
		if r.URL == "" || seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		res = append(res, searchResult{Title: cleanSnippet(r.Title), URL: r.URL, Snippet: cleanSnippet(r.Content)})
		if len(res) == n {
			break
		}
	}
	return res, nil
}

// checkURL allows only http and https URLs with a host and without credentials.
func checkURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http and https URLs (not %q)", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return errors.New("URL without a host")
	}
	if u.User != nil {
		return errors.New("URLs with credentials are not fetched")
	}
	return nil
}

func (s *service) handleFetch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.fetch(r.Context(), strings.TrimSpace(in.URL))
	s.log.Info("fetch", "url", in.URL, "bytes", len(res.Text), "err", errString(err))
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *service) fetch(ctx context.Context, raw string) (fetchResult, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return fetchResult{}, err
	}
	if err := checkURL(u); err != nil {
		return fetchResult{}, err
	}
	u.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fetchResult{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,application/json;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en,de;q=0.8")
	resp, err := s.viaProxy.Do(req)
	if err != nil {
		return fetchResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fetchResult{}, fmt.Errorf("%s: %s", resp.Request.URL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.maxFetch+1))
	if err != nil {
		return fetchResult{}, err
	}
	truncated := int64(len(body)) > s.maxFetch
	if truncated {
		body = body[:s.maxFetch]
	}
	ct, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if ct == "" {
		ct = http.DetectContentType(body)
		ct, params, _ = mime.ParseMediaType(ct)
	}
	text := decode(body, params["charset"])
	res := fetchResult{URL: resp.Request.URL.String(), ContentType: ct, Truncated: truncated}
	switch {
	case ct == "text/html" || ct == "application/xhtml+xml":
		res.Title, res.Text = HTMLToText(text)
	case strings.HasPrefix(ct, "text/") || ct == "application/json" || strings.HasSuffix(ct, "+json") ||
		ct == "application/xml" || strings.HasSuffix(ct, "+xml"):
		res.Text = strings.TrimSpace(text)
	default:
		return fetchResult{}, fmt.Errorf("%s: content type %q is not text", res.URL, ct)
	}
	if len(res.Text) > s.maxText {
		cut := s.maxText
		for cut > 0 && !utf8.RuneStart(res.Text[cut]) {
			cut--
		}
		res.Text, res.Truncated = res.Text[:cut], true
	}
	return res, nil
}

// decode makes UTF-8 of the body: UTF-8 as is (invalid bytes replaced), Latin-1/Windows-1252 byte by byte.
func decode(b []byte, charset string) string {
	switch strings.ToLower(charset) {
	case "iso-8859-1", "latin1", "latin-1", "windows-1252", "cp1252", "us-ascii":
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		return string(r)
	}
	return strings.ToValidUTF8(string(b), "�")
}

// probe runs one connectivity test for the isolation check; it prints OK or FAIL and exits 0 on success.
func probe(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: webtools probe tcp host:port | dns name | get url")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	var err error
	detail := ""
	switch args[0] {
	case "tcp":
		var c net.Conn
		c, err = (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, "tcp", args[1])
		if err == nil {
			c.Close()
		}
	case "dns":
		var addrs []string
		addrs, err = (&net.Resolver{PreferGo: true}).LookupHost(ctx, args[1])
		detail = strings.Join(addrs, ",")
	case "get":
		var c *http.Client
		if c, err = proxyClient(12 * time.Second); err == nil {
			c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			var resp *http.Response
			if resp, err = c.Get(args[1]); err == nil {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
				resp.Body.Close()
				detail = resp.Status
				if resp.StatusCode >= 400 {
					err = fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
				}
			}
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown probe", args[0])
		return 2
	}
	if err != nil {
		fmt.Println("FAIL", err)
		return 1
	}
	fmt.Println("OK", detail)
	return 0
}
