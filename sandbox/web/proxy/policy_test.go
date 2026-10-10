package main

import (
	"context"
	"net"
	"net/netip"
	"testing"
)

func testPolicy() *Policy {
	p := &Policy{Ports: map[int]bool{80: true, 443: true}}
	_ = p.ParseDeny("203.0.114.0/24, 8.8.4.4, evil.example, example.org")
	return p
}

func TestCheckIP(t *testing.T) {
	p := testPolicy()
	deny := []string{
		"127.0.0.1", "127.1.2.3", "10.0.0.1", "10.200.2.1", "172.16.0.1", "172.31.255.255", "192.168.0.1",
		"192.168.240.1", "192.168.241.1", "100.64.0.1", "100.127.255.254", "169.254.169.254", "0.0.0.0",
		"224.0.0.1", "239.255.255.250", "255.255.255.255", "240.0.0.1", "198.18.0.1", "192.0.0.170",
		"::1", "::", "::ffff:192.168.0.1", "::ffff:127.0.0.1", "fe80::1", "fd00::1", "fc00::1", "ff02::1",
		"64:ff9b::c0a8:1", "2002:c0a8:101::1", "2001:0:c0a8:101::1", "fec0::1",
		"203.0.114.7", "8.8.4.4",
	}
	for _, s := range deny {
		if err := p.CheckIP(netip.MustParseAddr(s)); err == nil {
			t.Errorf("%s allowed, want denied", s)
		}
	}
	allow := []string{"1.1.1.1", "8.8.8.8", "93.184.215.14", "172.32.0.1", "100.128.0.1", "2606:4700::1111"}
	for _, s := range allow {
		if err := p.CheckIP(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s denied: %v", s, err)
		}
	}
}

func TestCheckName(t *testing.T) {
	p := testPolicy()
	deny := []string{
		"example.org", "k8s.example.org", "a.b.example.org", "printer.local", "x.internal", "nas.lan", "box.home.arpa",
		"localhost", "foo.localhost", "router", "2130706433", "0x7f.1", "127.1", "1.2.3.4.5", "evil.example",
		"www.evil.example", "bad host.com", "ünicode.de", "a..b.com",
	}
	for _, s := range deny {
		if err := p.CheckName(NormalizeHost(s)); err == nil {
			t.Errorf("%s allowed, want denied", s)
		}
	}
	allow := []string{"example.com", "www.example.org.example.com", "exampleorg.com", "api.search.brave.com", "x.de", "b.cafe",
		"xn--nicode-2ya.de", "192.168.1.1.nip.io"}
	for _, s := range allow {
		if err := p.CheckName(NormalizeHost(s)); err != nil {
			t.Errorf("%s denied: %v", s, err)
		}
	}
	if NormalizeHost("K8S.EXAMPLE.ORG.") != "k8s.example.org" {
		t.Error("normalize")
	}
}

func TestCheckPort(t *testing.T) {
	p := testPolicy()
	for _, s := range []string{"22", "8080", "8484", "0", "65536", "x", "-1"} {
		if _, err := p.CheckPort(s); err == nil {
			t.Errorf("port %s allowed", s)
		}
	}
	for _, s := range []string{"80", "443"} {
		if _, err := p.CheckPort(s); err != nil {
			t.Errorf("port %s denied", s)
		}
	}
}

// Resolve refuses IP literals that are not public, and names whose addresses are not public (the stub DNS
// stands in for nip.io).
func TestResolveRefusesMixedAnswers(t *testing.T) {
	p := testPolicy()
	if _, err := p.Resolve(context.Background(), "192.168.1.1"); !isDenied(err) {
		t.Errorf("literal private IP: %v", err)
	}
	if _, err := p.Resolve(context.Background(), "[::1]"); !isDenied(err) {
		t.Errorf("literal ::1: %v", err)
	}
	addrs, err := p.Resolve(context.Background(), "1.1.1.1")
	if err != nil || len(addrs) != 1 {
		t.Errorf("public literal: %v %v", addrs, err)
	}
	// a resolver that answers with a private address (as 192.168.1.1.nip.io does)
	p.Resolver = &net.Resolver{PreferGo: true, Dial: stubDNS(t, [4]byte{192, 168, 1, 1})}
	if _, err := p.Resolve(context.Background(), "192.168.1.1.nip.io"); !isDenied(err) {
		t.Errorf("name resolving to a private IP: %v", err)
	}
	p.Resolver = &net.Resolver{PreferGo: true, Dial: stubDNS(t, [4]byte{93, 184, 215, 14})}
	if _, err := p.Resolve(context.Background(), "example.com"); err != nil {
		t.Errorf("public name: %v", err)
	}
}

// stubDNS is a resolver Dial func that answers A queries with ip and AAAA queries with nothing (TCP framing over
// an in-memory pipe).
func stubDNS(t *testing.T, ip [4]byte) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			for {
				var l [2]byte
				if _, err := readFull(server, l[:]); err != nil {
					return
				}
				q := make([]byte, int(l[0])<<8|int(l[1]))
				if _, err := readFull(server, q); err != nil || len(q) < 12 {
					return
				}
				// question ends after the name (labels up to 0) + type + class
				i := 12
				for i < len(q) && q[i] != 0 {
					i += int(q[i]) + 1
				}
				qend := i + 5
				if qend > len(q) {
					return
				}
				qtype := int(q[i+1])<<8 | int(q[i+2])
				resp := append([]byte{}, q[:2]...)
				an := byte(0)
				if qtype == 1 {
					an = 1
				}
				resp = append(resp, 0x81, 0x80, 0, 1, 0, an, 0, 0, 0, 0)
				resp = append(resp, q[12:qend]...)
				if an == 1 {
					resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, ip[0], ip[1], ip[2], ip[3])
				}
				out := append([]byte{byte(len(resp) >> 8), byte(len(resp))}, resp...)
				if _, err := server.Write(out); err != nil {
					return
				}
			}
		}()
		return client, nil
	}
}

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
