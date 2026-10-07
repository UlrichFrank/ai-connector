// Command egress-proxy is the only way out of mlxlab's web sandbox: an HTTP forward proxy (absolute-form GET/HEAD
// and CONNECT) that resolves names itself, refuses every destination that is not on the public internet, dials
// exactly the address it checked (no DNS rebinding), allows only ports 80 and 443, caps size and time and logs
// every decision as a JSON line. It holds the Brave Search API key (if any): a plain-HTTP request for
// http://api.search.brave.com/res/v1/... is sent on over HTTPS with the key added, so the key never enters the
// tools VM.
//
// Configuration (environment):
//
//	LISTEN            listen address (default :3128)
//	ALLOW_CLIENTS     comma separated CIDRs that may use the proxy (default: any)
//	DENY              extra deny list: CIDRs, IPs, name suffixes (comma separated)
//	DNS_SERVER        resolvers to use instead of /etc/resolv.conf (host:port, comma separated; public ones: the
//	                  lockdown blackholes the gateway, i.e. the Mac's DNS forwarder)
//	MAX_BYTES         per request / tunnel cap in bytes (default 32 MiB)
//	MAX_SECONDS       per request / tunnel time cap (default 90)
//	BRAVE_KEY         the Brave Search API key (optional)
//
// "egress-proxy probe tcp host:port" tries a direct TCP connection (for the isolation check) and exits 0 if it
// connected.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const braveHost = "api.search.brave.com"

type proxy struct {
	policy   *Policy
	clients  []netip.Prefix
	maxBytes int64
	maxTime  time.Duration
	braveKey string
	log      *slog.Logger
	tr       *http.Transport
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "probe" {
		os.Exit(probe(os.Args[2:]))
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	p, err := newProxy(log)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(2)
	}
	addr := envOr("LISTEN", ":3128")
	srv := &http.Server{
		Addr:              addr,
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	log.Info("listening", "addr", addr, "brave", p.braveKey != "", "deny", os.Getenv("DENY"), "clients", os.Getenv("ALLOW_CLIENTS"))
	if err := srv.ListenAndServe(); err != nil {
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

func newProxy(log *slog.Logger) (*proxy, error) {
	pol := &Policy{Ports: map[int]bool{80: true, 443: true}}
	if err := pol.ParseDeny(os.Getenv("DENY")); err != nil {
		return nil, err
	}
	// the proxy's own addresses are never a destination
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if pfx, err := netip.ParsePrefix(a.String()); err == nil {
				pol.Nets = append(pol.Nets, pfx.Masked())
			}
		}
	}
	if dns := os.Getenv("DNS_SERVER"); dns != "" {
		servers := strings.Split(dns, ",")
		var next atomic.Uint32
		d := net.Dialer{Timeout: 4 * time.Second}
		pol.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			// round robin over the servers (the resolver retries, so a dead one is skipped)
			s := strings.TrimSpace(servers[int(next.Add(1))%len(servers)])
			return d.DialContext(ctx, network, s)
		}}
	}
	p := &proxy{policy: pol, log: log, braveKey: strings.TrimSpace(os.Getenv("BRAVE_KEY"))}
	for _, c := range strings.Split(os.Getenv("ALLOW_CLIENTS"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			pfx, err := netip.ParsePrefix(c)
			if err != nil {
				return nil, fmt.Errorf("ALLOW_CLIENTS: %w", err)
			}
			p.clients = append(p.clients, pfx.Masked())
		}
	}
	p.maxBytes = 32 << 20
	if v, err := strconv.ParseInt(os.Getenv("MAX_BYTES"), 10, 64); err == nil && v > 0 {
		p.maxBytes = v
	}
	p.maxTime = 90 * time.Second
	if v, err := strconv.Atoi(os.Getenv("MAX_SECONDS")); err == nil && v > 0 {
		p.maxTime = time.Duration(v) * time.Second
	}
	p.tr = &http.Transport{
		Proxy:                  nil,
		DialContext:            p.dial,
		TLSHandshakeTimeout:    15 * time.Second,
		ResponseHeaderTimeout:  30 * time.Second,
		MaxIdleConns:           16,
		IdleConnTimeout:        30 * time.Second,
		DisableCompression:     true, // pass the client's Accept-Encoding and the encoded body through
		MaxResponseHeaderBytes: 64 << 10,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return p, nil
}

// dial connects to host:port after the policy allowed the port and every address of the host; it connects to
// the checked address itself, and checks again right before the connect (Control).
func (p *proxy) dial(ctx context.Context, network, hostport string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, denied("bad address %q", hostport)
	}
	n, err := p.policy.CheckPort(port)
	if err != nil {
		return nil, err
	}
	addrs, err := p.policy.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return denied("bad address %q", address)
			}
			return p.policy.CheckIP(ap.Addr())
		},
	}
	var last error
	for _, a := range addrs {
		c, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(a, n).String())
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

func (p *proxy) clientAllowed(remote string) bool {
	if len(p.clients) == 0 {
		return true
	}
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return false
	}
	for _, c := range p.clients {
		if c.Contains(ap.Addr().Unmap()) {
			return true
		}
	}
	return false
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if !p.clientAllowed(r.RemoteAddr) {
		p.log.Warn("deny", "client", r.RemoteAddr, "method", r.Method, "target", r.Host, "reason", "client not allowed")
		http.Error(w, "client not allowed", http.StatusForbidden)
		return
	}
	switch {
	case r.Method == http.MethodConnect:
		p.connect(w, r, start)
	case r.URL.IsAbs():
		p.forward(w, r, start)
	case r.URL.Path == "/healthz":
		_, _ = io.WriteString(w, "ok\n")
	default:
		http.Error(w, "this is a forward proxy", http.StatusBadRequest)
	}
}

func (p *proxy) decision(err error, r *http.Request, start time.Time, n int64, extra ...any) {
	args := []any{"client", r.RemoteAddr, "method", r.Method, "target", r.Host, "ms", time.Since(start).Milliseconds()}
	if r.Method != http.MethodConnect {
		args = append(args, "url", r.URL.Redacted())
	}
	args = append(args, extra...)
	switch {
	case err == nil:
		p.log.Info("allow", append(args, "bytes", n)...)
	case isDenied(err):
		p.log.Warn("deny", append(args, "reason", err.Error())...)
	default:
		p.log.Warn("error", append(args, "bytes", n, "err", err.Error())...)
	}
}

func (p *proxy) connect(w http.ResponseWriter, r *http.Request, start time.Time) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	up, err := p.dial(ctx, "tcp", r.Host)
	cancel()
	if err != nil {
		p.decision(err, r, start, 0)
		code := http.StatusBadGateway
		if isDenied(err) {
			code = http.StatusForbidden
		}
		http.Error(w, err.Error(), code)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	down, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	deadline := time.Now().Add(p.maxTime)
	_ = up.SetDeadline(deadline)
	_ = down.SetDeadline(deadline)
	var total atomic.Int64
	done := make(chan struct{}, 2)
	pipe := func(dst io.Writer, src io.Reader) {
		_, _ = io.Copy(dst, &capReader{r: src, total: &total, max: p.maxBytes})
		_ = up.Close()
		_ = down.Close()
		done <- struct{}{}
	}
	// bytes the client sent after the CONNECT line, already buffered
	var src io.Reader = down
	if n := buf.Reader.Buffered(); n > 0 {
		pre, _ := buf.Reader.Peek(n)
		src = io.MultiReader(strings.NewReader(string(pre)), down)
	}
	go pipe(up, src)
	go pipe(down, up)
	<-done
	<-done
	p.decision(nil, r, start, total.Load(), "remote", up.RemoteAddr().String())
}

// capReader stops a tunnel (both directions share total) when it has carried max bytes.
type capReader struct {
	r     io.Reader
	total *atomic.Int64
	max   int64
}

func (c *capReader) Read(b []byte) (int, error) {
	if c.total.Load() >= c.max {
		return 0, errors.New("size cap reached")
	}
	n, err := c.r.Read(b)
	c.total.Add(int64(n))
	return n, err
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *proxy) forward(w http.ResponseWriter, r *http.Request, start time.Time) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		err := denied("method %s is not allowed", r.Method)
		p.decision(err, r, start, 0)
		http.Error(w, err.Error(), http.StatusMethodNotAllowed)
		return
	}
	u := *r.URL
	if u.Scheme != "http" {
		err := denied("scheme %q (use CONNECT for https)", u.Scheme)
		p.decision(err, r, start, 0)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if u.User != nil {
		err := denied("credentials in the URL")
		p.decision(err, r, start, 0)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.maxTime)
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, r.Method, u.String(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for k, vv := range r.Header {
		out.Header[k] = vv
	}
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	out.Header.Del("X-Subscription-Token")
	brave := false
	if NormalizeHost(u.Hostname()) == braveHost && p.braveKey != "" {
		if !strings.HasPrefix(u.Path, "/res/v1/") || (u.Port() != "" && u.Port() != "80") {
			err := denied("only /res/v1/ of the Brave API")
			p.decision(err, r, start, 0)
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		out.URL.Scheme, out.URL.Host = "https", braveHost
		out.Host = braveHost
		out.Header.Set("X-Subscription-Token", p.braveKey)
		out.Header.Set("Accept", "application/json")
		brave = true
	}
	resp, err := p.tr.RoundTrip(out)
	if err != nil {
		p.decision(err, r, start, 0)
		code := http.StatusBadGateway
		if isDenied(err) {
			code = http.StatusForbidden
		}
		http.Error(w, err.Error(), code)
		return
	}
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vv := range resp.Header {
		w.Header()[k] = vv
	}
	w.WriteHeader(resp.StatusCode)
	n, err := io.Copy(w, io.LimitReader(resp.Body, p.maxBytes))
	p.decision(err, r, start, n, "status", resp.StatusCode, "brave", brave)
}

// probe: "tcp host:port" — a direct connection, as the isolation check runs it.
func probe(args []string) int {
	if len(args) != 2 || args[0] != "tcp" {
		fmt.Fprintln(os.Stderr, "usage: egress-proxy probe tcp host:port")
		return 2
	}
	c, err := net.DialTimeout("tcp", args[1], 4*time.Second)
	if err != nil {
		fmt.Println("FAIL", err)
		return 1
	}
	c.Close()
	fmt.Println("OK connected")
	return 0
}
