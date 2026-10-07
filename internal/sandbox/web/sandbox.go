package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	websandbox "github.com/philipparndt/ai-connector/sandbox/web"
)

const label = "mlxlab.sandbox=web"

// Sandbox is the web sandbox: its containers, their lifecycle, and the tools (see toolbox.go).
type Sandbox struct {
	cfg  Config
	cli  cli
	http *http.Client

	mu       sync.Mutex // serializes start/stop
	lastUse  time.Time
	running  bool
	idleStop chan struct{}
	closed   bool
}

// New makes the sandbox; nothing starts until the first call (or Ensure).
func New(cfg Config) *Sandbox {
	cfg.defaults()
	c := cli{bin: cfg.CLI}
	if cfg.BraveKey != "" {
		c.env = []string{"BRAVE_KEY=" + cfg.BraveKey}
	}
	sock := cfg.Socket
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Sandbox{cfg: cfg, cli: c, http: &http.Client{Timeout: 120 * time.Second, Transport: tr}}
}

func (s *Sandbox) baseURL() string { return "http://webtools" }

func (s *Sandbox) images() map[string]string {
	h := websandbox.Hash()
	m := map[string]string{}
	for _, im := range websandbox.Images {
		m[im.Name] = s.cfg.Prefix + "-" + im.Name + ":" + h
	}
	return m
}

func (s *Sandbox) search() string {
	if s.cfg.BraveKey != "" {
		return "brave"
	}
	return "searxng"
}

// healthy asks the tools service (which checks the proxy and SearXNG) whether all is up.
func (s *Sandbox) healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL()+"/health", nil)
	resp, err := s.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Ensure starts the sandbox if it is not running and healthy (building images first if needed), and restarts it
// if a part died.
func (s *Sandbox) Ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("web sandbox closed")
	}
	s.lastUse = time.Now()
	if s.healthy(ctx) {
		s.running = true
		s.startIdleWatch()
		return nil
	}
	s.cfg.Logf("web sandbox: starting")
	s.stopLocked(context.Background())
	if err := s.start(ctx); err != nil {
		s.stopLocked(context.Background())
		return fmt.Errorf("web sandbox: %w", err)
	}
	s.running = true
	s.startIdleWatch()
	return nil
}

func (s *Sandbox) startIdleWatch() {
	if s.idleStop != nil {
		return
	}
	stop := make(chan struct{})
	s.idleStop = stop
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.mu.Lock()
				if s.running && time.Since(s.lastUse) > s.cfg.Idle {
					s.cfg.Logf("web sandbox: idle, stopping")
					s.stopLocked(context.Background())
				}
				s.mu.Unlock()
			}
		}
	}()
}

func (s *Sandbox) start(ctx context.Context) error {
	if err := s.ensureNetworks(ctx); err != nil {
		return err
	}
	imgs := s.images()
	need := []string{"proxy", "tools"}
	if s.search() == "searxng" {
		need = append(need, "searxng")
	}
	if err := s.ensureImages(ctx, imgs, need); err != nil {
		return err
	}
	deny := strings.Trim(strings.Join([]string{s.cfg.IntSubnet, s.cfg.ExtSubnet, s.cfg.Deny}, ","), ",")

	// The proxy: NAT network first (its default route), then the isolated one.
	// For all three: CAP_NET_ADMIN and a writable /proc/sys ("--read-only-path NONE" drops the runtime's
	// read-only /proc/sys mount) are for the lockdown entrypoint (routes, IPv6 off); it drops every capability
	// and becomes an unprivileged user before the service starts, so neither is available to the service.
	args := []string{"run", "-d", "--rm", "--name", s.cfg.name("proxy"), "-l", label,
		"--network", s.cfg.name("ext"), "--network", s.cfg.name("int"), "--no-dns",
		"--cap-add", "CAP_NET_ADMIN", "--read-only-path", "NONE", "-c", "1", "-m", "512M",
		"-e", "ALLOW_CLIENTS=" + s.cfg.IntSubnet, "-e", "DENY=" + deny, "-e", "DNS_SERVER=" + s.cfg.DNS}
	if s.cfg.BraveKey != "" {
		args = append(args, "-e", "BRAVE_KEY") // inherited from our environment (cli.env), not on the command line
	}
	if _, err := s.cli.run(ctx, append(args, imgs["proxy"])...); err != nil {
		return err
	}
	proxy, ok := s.cli.inspect(ctx, s.cfg.name("proxy"))
	if !ok {
		return errors.New("proxy did not start")
	}
	proxyIP, err := proxy.ipOn(s.cfg.name("int"))
	if err != nil {
		return err
	}
	proxyURL := "http://" + proxyIP.String() + ":3128"

	env := []string{"-e", "PROXY_URL=" + proxyURL, "-e", "SEARCH=" + s.search()}
	if s.search() == "searxng" {
		if _, err := s.cli.run(ctx, "run", "-d", "--rm", "--name", s.cfg.name("searxng"), "-l", label,
			"--network", s.cfg.name("int"), "--no-dns", "--cap-add", "CAP_NET_ADMIN", "--read-only-path", "NONE", "-c", "2", "-m", "1G",
			"-e", "PROXY_URL="+proxyURL, imgs["searxng"]); err != nil {
			return err
		}
		sx, ok := s.cli.inspect(ctx, s.cfg.name("searxng"))
		if !ok {
			return errors.New("searxng did not start")
		}
		sxIP, err := sx.ipOn(s.cfg.name("int"))
		if err != nil {
			return err
		}
		env = append(env, "-e", "SEARXNG_URL=http://"+sxIP.String()+":8080")
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.Socket), 0o700); err != nil {
		return err
	}
	_ = os.Remove(s.cfg.Socket) // a stale socket makes the start fail ("address already in use")
	args = []string{"run", "-d", "--rm", "--name", s.cfg.name("tools"), "-l", label,
		"--network", s.cfg.name("int"), "--no-dns", "--cap-add", "CAP_NET_ADMIN", "--read-only-path", "NONE", "-c", "1", "-m", "512M",
		"--publish-socket", s.cfg.Socket + ":/run/webtools/tools.sock"}
	args = append(append(args, env...), imgs["tools"])
	if _, err := s.cli.run(ctx, args...); err != nil {
		return err
	}
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if s.healthy(ctx) {
			s.cfg.Logf("web sandbox: up (search: %s, proxy %s)", s.search(), proxyURL)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("the tools service did not become healthy in time (see `container logs " + s.cfg.name("tools") + "`)")
}

// ensureNetworks creates the two networks if missing and checks that existing ones are what we expect. Networks
// of other tools are never changed.
func (s *Sandbox) ensureNetworks(ctx context.Context) error {
	nets, err := s.cli.networks(ctx)
	if err != nil {
		return err
	}
	want := []struct {
		name, subnet, subnet6 string
		internal              bool
	}{
		{s.cfg.name("int"), s.cfg.IntSubnet, s.cfg.IntSubnetV6, true},
		{s.cfg.name("ext"), s.cfg.ExtSubnet, s.cfg.ExtSubnetV6, false},
	}
	for _, w := range want {
		pfx := netip.MustParsePrefix(w.subnet)
		var found *networkInfo
		for i, n := range nets {
			if n.ID == w.name {
				found = &nets[i]
				continue
			}
			if other, err := netip.ParsePrefix(n.Status.IPv4Subnet); err == nil && other.Overlaps(pfx) {
				return fmt.Errorf("subnet %s of %s overlaps network %s (%s); choose another subnet", w.subnet, w.name, n.ID, other)
			}
		}
		if found != nil {
			mode := found.Configuration.Mode
			if w.internal && mode != "hostOnly" || !w.internal && mode != "nat" {
				return fmt.Errorf("network %s has mode %q; delete it (container network rm %s) to recreate it", w.name, mode, w.name)
			}
			if found.Status.IPv4Subnet != w.subnet {
				return fmt.Errorf("network %s has subnet %s, want %s; delete it (container network rm %s)", w.name, found.Status.IPv4Subnet, w.subnet, w.name)
			}
			continue
		}
		args := []string{"network", "create", "--subnet", w.subnet, "--subnet-v6", w.subnet6, "--label", label}
		if w.internal {
			args = append(args, "--internal")
		}
		if _, err := s.cli.run(ctx, append(args, w.name)...); err != nil {
			return err
		}
		s.cfg.Logf("web sandbox: created network %s (%s)", w.name, w.subnet)
	}
	return nil
}

// ensureImages builds the images that are missing (tagged with the hash of their sources).
func (s *Sandbox) ensureImages(ctx context.Context, imgs map[string]string, need []string) error {
	var missing []string
	for _, n := range need {
		if !s.cli.imageExists(ctx, imgs[n]) {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	dir, err := os.MkdirTemp("", "mlxlab-websandbox-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := websandbox.WriteContext(dir); err != nil {
		return err
	}
	for _, n := range missing {
		for _, im := range websandbox.Images {
			if im.Name != n {
				continue
			}
			s.cfg.Logf("web sandbox: building image %s", imgs[n])
			if _, err := s.cli.run(ctx, "build", "--progress", "plain", "-t", imgs[n], "-f", dir+"/"+im.File, dir); err != nil {
				return err
			}
		}
	}
	s.removeStaleImages(ctx, imgs)
	return nil
}

// removeStaleImages deletes images of earlier versions of the sandbox (our names, another tag).
func (s *Sandbox) removeStaleImages(ctx context.Context, current map[string]string) {
	refs, err := s.cli.imageNames(ctx)
	if err != nil {
		return
	}
	keep := map[string]bool{}
	for _, ref := range current {
		keep[ref] = true
	}
	for _, ref := range refs {
		name, _, _ := strings.Cut(ref, ":")
		for _, im := range websandbox.Images {
			if name == s.cfg.Prefix+"-"+im.Name && !keep[ref] {
				s.cfg.Logf("web sandbox: removing old image %s", ref)
				_, _ = s.cli.run(ctx, "image", "delete", ref)
			}
		}
	}
}

// Stop stops the containers (they are removed on stop); the next call starts them again.
func (s *Sandbox) Stop(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked(ctx)
}

func (s *Sandbox) stopLocked(ctx context.Context) {
	for _, n := range []string{"tools", "searxng", "proxy"} {
		name := s.cfg.name(n)
		if _, ok := s.cli.inspect(ctx, name); !ok {
			continue
		}
		_, _ = s.cli.run(ctx, "stop", "-t", "3", name)
		_, _ = s.cli.run(ctx, "delete", "--force", name)
	}
	s.running = false
}

// Close stops the sandbox and its idle watch.
func (s *Sandbox) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleStop != nil {
		close(s.idleStop)
		s.idleStop = nil
	}
	s.stopLocked(context.Background())
	s.closed = true
	return nil
}
