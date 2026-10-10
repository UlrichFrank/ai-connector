package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// blockedNets are never dialled: everything that is not the public internet (IANA special-purpose registries),
// plus the IPv6 transition prefixes that can embed an IPv4 address (NAT64, 6to4, Teredo).
var blockedNets = mustPrefixes(
	"0.0.0.0/8",       // "this network"
	"10.0.0.0/8",      // RFC1918
	"100.64.0.0/10",   // CGNAT (also Tailscale)
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local, cloud metadata
	"172.16.0.0/12",   // RFC1918
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"192.88.99.0/24",  // 6to4 relay anycast
	"192.168.0.0/16",  // RFC1918 (also the container networks)
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, broadcast
	"::/128",          // unspecified
	"::1/128",         // loopback
	"::ffff:0:0/96",   // IPv4-mapped (unmapped before the check; kept for safety)
	"64:ff9b::/96",    // NAT64
	"64:ff9b:1::/48",  // local-use NAT64
	"100::/64",        // discard
	"2001::/32",       // Teredo
	"2001:2::/48",     // benchmarking
	"2001:db8::/32",   // documentation
	"2002::/16",       // 6to4
	"fc00::/7",        // ULA
	"fe80::/10",       // link-local
	"fec0::/10",       // site-local (deprecated)
	"ff00::/8",        // multicast
)

// deniedSuffixes are names (and their subdomains) that are never resolved: the names that only mean something on
// a LAN. Your own domain goes into DENY (the worker's MLXLAB_WEB_DENY or ~/.config/mlxlab/web.deny).
var deniedSuffixes = []string{
	"localhost", "local", "internal", "lan", "home.arpa", "intranet", "corp", "private", "localdomain",
	"home", "arpa",
}

// Policy decides which destinations the proxy may connect to.
type Policy struct {
	Nets     []netip.Prefix // denied in addition to blockedNets (the container networks, the proxy's own addresses)
	Suffixes []string       // denied in addition to deniedSuffixes
	Ports    map[int]bool
	Resolver *net.Resolver
}

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

// ParseDeny reads a comma separated deny list: CIDRs, IPs and name suffixes ("example.com" denies it and its
// subdomains).
func (p *Policy) ParseDeny(list string) error {
	for _, item := range strings.Split(list, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" {
			continue
		}
		if pfx, err := netip.ParsePrefix(item); err == nil {
			p.Nets = append(p.Nets, pfx.Masked())
			continue
		}
		if ip, err := netip.ParseAddr(item); err == nil {
			p.Nets = append(p.Nets, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
			continue
		}
		p.Suffixes = append(p.Suffixes, strings.Trim(item, "."))
	}
	return nil
}

// errDenied is a refusal by the policy (as opposed to a network error).
type errDenied struct{ reason string }

func (e errDenied) Error() string { return "denied: " + e.reason }

func denied(format string, a ...any) error { return errDenied{fmt.Sprintf(format, a...)} }

func isDenied(err error) bool {
	var d errDenied
	return errors.As(err, &d)
}

// CheckIP refuses every address that is not on the public internet.
func (p *Policy) CheckIP(ip netip.Addr) error {
	if !ip.IsValid() {
		return denied("invalid address")
	}
	ip = ip.Unmap().WithZone("")
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return denied("%s is not a public address", ip)
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return denied("%s is in %s", ip, n)
		}
	}
	for _, n := range p.Nets {
		if n.Contains(ip) {
			return denied("%s is in the deny list (%s)", ip, n)
		}
	}
	return nil
}

// NormalizeHost lowercases a host name and removes a trailing dot; IPv6 brackets are removed.
func NormalizeHost(host string) string {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// CheckName refuses names that only exist on a LAN, the user's own domain, and anything odd.
func (p *Policy) CheckName(host string) error {
	if host == "" {
		return denied("empty host")
	}
	if len(host) > 253 {
		return denied("host name too long")
	}
	for _, r := range host {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_') {
			return denied("host name %q has characters that are not allowed (use punycode)", host)
		}
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return denied("single-label name %q", host) // would be completed by a search domain
	}
	for _, l := range labels {
		if l == "" {
			return denied("empty label in %q", host)
		}
	}
	tld := labels[len(labels)-1]
	if strings.Trim(tld, "0123456789") == "" || strings.HasPrefix(tld, "0x") {
		// a numeric (or hex) last label: "2130706433", "0x7f.1", "127.1" — IP literals in disguise
		return denied("numeric host %q", host)
	}
	for _, s := range append(append([]string{}, deniedSuffixes...), p.Suffixes...) {
		if host == s || strings.HasSuffix(host, "."+s) {
			return denied("name %q is under %q", host, s)
		}
	}
	return nil
}

// Resolve returns the addresses of host, IPv4 first; it refuses the host if any of them is not public (a name
// that resolves to a mix is refused as a whole).
func (p *Policy) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	host = NormalizeHost(host)
	if ip, err := netip.ParseAddr(host); err == nil {
		if err := p.CheckIP(ip); err != nil {
			return nil, err
		}
		return []netip.Addr{ip.Unmap()}, nil
	}
	if err := p.CheckName(host); err != nil {
		return nil, err
	}
	r := p.Resolver
	if r == nil {
		r = &net.Resolver{PreferGo: true}
	}
	addrs, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	var v4, v6 []netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if err := p.CheckIP(a); err != nil {
			return nil, denied("%s resolves to a non-public address (%v)", host, err)
		}
		if a.Is4() {
			v4 = append(v4, a)
		} else {
			v6 = append(v6, a)
		}
	}
	return append(v4, v6...), nil
}

// CheckPort refuses every port but the allowed ones (80 and 443).
func (p *Policy) CheckPort(port string) (uint16, error) {
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0, denied("bad port %q", port)
	}
	if !p.Ports[int(n)] {
		return 0, denied("port %d is not allowed", n)
	}
	return uint16(n), nil
}
