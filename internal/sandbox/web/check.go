package web

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Probe is one connectivity test of the isolation check.
type Probe struct {
	Group string // where it runs: "tools", "proxy", "searxng", "control"
	What  string
	Want  bool // true: must work; false: must fail
	// Advisory: a probe whose failure is reported but does not fail the check (the control group: without pf the
	// Mac is reachable from a VM whose in-VM lockdown is gone).
	Advisory bool
	Got      bool
	Detail   string
}

// Pass says the probe came out as required.
func (p Probe) Pass() bool { return p.Got == p.Want }

func (p Probe) String() string {
	verdict := "PASS"
	if !p.Pass() {
		verdict = "FAIL"
		if p.Advisory {
			verdict = "WARN"
		}
	}
	want, got := "blocked", "blocked"
	if p.Want {
		want = "reachable"
	}
	if p.Got {
		got = "reachable"
	}
	return fmt.Sprintf("%s  [%s] %-58s want %-9s got %-9s %s", verdict, p.Group, p.What, want, got, p.Detail)
}

// Check proves the isolation from inside the running sandbox (it starts it first). Every probe runs as the
// unprivileged user the services run as, i.e. what a compromised service could do. A canary port opened on the
// Mac (all interfaces) tells whether the Mac is reachable; the "control" group runs the same probes in a fresh
// VM on the isolated network without the in-VM lockdown, so it shows what only the network (and pf, when
// loaded) blocks.
func (s *Sandbox) Check(ctx context.Context) ([]Probe, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock() // no idle stop while checking
	defer s.mu.Unlock()

	canary, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}
	defer canary.Close()
	go func() {
		for {
			c, err := canary.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	cport := strconv.Itoa(canary.Addr().(*net.TCPAddr).Port)

	nets, err := s.cli.networks(ctx)
	if err != nil {
		return nil, err
	}
	var intGW, extGW string
	for _, n := range nets {
		switch n.ID {
		case s.cfg.name("int"):
			intGW = n.Status.IPv4Gateway
		case s.cfg.name("ext"):
			extGW = n.Status.IPv4Gateway
		}
	}
	macIPs := hostIPv4s()
	router := defaultRouter()
	proxy, _ := s.cli.inspect(ctx, s.cfg.name("proxy"))
	proxyIP, _ := proxy.ipOn(s.cfg.name("int"))
	_, sxUp := s.cli.inspect(ctx, s.cfg.name("searxng"))

	// Mac addresses with the canary port: the bridge gateways and every address of the Mac
	macTargets := []string{intGW, extGW}
	for _, ip := range macIPs {
		if ip != intGW && ip != extGW {
			macTargets = append(macTargets, ip)
		}
	}

	var probes []Probe
	add := func(group, what string, want bool) {
		probes = append(probes, Probe{Group: group, What: what, Want: want})
	}

	// the Mac's IPv6 link-local addresses on the two bridges (services listening on [::] answer there)
	intLL, extLL := linkLocalOf(intGW), linkLocalOf(extGW)
	ll := func(addr, dev string) string {
		return "tcp [" + addr + "%" + dev + "]:" + cport + " (Mac canary, IPv6)"
	}

	// tools VM: direct connections (all must fail)
	for _, ip := range macTargets {
		add("tools", "tcp "+ip+":"+cport+" (Mac canary)", false)
	}
	if intLL != "" {
		add("tools", ll(intLL, "eth0"), false)
		if sxUp {
			add("searxng", ll(intLL, "eth0"), false)
		}
		add("proxy", ll(intLL, "eth1"), false)
		add("control", ll(intLL, "eth0"), false)
		probes[len(probes)-1].Advisory = true
	}
	if extLL != "" {
		add("proxy", ll(extLL, "eth0"), false)
	}
	add("tools", "tcp "+intGW+":8484 (Mac, mlxlab port)", false)
	add("tools", "tcp 127.0.0.1:8484 (localhost:8484)", false)
	for _, t := range []string{"192.168.0.1:80", "192.168.1.1:80", "10.0.0.1:80", "100.64.0.1:80", "169.254.169.254:80"} {
		add("tools", "tcp "+t, false)
	}
	if router != "" {
		add("tools", "tcp "+router+":80 (LAN router)", false)
		add("tools", "tcp "+router+":53 (LAN router)", false)
	}
	add("tools", "tcp 1.1.1.1:443 (internet, direct)", false)
	add("tools", "tcp 8.8.8.8:53 (internet DNS, direct)", false)
	add("tools", "dns example.com (direct)", false)
	add("tools", "tcp example.org:443 (direct)", false)
	// tools VM through the proxy
	via := []struct {
		url  string
		want bool
	}{
		{"http://192.168.0.1/", false},
		{"http://10.0.0.1/", false},
		{"http://100.64.0.1/", false},
		{"http://169.254.169.254/latest/meta-data/", false},
		{"http://127.0.0.1/", false},
		{"http://localhost:8484/", false},
		{"http://[::1]/", false},
		{"http://2130706433/", false},
		{"http://0x7f000001/", false},
		{"https://example.org/", false},
		{"https://www.example.org/", false},
		{"https://192.168.1.1.nip.io/", false},
		{"http://192.168.1.1.nip.io/", false},
		{"http://10.0.0.1.nip.io/", false},
		{"http://" + intGW + "/", false},
		{"http://" + extGW + "/", false},
		{"https://example.com:8443/", false},
		{"http://example.com/", true},
		{"https://example.com/", true},
	}
	for _, ip := range macIPs {
		via = append(via, struct {
			url  string
			want bool
		}{"http://" + ip + "/", false})
	}
	for _, v := range via {
		add("tools", "via proxy GET "+v.url, v.want)
	}

	// proxy VM (as the proxy's user): no LAN, no Mac; internet on 443
	for _, ip := range macTargets {
		add("proxy", "tcp "+ip+":"+cport+" (Mac canary)", false)
	}
	if router != "" {
		add("proxy", "tcp "+router+":80 (LAN router)", false)
	}
	add("proxy", "tcp "+extGW+":53 (the Mac's DNS forwarder)", false)
	add("proxy", "tcp 192.168.0.1:80", false)
	add("proxy", "tcp 10.0.0.1:80", false)
	add("proxy", "tcp 172.16.0.1:80", false)
	add("proxy", "tcp 100.64.0.1:80", false)
	add("proxy", "tcp 169.254.169.254:80", false)
	add("proxy", "tcp 1.1.1.1:443 (internet)", true)

	// the services run unprivileged, without capabilities, without IPv6
	add("tools", "caps webtools", true)
	add("tools", "ipv6off", true)
	add("proxy", "caps egress-proxy", true)
	add("proxy", "ipv6off", true)

	// SearXNG VM (as searxng)
	if sxUp {
		for _, ip := range macTargets {
			add("searxng", "tcp "+ip+":"+cport+" (Mac canary)", false)
		}
		add("searxng", "tcp 1.1.1.1:443 (internet, direct)", false)
		add("searxng", "tcp 192.168.0.1:80", false)
		if proxyIP.IsValid() {
			add("searxng", "tcp "+proxyIP.String()+":3128 (the proxy)", true)
		}
		add("searxng", "caps granian searxng", true)
		add("searxng", "ipv6off", true)
	}

	// control: a fresh VM on the isolated network without lockdown (only the network and pf)
	for _, ip := range macTargets {
		add("control", "tcp "+ip+":"+cport+" (Mac canary; blocked only by pf)", false)
		probes[len(probes)-1].Advisory = true
	}
	add("control", "tcp 1.1.1.1:443 (internet, direct)", false)
	if router != "" {
		add("control", "tcp "+router+":80 (LAN router)", false)
	}

	// run them, a few at a time
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	control := s.cfg.name("check")
	ctrlUp := false
	if _, err := s.cli.run(ctx, "run", "-d", "--rm", "--name", control, "-l", label, "--network", s.cfg.name("int"),
		"--no-dns", "-c", "1", "-m", "256M", "--entrypoint", "/bin/sleep", s.images()["tools"], "300"); err == nil {
		ctrlUp = true
		defer func() { _, _ = s.cli.run(context.Background(), "delete", "--force", control) }()
	}
	for i := range probes {
		p := &probes[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if p.Group == "control" && !ctrlUp {
				p.Got, p.Detail = false, "control VM did not start"
				return
			}
			p.Got, p.Detail = s.runProbe(ctx, p.Group, p.What)
		}()
	}
	wg.Wait()
	return probes, nil
}

// runProbe runs one probe in the container of its group, as the unprivileged service user.
func (s *Sandbox) runProbe(ctx context.Context, group, what string) (bool, string) {
	// what → probe arguments: "tcp h:p (…)", "dns name (…)", "via proxy GET url", "caps NAME…", "ipv6off"
	f := strings.Fields(what)
	var pargs []string
	switch f[0] {
	case "via":
		pargs = []string{"get", f[3]}
	case "caps":
		pargs = f
	case "ipv6off":
		pargs = f[:1]
	default:
		pargs = f[:2]
	}
	user, container, bin := "65534", s.cfg.name(group), "/usr/local/bin/lockdown"
	switch {
	case group == "searxng":
		user, bin = "searxng", "/opt/mlxlab/lockdown"
	case group == "control":
		container = s.cfg.name("check")
		bin = "/usr/local/bin/webtools"
	case group == "tools" && (f[0] == "via" || f[0] == "dns" || f[0] == "tcp"):
		bin = "/usr/local/bin/webtools"
	}
	args := append([]string{"exec", "--user", user, container, bin, "probe"}, pargs...)
	cmd := exec.CommandContext(ctx, s.cfg.CLI, args...)
	out, err := cmd.CombinedOutput()
	detail := strings.TrimSpace(string(out))
	if len(detail) > 160 {
		detail = detail[:160] + "…"
	}
	return err == nil, detail
}

// hostIPv4s are the Mac's own IPv4 addresses (not loopback).
func hostIPv4s() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		p, err := netip.ParsePrefix(a.String())
		if err != nil || !p.Addr().Is4() || p.Addr().IsLoopback() || p.Addr().IsLinkLocalUnicast() {
			continue
		}
		out = append(out, p.Addr().String())
	}
	return out
}

// linkLocalOf is the IPv6 link-local address of the Mac's interface that has the IPv4 address ip (a vmnet bridge).
func linkLocalOf(ip string) string {
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		addrs, _ := ifc.Addrs()
		has, ll := false, ""
		for _, a := range addrs {
			p, err := netip.ParsePrefix(a.String())
			if err != nil {
				continue
			}
			if p.Addr().String() == ip {
				has = true
			}
			if p.Addr().Is6() && p.Addr().IsLinkLocalUnicast() {
				ll = p.Addr().WithZone("").String()
			}
		}
		if has && ll != "" {
			return ll
		}
	}
	return ""
}

// defaultRouter is the Mac's default IPv4 gateway (the LAN router), if any.
func defaultRouter() string {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), ":"); ok && k == "gateway" {
			if ip, err := netip.ParseAddr(strings.TrimSpace(v)); err == nil && ip.Is4() {
				return ip.String()
			}
		}
	}
	return ""
}
