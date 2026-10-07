package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
)

// cli runs the Apple `container` CLI.
type cli struct {
	bin string
	env []string // extra environment (e.g. the Brave key, passed by name with -e so it is not on the command line)
}

func (c cli) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Env = append(os.Environ(), c.env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return out.String(), fmt.Errorf("container %s: %w: %s", strings.Join(redact(args), " "), err, msg)
	}
	return out.String(), nil
}

func redact(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if k, _, ok := strings.Cut(a, "="); ok && strings.Contains(strings.ToUpper(k), "KEY") {
			a = k + "=…"
		}
		out[i] = a
	}
	return out
}

type netStatus struct {
	Network     string `json:"network"`
	IPv4Address string `json:"ipv4Address"`
	IPv4Gateway string `json:"ipv4Gateway"`
}

type containerInfo struct {
	ID     string `json:"id"`
	Status struct {
		State    string      `json:"state"`
		Networks []netStatus `json:"networks"`
	} `json:"status"`
}

// inspect returns the container, or ok=false if there is none.
func (c cli) inspect(ctx context.Context, name string) (containerInfo, bool) {
	out, err := c.run(ctx, "inspect", name)
	if err != nil {
		return containerInfo{}, false
	}
	var v []containerInfo
	if json.Unmarshal([]byte(out), &v) != nil || len(v) == 0 {
		return containerInfo{}, false
	}
	return v[0], true
}

// ipOn is the container's IPv4 address on network.
func (ci containerInfo) ipOn(network string) (netip.Addr, error) {
	for _, n := range ci.Status.Networks {
		if n.Network == network {
			p, err := netip.ParsePrefix(n.IPv4Address)
			if err != nil {
				return netip.Addr{}, fmt.Errorf("%s on %s: %q: %w", ci.ID, network, n.IPv4Address, err)
			}
			return p.Addr(), nil
		}
	}
	return netip.Addr{}, fmt.Errorf("%s is not on network %s", ci.ID, network)
}

type networkInfo struct {
	ID            string `json:"id"`
	Configuration struct {
		Mode string `json:"mode"`
	} `json:"configuration"`
	Status struct {
		IPv4Subnet  string `json:"ipv4Subnet"`
		IPv4Gateway string `json:"ipv4Gateway"`
		IPv6Subnet  string `json:"ipv6Subnet"`
	} `json:"status"`
}

func (c cli) networks(ctx context.Context) ([]networkInfo, error) {
	out, err := c.run(ctx, "network", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var v []networkInfo
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("container network list: %w", err)
	}
	return v, nil
}

// imageNames lists the local images as name:tag.
func (c cli) imageNames(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, "image", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var v []struct {
		Configuration struct {
			Name string `json:"name"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(v))
	for _, i := range v {
		names = append(names, i.Configuration.Name)
	}
	return names, nil
}

func (c cli) imageExists(ctx context.Context, ref string) bool {
	_, err := c.run(ctx, "image", "inspect", ref)
	return err == nil
}
