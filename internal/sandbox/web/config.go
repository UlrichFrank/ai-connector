// Package web runs mlxlab's web sandbox and offers it as an agent.Toolbox (web_search, web_fetch).
//
// Three Apple `container` VMs (each its own lightweight Linux VM):
//
//	mlxlab-web-proxy    the egress proxy; on the NAT network mlxlab-web-ext (its only route out) and on the
//	                    host-only network mlxlab-web-int; refuses everything that is not the public internet
//	mlxlab-web-tools    the tools service; only on mlxlab-web-int; the Mac talks to it through a unix socket
//	                    forwarded over vsock (--publish-socket), so it needs no route to the Mac at all
//	mlxlab-web-searxng  SearXNG (only when no Brave key is configured); only on mlxlab-web-int
//
// Inside each VM a lockdown entrypoint closes the network down before the service starts unprivileged (see
// sandbox/web/lockdown.sh); the pf anchor on the Mac (sandbox/web/pf.conf) covers a VM whose kernel is compromised.
// The containers start on first use, are restarted when dead and stop after an idle time.
package web

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config of the web sandbox. Zero values are replaced by the defaults.
type Config struct {
	CLI         string        // the container CLI (default "container")
	Prefix      string        // names of containers and networks (default "mlxlab-web")
	Socket      string        // the tools service's socket on the Mac (default ~/Library/Caches/mlxlab/web-tools.sock)
	DNS         string        // public resolvers of the proxy (default 1.1.1.1:53,9.9.9.9:53)
	IntSubnet   string        // host-only network of tools, SearXNG and proxy (default 192.168.240.0/24)
	ExtSubnet   string        // NAT network of the proxy (default 192.168.241.0/24)
	IntSubnetV6 string        // (default fd6d:6c78:7765:f0::/64)
	ExtSubnetV6 string        // (default fd6d:6c78:7765:f1::/64)
	Idle        time.Duration // stop after this long without a call (default 10m)
	Deny        string        // extra deny list for the proxy: CIDRs, IPs, name suffixes (comma separated; see LoadDeny)
	BraveKey    string        // Brave Search API key; empty = SearXNG (see LoadBraveKey)
	Logf        func(format string, args ...any)
}

func (c *Config) defaults() {
	if c.CLI == "" {
		c.CLI = "container"
	}
	if c.Prefix == "" {
		c.Prefix = "mlxlab-web"
	}
	if c.Socket == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			dir = os.TempDir()
		}
		c.Socket = filepath.Join(dir, "mlxlab", "web-tools.sock")
	}
	if c.DNS == "" {
		c.DNS = "1.1.1.1:53,9.9.9.9:53"
	}
	if c.IntSubnet == "" {
		c.IntSubnet = "192.168.240.0/24"
	}
	if c.ExtSubnet == "" {
		c.ExtSubnet = "192.168.241.0/24"
	}
	if c.IntSubnetV6 == "" {
		c.IntSubnetV6 = "fd6d:6c78:7765:f0::/64"
	}
	if c.ExtSubnetV6 == "" {
		c.ExtSubnetV6 = "fd6d:6c78:7765:f1::/64"
	}
	if c.Idle == 0 {
		c.Idle = 10 * time.Minute
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
}

func (c *Config) name(part string) string { return c.Prefix + "-" + part }

// LoadBraveKey reads the Brave Search API key from MLXLAB_BRAVE_KEY or ~/.config/mlxlab/brave.key. The file must
// not be readable by others (mode 0600). No key (and no error) means: use SearXNG.
func LoadBraveKey() (string, error) {
	if k := strings.TrimSpace(os.Getenv("MLXLAB_BRAVE_KEY")); k != "" {
		return k, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil
	}
	return readKeyFile(filepath.Join(home, ".config", "mlxlab", "brave.key"))
}

// LoadDeny reads the proxy's extra deny list (Config.Deny): MLXLAB_WEB_DENY and ~/.config/mlxlab/web.deny, both
// comma or line separated CIDRs, IPs and name suffixes (e.g. your own domain: "example.org" denies it and its
// subdomains). Lines starting with # are comments. No file is no error.
func LoadDeny() (string, error) {
	var items []string
	add := func(s string) {
		for _, line := range strings.Split(s, "\n") {
			if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			for _, it := range strings.Split(line, ",") {
				if it = strings.TrimSpace(it); it != "" {
					items = append(items, it)
				}
			}
		}
	}
	add(os.Getenv("MLXLAB_WEB_DENY"))
	if home, err := os.UserHomeDir(); err == nil {
		b, err := os.ReadFile(filepath.Join(home, ".config", "mlxlab", "web.deny"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		add(string(b))
	}
	return strings.Join(items, ","), nil
}

func readKeyFile(path string) (string, error) {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by others (mode %v): chmod 600 %s", path, st.Mode().Perm(), path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	k := strings.TrimSpace(string(b))
	if strings.ContainsAny(k, " \t\r\n") {
		return "", fmt.Errorf("%s: the key must be a single line", path)
	}
	return k, nil
}
