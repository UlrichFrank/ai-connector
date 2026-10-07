// Command lockdown is the entrypoint of every web sandbox VM. It runs as root, closes the VM's network down to
// what its role needs (routes, via netlink; no packages needed in the image), then drops to an unprivileged user
// with no capabilities at all (empty bounding set, no_new_privs) and execs the service. A compromised service
// can therefore not undo the lockdown; a compromised VM kernel could — that is what the pf anchor on the Mac is
// for (sandbox/web/pf.conf).
//
//	lockdown tools   -- CMD...   no default route; the gateway (the Mac) blackholed; only the isolated subnet
//	                             (the proxy, SearXNG) is reachable. The Mac talks to it via a vsock socket.
//	lockdown searxng -- CMD...   the same; writes SearXNG's settings (PROXY_URL) first
//	lockdown proxy   -- CMD...   default route kept (internet), but every private / CGNAT / link-local / multicast
//	                             range and both gateways (the Mac) blackholed; DNS must be a public server
//
// All modes: IPv6 off, no forwarding.
package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func init() { runtime.LockOSThread() } // prctl is per thread: keep main on one thread until exec

func say(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "lockdown: "+format+"\n", a...)
}

func die(format string, a ...any) {
	say(format, a...)
	os.Exit(1)
}

// blackholed in the proxy VM (besides both gateways): everything that is not the public internet
var proxyBlackholes = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24",
	"192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"224.0.0.0/4", "240.0.0.0/4",
}

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "probe" {
		os.Exit(probe(os.Args[2:]))
	}
	if len(os.Args) < 4 || os.Args[2] != "--" {
		die("usage: lockdown tools|searxng|proxy -- CMD [ARGS...]")
	}
	mode, cmd := os.Args[1], os.Args[3:]
	if os.Getuid() != 0 {
		die("must start as root")
	}
	sysctls()
	routes, err := listRoutes()
	if err != nil {
		die("routes: %v", err)
	}
	var gateways []netip.Addr
	for _, r := range routes {
		if r.gw.IsValid() {
			gateways = append(gateways, r.gw)
		}
	}
	user := "nobody"
	switch mode {
	case "tools", "searxng":
		for _, r := range routes {
			if r.dstLen == 0 {
				if err := delRoute(r); err != nil {
					die("delete default route: %v", err)
				}
			}
		}
		for _, gw := range gateways {
			if err := addBlackhole(netip.PrefixFrom(gw, 32)); err != nil {
				die("blackhole %s: %v", gw, err)
			}
		}
		if mode == "tools" {
			if err := os.MkdirAll("/run/webtools", 0o755); err != nil {
				die("%v", err)
			}
			_ = os.Chown("/run/webtools", 65534, 65534)
			_ = os.Remove("/run/webtools/tools.sock")
		} else {
			user = "searxng"
			if err := searxngSettings(user); err != nil {
				die("searxng settings: %v", err)
			}
		}
		say("%s: default route removed, gateway(s) %v blackholed", mode, gateways)
	case "proxy":
		for _, gw := range gateways {
			if err := addBlackhole(netip.PrefixFrom(gw, 32)); err != nil {
				die("blackhole %s: %v", gw, err)
			}
		}
		// the gateway of the isolated network is on-link only (no route has it as gateway): blackhole every
		// interface network's first address too, which is where vmnet puts the Mac
		for _, r := range routes {
			if r.dstLen > 0 && !r.gw.IsValid() && r.dst.Is4() {
				first := netip.PrefixFrom(r.dst, int(r.dstLen)).Masked().Addr().Next()
				if err := addBlackhole(netip.PrefixFrom(first, 32)); err != nil {
					die("blackhole %s: %v", first, err)
				}
			}
		}
		for _, s := range proxyBlackholes {
			if err := addBlackhole(netip.MustParsePrefix(s)); err != nil {
				die("blackhole %s: %v", s, err)
			}
		}
		say("proxy: private, CGNAT, link-local, multicast and the gateways blackholed; default route kept")
	default:
		die("unknown mode %q", mode)
	}
	if after, err := listRoutes(); err == nil {
		for _, r := range after {
			say("route %s", r)
		}
	}
	uid, gid, err := lookupUser(user)
	if err != nil {
		die("user %s: %v", user, err)
	}
	if err := dropPrivileges(uid, gid); err != nil {
		die("drop privileges: %v", err)
	}
	path := cmd[0]
	env := append(os.Environ(), "HOME=/tmp")
	if err := syscall.Exec(path, cmd, env); err != nil {
		die("exec %s: %v", path, err)
	}
}

func sysctls() {
	write := func(p, v string) {
		if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
			say("sysctl %s: %v", p, err)
		}
	}
	files, _ := filepath.Glob("/proc/sys/net/ipv6/conf/*/disable_ipv6")
	for _, f := range files {
		write(f, "1")
	}
	write("/proc/sys/net/ipv4/ip_forward", "0")
	write("/proc/sys/net/ipv4/conf/all/accept_redirects", "0")
}

// ---------- routes (rtnetlink) ----------

type route struct {
	dst    netip.Addr
	dstLen uint8
	gw     netip.Addr
	oif    uint32
	table  uint8
	typ    uint8
	proto  uint8
	scope  uint8
}

func (r route) String() string {
	s := "default"
	if r.dstLen > 0 {
		s = netip.PrefixFrom(r.dst, int(r.dstLen)).String()
	}
	if r.typ == syscall.RTN_BLACKHOLE {
		return "blackhole " + s
	}
	if r.gw.IsValid() {
		s += " via " + r.gw.String()
	}
	return s + " dev#" + strconv.Itoa(int(r.oif))
}

func listRoutes() ([]route, error) {
	b, err := syscall.NetlinkRIB(syscall.RTM_GETROUTE, syscall.AF_INET)
	if err != nil {
		return nil, err
	}
	msgs, err := syscall.ParseNetlinkMessage(b)
	if err != nil {
		return nil, err
	}
	var out []route
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWROUTE || len(m.Data) < syscall.SizeofRtMsg {
			continue
		}
		rt := (*syscall.RtMsg)(unsafe.Pointer(&m.Data[0]))
		if rt.Table != syscall.RT_TABLE_MAIN {
			continue
		}
		r := route{dstLen: rt.Dst_len, table: rt.Table, typ: rt.Type, proto: rt.Protocol, scope: rt.Scope,
			dst: netip.IPv4Unspecified()}
		attrs, err := syscall.ParseNetlinkRouteAttr(&m)
		if err != nil {
			return nil, err
		}
		for _, a := range attrs {
			switch a.Attr.Type {
			case syscall.RTA_DST:
				if ip, ok := netip.AddrFromSlice(a.Value); ok {
					r.dst = ip
				}
			case syscall.RTA_GATEWAY:
				if ip, ok := netip.AddrFromSlice(a.Value); ok {
					r.gw = ip
				}
			case syscall.RTA_OIF:
				if len(a.Value) == 4 {
					r.oif = binary.NativeEndian.Uint32(a.Value)
				}
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func attr(typ uint16, val []byte) []byte {
	l := 4 + len(val)
	b := make([]byte, (l+3)&^3)
	binary.NativeEndian.PutUint16(b[0:], uint16(l))
	binary.NativeEndian.PutUint16(b[2:], typ)
	copy(b[4:], val)
	return b
}

func rtnl(typ uint16, flags uint16, rt syscall.RtMsg, attrs ...[]byte) error {
	body := (*[syscall.SizeofRtMsg]byte)(unsafe.Pointer(&rt))[:]
	payload := append([]byte{}, body...)
	for _, a := range attrs {
		payload = append(payload, a...)
	}
	msg := make([]byte, syscall.NLMSG_HDRLEN+len(payload))
	binary.NativeEndian.PutUint32(msg[0:], uint32(len(msg)))
	binary.NativeEndian.PutUint16(msg[4:], typ)
	binary.NativeEndian.PutUint16(msg[6:], flags|syscall.NLM_F_REQUEST|syscall.NLM_F_ACK)
	binary.NativeEndian.PutUint32(msg[8:], 1)
	copy(msg[syscall.NLMSG_HDRLEN:], payload)

	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	sa := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Bind(fd, sa); err != nil {
		return err
	}
	if err := syscall.Sendto(fd, msg, 0, sa); err != nil {
		return err
	}
	buf := make([]byte, 8192)
	n, _, err := syscall.Recvfrom(fd, buf, 0)
	if err != nil {
		return err
	}
	msgs, err := syscall.ParseNetlinkMessage(buf[:n])
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if m.Header.Type == syscall.NLMSG_ERROR && len(m.Data) >= 4 {
			if code := int32(binary.NativeEndian.Uint32(m.Data[0:4])); code != 0 {
				return syscall.Errno(-code)
			}
			return nil
		}
	}
	return errors.New("no netlink ack")
}

func delRoute(r route) error {
	rt := syscall.RtMsg{Family: syscall.AF_INET, Dst_len: r.dstLen, Table: r.table, Protocol: r.proto,
		Scope: r.scope, Type: r.typ}
	var attrs [][]byte
	if r.dstLen > 0 {
		d := r.dst.As4()
		attrs = append(attrs, attr(syscall.RTA_DST, d[:]))
	}
	if r.gw.IsValid() {
		g := r.gw.As4()
		attrs = append(attrs, attr(syscall.RTA_GATEWAY, g[:]))
	}
	if r.oif != 0 {
		o := make([]byte, 4)
		binary.NativeEndian.PutUint32(o, r.oif)
		attrs = append(attrs, attr(syscall.RTA_OIF, o))
	}
	return rtnl(syscall.RTM_DELROUTE, 0, rt, attrs...)
}

func addBlackhole(p netip.Prefix) error {
	p = p.Masked()
	rt := syscall.RtMsg{Family: syscall.AF_INET, Dst_len: uint8(p.Bits()), Table: syscall.RT_TABLE_MAIN,
		Protocol: syscall.RTPROT_STATIC, Scope: syscall.RT_SCOPE_UNIVERSE, Type: syscall.RTN_BLACKHOLE}
	d := p.Addr().As4()
	err := rtnl(syscall.RTM_NEWROUTE, syscall.NLM_F_CREATE|syscall.NLM_F_EXCL, rt, attr(syscall.RTA_DST, d[:]))
	if errors.Is(err, syscall.EEXIST) {
		return nil
	}
	return err
}

// ---------- privileges ----------

const (
	prCapbsetDrop      = 24
	prSetNoNewPrivs    = 38
	prCapAmbient       = 47
	prCapAmbientClrAll = 4
)

func prctl(op, arg uintptr) syscall.Errno {
	_, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, op, arg, 0, 0, 0, 0)
	return e
}

// dropPrivileges empties the capability bounding set, sets no_new_privs, and becomes uid:gid (which clears the
// permitted and effective capabilities); then it checks that no capability is left.
func dropPrivileges(uid, gid int) error {
	last := 63
	if b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			last = n
		}
	}
	for c := 0; c <= last; c++ {
		if e := prctl(prCapbsetDrop, uintptr(c)); e != 0 && e != syscall.EINVAL {
			return fmt.Errorf("drop cap %d from the bounding set: %v", c, e)
		}
	}
	_ = prctl(prCapAmbient, prCapAmbientClrAll)
	if e := prctl(prSetNoNewPrivs, 1); e != 0 {
		return fmt.Errorf("no_new_privs: %v", e)
	}
	if err := syscall.Setgroups(nil); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setresgid(gid, gid, gid); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setresuid(uid, uid, uid); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	st, err := os.ReadFile("/proc/thread-self/status")
	if err != nil {
		return err
	}
	for _, l := range strings.Split(string(st), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		switch k {
		case "CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb":
			if n, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64); err != nil || n != 0 {
				return fmt.Errorf("%s is %s after the drop", k, strings.TrimSpace(v))
			}
		case "NoNewPrivs":
			if strings.TrimSpace(v) != "1" {
				return errors.New("no_new_privs not set")
			}
		}
	}
	return nil
}

func lookupUser(name string) (int, int, error) {
	if name == "nobody" {
		return 65534, 65534, nil
	}
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return 0, 0, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, ":")
		if len(f) >= 4 && f[0] == name {
			uid, err1 := strconv.Atoi(f[2])
			gid, err2 := strconv.Atoi(f[3])
			if err1 != nil || err2 != nil || uid == 0 {
				return 0, 0, fmt.Errorf("bad passwd entry %q", l)
			}
			return uid, gid, nil
		}
	}
	return 0, 0, errors.New("no such user")
}

// ---------- probes (for the isolation check) ----------

// probe runs a check inside the VM and exits 0 if it succeeded:
//
//	probe tcp host:port     a direct TCP connection
//	probe ipv6off           IPv6 is disabled
//	probe caps NAME...      every process called NAME runs as non-root with no capabilities and no_new_privs
func probe(args []string) int {
	ok := func(format string, a ...any) int { fmt.Printf("OK "+format+"\n", a...); return 0 }
	fail := func(format string, a ...any) int { fmt.Printf("FAIL "+format+"\n", a...); return 1 }
	switch {
	case args[0] == "tcp" && len(args) == 2:
		c, err := net.DialTimeout("tcp", args[1], 4*time.Second)
		if err != nil {
			return fail("%v", err)
		}
		c.Close()
		return ok("connected")
	case args[0] == "ipv6off":
		b, err := os.ReadFile("/proc/sys/net/ipv6/conf/all/disable_ipv6")
		if err != nil || strings.TrimSpace(string(b)) != "1" {
			return fail("IPv6 is on")
		}
		return ok("disable_ipv6=1")
	case args[0] == "caps" && len(args) > 1:
		dirs, _ := filepath.Glob("/proc/[0-9]*")
		found := 0
		for _, d := range dirs {
			comm, err := os.ReadFile(d + "/comm")
			if err != nil {
				continue
			}
			name := strings.TrimSpace(string(comm))
			match := false
			for _, n := range args[1:] {
				match = match || name == n
			}
			if !match {
				continue
			}
			// skip the probes themselves (started with `container exec`, not by the lockdown)
			if cl, err := os.ReadFile(d + "/cmdline"); err == nil {
				if argv := strings.Split(string(cl), "\x00"); len(argv) > 1 && argv[1] == "probe" {
					continue
				}
			}
			found++
			st, err := os.ReadFile(d + "/status")
			if err != nil {
				return fail("%s: %v", d, err)
			}
			for _, l := range strings.Split(string(st), "\n") {
				k, v, _ := strings.Cut(l, ":")
				v = strings.TrimSpace(v)
				switch k {
				case "Uid":
					if strings.HasPrefix(v, "0") || strings.Contains(v, "\t0\t") || strings.HasSuffix(v, "\t0") {
						return fail("%s (%s) runs as root: Uid %s", name, d, v)
					}
				case "CapPrm", "CapEff", "CapBnd", "CapAmb", "CapInh":
					if v != "0000000000000000" {
						return fail("%s (%s): %s %s", name, d, k, v)
					}
				case "NoNewPrivs":
					if v != "1" {
						return fail("%s (%s): no_new_privs not set", name, d)
					}
				}
			}
		}
		if found == 0 {
			return fail("no process named %v", args[1:])
		}
		return ok("%d process(es) %v: non-root, no capabilities, no_new_privs", found, args[1:])
	}
	fmt.Fprintln(os.Stderr, "usage: lockdown probe tcp host:port | ipv6off | caps NAME...")
	return 2
}

// ---------- SearXNG ----------

func searxngSettings(user string) error {
	proxy := os.Getenv("PROXY_URL")
	if !strings.HasPrefix(proxy, "http://") || strings.ContainsAny(proxy, "\"'\n ") {
		return fmt.Errorf("PROXY_URL %q", proxy)
	}
	tmpl, err := os.ReadFile("/opt/mlxlab/settings.yml")
	if err != nil {
		return err
	}
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	s := strings.ReplaceAll(string(tmpl), "@PROXY_URL@", proxy)
	s = strings.ReplaceAll(s, "@SECRET@", hex.EncodeToString(secret))
	uid, gid, err := lookupUser(user)
	if err != nil {
		return err
	}
	for _, d := range []string{"/etc/searxng", "/var/cache/searxng"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile("/etc/searxng/settings.yml", []byte(s), 0o640); err != nil {
		return err
	}
	for _, d := range []string{"/etc/searxng", "/var/cache/searxng"} {
		_ = filepath.Walk(d, func(p string, _ os.FileInfo, err error) error {
			if err == nil {
				_ = os.Lchown(p, uid, gid)
			}
			return nil
		})
	}
	return nil
}
