package main

// ai-connector local: this computer's AI for the apps on it (OpenCAD, EDDy, scripts), without a server. With --uplink
// it also dials an application that cannot reach it (an uplink: the application then uses its API through it). The hub runs inside this process, this computer is its one
// worker (Claude Code too) and the apps find the hub and a client token in <config>/local-ai.json (localai.go).
// With --lan the hub listens on the network, announces itself (Bonjour) and apps on other computers link with a PIN.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/hub"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/web"
)

// localLANPort is the port of --lan without --addr (a fixed one: an app on another computer keeps its address).
const localLANPort = 8485

const localUsage = `ai-connector local — this computer's AI for the apps on it (OpenCAD, scripts), no server, no vboard

usage:
  ai-connector local [--addr HOST:PORT] [--lan] [--name NAME] [--console ADDR] [--state DIR] [--print-token]
                     [--uplink URL [--uplink-token TOKEN]] [--install | --uninstall]

  --addr HOST:PORT  where the hub listens (default 127.0.0.1:0, a free port on this computer only; with --lan
                    :8485, every network interface). Without --lan only a loopback address.
  --lan             apps on other computers too: listen on the network, announce it with Bonjour
                    (_ai-connector._tcp) and link apps with a PIN (approved at <address>/pin with the console
                    password, which it prints)
  --name NAME       the Bonjour name (default: "<this computer> (ai-connector)")
  --console ADDR    also serve the ai-connector console there (it prints the password)
  --state DIR       its jobs and Claude Code sessions (default <files>/local/worker, apart from a plain worker's)
  --uplink URL      also dial this application's uplink endpoint (an address it gives for that) and serve it the
                    API through it, as a client acting for this computer's owner (docs/api.md "Uplinks")
  --uplink-token T  the application's token for it (or $AIC_UPLINK_TOKEN); needed the first time, then kept in
                    <files>/local (never in the start at login)
  --print-token     print the local apps' token (to type it into an app on another computer); a running one's
                    is printed and nothing starts. Tokens are never printed otherwise.
  --install         start it at login with these flags (a launchd agent, a systemd user unit, a scheduled task);
                    a copy that runs already is stopped: the service takes over
  --uninstall       stop that and remove it

The apps on this computer find the address and a client token in <files>/local-ai.json (mode 0600), written
while it runs and removed when it ends; they use the OpenAI API (/v1) and the jobs API (docs/api.md).
One runs at a time with the same settings ($MLXLAB_HOME): another one waits until the first ends, then takes over.
Its state (the hub's clients, the console password) is kept in <files>/local, apart from ai-connector vboard's.
`

// localOptions are the flags of `ai-connector local`.
type localOptions struct {
	addr                 string // "": the default (listenAddr)
	lan                  bool
	name, console, state string
	install, uninstall   bool
	printToken           bool
	uplink, uplinkToken  string
	env                  envFlag
}

// parseLocal reads the flags; flag.ErrHelp for -h/--help (the usage went to out then).
func parseLocal(args []string, out io.Writer) (localOptions, error) {
	var o localOptions
	if helpFlag(args) {
		fmt.Fprint(out, localUsage)
		return o, flag.ErrHelp
	}
	fs := flag.NewFlagSet("ai-connector local", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.addr, "addr", "", "where the hub listens")
	fs.BoolVar(&o.lan, "lan", false, "listen on the network, announce it, link apps with a PIN")
	fs.StringVar(&o.name, "name", "", "the Bonjour name")
	fs.StringVar(&o.console, "console", "", "serve the console here")
	fs.StringVar(&o.state, "state", "", "this process's jobs and sessions")
	fs.BoolVar(&o.printToken, "print-token", false, "print the local apps' token")
	fs.StringVar(&o.uplink, "uplink", "", "an application's uplink endpoint")
	fs.StringVar(&o.uplinkToken, "uplink-token", "", "the application's token for it")
	fs.BoolVar(&o.install, "install", false, "start at login")
	fs.BoolVar(&o.uninstall, "uninstall", false, "do not start at login any more")
	// not in the usage: the Windows task's command carries the environment of --install with it (as vboard's)
	fs.Var(&o.env, "env", "KEY=VALUE, set before anything else (only the keys --install keeps)")
	if err := fs.Parse(args); err != nil {
		return o, fmt.Errorf("%w (ai-connector local --help)", err)
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("ai-connector local takes no arguments, got %q (ai-connector local --help)", fs.Arg(0))
	}
	if o.install && o.uninstall {
		return o, errors.New("--install or --uninstall, not both")
	}
	if o.uplinkToken != "" && o.uplink == "" {
		return o, errors.New("--uplink-token goes with --uplink URL")
	}
	if _, err := o.listenAddr(); err != nil {
		return o, err
	}
	return o, nil
}

// listenAddr is where the hub listens: --addr, else a free loopback port (on the network: localLANPort). Without
// --lan the hub is for this computer only, so an address other devices reach is refused.
func (o localOptions) listenAddr() (string, error) {
	if o.addr == "" {
		if o.lan {
			return ":" + strconv.Itoa(localLANPort), nil
		}
		return "127.0.0.1:0", nil
	}
	host, _, err := net.SplitHostPort(o.addr)
	if err != nil {
		return "", fmt.Errorf("--addr %q: %v (HOST:PORT, e.g. 127.0.0.1:8485)", o.addr, err)
	}
	if !o.lan && !loopbackHost(host) {
		return "", fmt.Errorf("--addr %s is reachable from other devices: use --lan for that (apps link with a PIN), "+
			"else a loopback address such as 127.0.0.1:8485", o.addr)
	}
	return o.addr, nil
}

// loopbackHost: localhost or a loopback IP ("" and 0.0.0.0 are every interface).
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// localKind is the start at login of `ai-connector local` with the flags that were given (paths absolute); never
// --print-token or a token: the client token is in <files>/local.
func localKind(o localOptions) serviceKind {
	k := serviceKind{label: "de.rnd7.ai-connector.local", unit: "ai-connector-local.service", task: "ai-connector local",
		log: "ai-connector-local.log", what: "ai-connector local", desc: "ai-connector local (this computer's AI for its apps)",
		args: []string{"local"}}
	if o.lan {
		k.args = append(k.args, "--lan")
	}
	if o.addr != "" {
		k.args = append(k.args, "--addr", o.addr)
	}
	if o.name != "" {
		k.args = append(k.args, "--name", o.name)
	}
	if o.console != "" {
		k.args = append(k.args, "--console", o.console)
	}
	if o.state != "" {
		k.args = append(k.args, "--state", absPath(o.state))
	}
	if o.uplink != "" { // its token is kept in <files>/local (ensureUplink)
		k.args = append(k.args, "--uplink", o.uplink)
	}
	return k
}

// uplinkName is the name an application shows for this computer's uplink.
func (o localOptions) uplinkName(host string) string {
	if o.name != "" {
		return o.name
	}
	return host + " (ai-connector)"
}

// token is --uplink-token, else $AIC_UPLINK_TOKEN ("" keeps the stored one).
func (o localOptions) token(getenv func(string) string) string {
	if o.uplinkToken != "" {
		return o.uplinkToken
	}
	return strings.TrimSpace(getenv("AIC_UPLINK_TOKEN"))
}

// storeUplink keeps the uplink and its token in the hub's state in dir before the start at login is installed (the
// service gets only --uplink URL): the hub is opened for it and closed again.
func storeUplink(dir string, o localOptions) error {
	pw, err := consolePassword(dir)
	if err != nil {
		return err
	}
	s, err := hub.New(hub.Config{Password: pw, Data: dir, Tools: filepath.Join(dir, "tools"), Owner: standaloneOwner})
	if err != nil {
		return err
	}
	defer s.Close()
	_, err = s.EnsureUplink(o.uplinkName(web.HostName()), o.uplink, o.token(os.Getenv), standaloneOwner)
	return err
}

// standaloneArgs: `ai-connector serve --standalone …` is `ai-connector local …` (ok: --standalone was there).
func standaloneArgs(args []string) ([]string, bool) {
	i := slices.IndexFunc(args, func(a string) bool { return a == "--standalone" || a == "-standalone" })
	if i < 0 {
		return args, false
	}
	return slices.Delete(slices.Clone(args), i, i+1), true
}

// localBase is the hub's address for the apps on this computer: 127.0.0.1 when it listens on every interface.
func localBase(a net.Addr) string {
	if t, ok := a.(*net.TCPAddr); ok && (t.IP == nil || t.IP.IsUnspecified()) {
		return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(t.Port))
	}
	return "http://" + a.String()
}

// lanURLs are the hub's addresses for other computers: <host>.local, then this computer's IPv4 addresses.
func lanURLs(a net.Addr, host string) []string {
	t, ok := a.(*net.TCPAddr)
	if !ok {
		return nil
	}
	port := strconv.Itoa(t.Port)
	if t.IP != nil && !t.IP.IsUnspecified() {
		return []string{"http://" + net.JoinHostPort(t.IP.String(), port)}
	}
	var out []string
	if host != "" {
		h := strings.TrimSuffix(strings.ToLower(host), ".local")
		out = append(out, "http://"+h+".local:"+port)
	}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, ad := range addrs {
			if n, ok := ad.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				out = append(out, "http://"+net.JoinHostPort(n.IP.String(), port))
			}
		}
	}
	return out
}

// consolePassword is the hub's sign-in password: made once, kept in dir (vboard and local each their own).
func consolePassword(dir string) (string, error) {
	file := filepath.Join(dir, "console-password")
	pw, _ := os.ReadFile(file)
	if p := strings.TrimSpace(string(pw)); p != "" {
		return p, nil
	}
	p := randomHex(12)
	return p, os.WriteFile(file, []byte(p), 0o600)
}

// runningToken is the client token of the `ai-connector local` that runs with these settings (its discovery file
// names its PID); false when none runs.
func runningToken(root, dir string) (string, bool) {
	pid, held := lockHolder(instanceLock(dir))
	if !held || pid <= 0 {
		return "", false
	}
	info, err := readLocalAI(root)
	if err != nil || info.PID != pid || info.Token == "" {
		return "", false
	}
	return info.Token, true
}

func localCommand(args []string) error {
	o, err := parseLocal(args, os.Stdout)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := applyEnv(o.env, os.Setenv); err != nil {
		return err
	}
	root := engine.ConfigDir()
	dir := filepath.Join(root, "local")
	if o.uninstall || o.install { // install.go
		svc, err := newService(os.Getenv, localKind(o), instanceLock(dir))
		if err != nil {
			return err
		}
		if o.uninstall {
			if err := svc.uninstall(); err != nil {
				return err
			}
			fmt.Println("ai-connector local does not start at login any more")
			return nil
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if o.uplink != "" {
			if err := storeUplink(dir, o); err != nil {
				return fmt.Errorf("--uplink: %w", err)
			}
		}
		if err := svc.install(); err != nil {
			return err
		}
		fmt.Println(svc.describe())
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if o.printToken {
		if tok, ok := runningToken(root, dir); ok {
			fmt.Println(tok)
			return nil
		}
	}
	addr, _ := o.listenAddr() // checked by parseLocal
	// its own jobs, sessions and approvals, apart from a plain worker's in root and vboard's; models shared
	own := stateDir(o.state, os.Getenv, filepath.Join(dir, "worker"))

	// one at a time with these settings (instance.go): another one is waited for, not given up on
	waitCtx, waitStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	waited := false
	lock, err := waitInstance(waitCtx, instanceLock(dir), 5*time.Second, func(pid int) {
		who := "another ai-connector local runs"
		if pid > 0 {
			who += fmt.Sprintf(" (pid %d)", pid)
		}
		fmt.Printf("%s with the settings in %s: waiting until it ends (`ai-connector local --install` replaces it)\n", who, dir)
		waited = true
	})
	waitStop()
	if errors.Is(err, context.Canceled) {
		return nil // stopped while waiting
	}
	if err != nil {
		return fmt.Errorf("%s: %w", instanceLock(dir), err)
	}
	defer lock.Close()
	if waited {
		fmt.Println("the other ai-connector local ended: this one takes over")
	}

	host := web.HostName()
	name := o.name
	if name == "" {
		name = host + " (ai-connector)"
	}
	pw, err := consolePassword(dir)
	if err != nil {
		return err
	}
	// Owner: whoever signs in (the console, to approve a PIN) acts for the worker's owner, so linked apps get Claude Code
	s, err := hub.New(hub.Config{Password: pw, Data: dir, Tools: filepath.Join(dir, "tools"), MDNSName: name, Owner: standaloneOwner})
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w (--addr)", addr, err)
	}
	if !o.lan && !loopback(l.Addr()) {
		l.Close()
		return fmt.Errorf("%s is reachable from other devices: use --lan for that", l.Addr())
	}
	base := localBase(l.Addr())
	setupPage := s.EnableSetupPage(base) // the Hugging Face token (its code is in the address)
	if o.uplink != "" {
		up, err := s.EnsureUplink(o.uplinkName(host), o.uplink, o.token(os.Getenv), standaloneOwner)
		if err != nil {
			l.Close()
			return fmt.Errorf("--uplink: %w", err)
		}
		fmt.Printf("uplink: %s (its state shows in the console)\n", up.URL)
	}
	workerToken, err := s.LocalWorker(host, standaloneOwner)
	if err != nil {
		return fmt.Errorf("this computer as worker: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hubDone := make(chan error, 1)
	go func() { hubDone <- s.Serve(ctx, l) }()
	fmt.Printf("ai-connector local: %s\n", base)
	if o.lan {
		withdraw := s.Announce(l.Addr())
		defer withdraw()
		urls := lanURLs(l.Addr(), host)
		fmt.Printf("on the network (Bonjour %q): %s\n", name, strings.Join(urls, ", "))
		pinAt := base + "/pin"
		if len(urls) > 0 {
			pinAt = urls[0] + "/pin"
		}
		fmt.Printf("apps on other computers link with a PIN: approve it at %s (console password %s)\n", pinAt, pw)
	}
	if o.console != "" {
		cl, err := net.Listen("tcp", o.console)
		if err != nil {
			return fmt.Errorf("--console: %w", err)
		}
		fmt.Printf("ai-connector console: http://%s/console/ (password %s)\n", cl.Addr(), pw)
		go func() { _ = http.Serve(cl, s.Handler()) }()
	}
	// the apps on this computer (localai.go)
	if clientToken, err := s.LocalClient("local apps", standaloneOwner); err != nil {
		fmt.Fprintf(os.Stderr, "local apps: %v\n", err)
	} else {
		if o.printToken {
			fmt.Printf("token for an app on another computer: %s\n", clientToken)
		}
		if err := writeLocalAI(root, localAI{URL: base, Token: clientToken, PID: os.Getpid()}); err != nil {
			fmt.Fprintf(os.Stderr, "local apps: %v\n", err)
		} else {
			defer removeLocalAI(root, os.Getpid())
			fmt.Printf("local apps: %s\n", filepath.Join(root, localAIFile))
		}
	}

	_, mgr := setup(root, own)
	dl := downloads(root, mgr.Catalog) // on request only (GET /api/setup, POST /api/downloads)
	dl.Changed = func(st engine.DownloadState) {
		if st.State == engine.DownloadDone {
			go s.RefreshWorkers() // offered now
		}
	}
	fmt.Printf("setup page (Hugging Face token): %s\n", setupPage)
	werr := worker(mgr, workerConfig{Hub: base, Token: workerToken}, host,
		(&web.Server{Jobs: mgr, Prompts: prompts.Open(mgr.Dir), Home: root, HomeTools: homeTools, Version: version, Agents: claudeAgents,
			Downloads: dl}).Handler())
	stop()
	<-hubDone
	mgr.Unload()
	claudeAgents.Close()
	closeSandboxes()
	return werr
}
