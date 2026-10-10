package main

// ai-connector vboard: vboard's local AI on this computer, without a server — what vboard-ai was. The hub runs inside
// this process on a loopback port only, this computer is its one worker and the hub's one connector dials vboard
// (internal/hub/standalone.go): the same routing, agents (Claude Code) and bridge protocol as with a server.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/hub"
	"github.com/philipparndt/ai-connector/internal/llm"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/web"
)

// vboardSettings are kept in <files>/vboard/settings.json: a later `ai-connector vboard` without flags starts the
// same connection again.
type vboardSettings struct {
	URL    string `json:"url"`
	Token  string `json:"token"`
	Board  string `json:"board,omitempty"`
	Name   string `json:"name,omitempty"`
	Chat   string `json:"chat,omitempty"`
	Agents string `json:"agents,omitempty"`
	// Generators: the image, video, music, sound and speech models offered to the board (comma list; "" none)
	Generators string `json:"generators,omitempty"`
}

// standaloneOwner owns the worker and the connector of the standalone hub: Claude Code runs on its owner's workers.
const standaloneOwner = "local"

const vboardUsage = `ai-connector vboard — vboard's local AI on this computer, no server needed

usage:
  ai-connector vboard [<vboard address>] [--token T] [--board ID] [--name NAME] [--chat MODEL] [--agents LIST]
                      [--generators LIST] [--console 127.0.0.1:8485] [--state DIR] [--install]
  ai-connector vboard --uninstall

  <vboard address>  e.g. https://vboard.example.com (or --url, $VBOARD_URL)
  --token T         a vboard token: "Set up AI" → "Create token" ($VBOARD_TOKEN)
  --board ID        everyone on this board uses this AI (the token needs "Can edit" there); --board= for all boards
  --name NAME       what vboard shows (default: "<this computer> (ai-connector)")
  --chat MODEL      the chat model (default: the first one this computer offers that is not Claude Code)
  --agents LIST     what vboard's chat panel offers, in order, comma-separated — the first is the default chat model,
                    e.g. "qwen3.6-35b-a3b,claude-code/sonnet,claude-code/opus" (Claude Code: installed and logged in
                    here). Default: the chat model, and Claude Code (Sonnet, Opus) when it is installed and logged in
                    here. With only claude-code/… models the --chat model (or the default one) goes first.
  --generators LIST the models vboard may make pictures, videos, songs, sounds and speech on the board with, in
                    order, comma-separated, e.g. "flux2-klein-4b,ltx-2.5-q4,kokoro-82m" (only those on this
                    computer's disk are offered; "*" for all of them). Default: all of them on this computer's
                    disk, so one downloaded from vboard is offered at once; "none" for none.
  --console ADDR    also serve the ai-connector console there (it prints the password)
  --state DIR       its jobs and Claude Code sessions (default <files>/vboard/worker, apart from a plain worker's)
  --install         start it at login with these settings (a launchd agent, a systemd user unit, a scheduled task)
  --uninstall       stop that and remove it

On its first start it downloads the embedder and the classifier (about 1 GB); vboard asks before anything else
is downloaded ("Set up AI").

An empty flag (--board=, --name=, --chat=, --agents=, --generators=) goes back to the default.
The settings are kept in <files>/vboard/settings.json: ` + "`ai-connector vboard`" + ` alone starts the same connection again.
`

func vboardCommand(args []string) error {
	fs := flag.NewFlagSet("ai-connector vboard", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, vboardUsage) }
	url := fs.String("url", "", "vboard's address")
	token := fs.String("token", "", "a vboard token")
	board := fs.String("board", "", "a board id")
	name := fs.String("name", "", "the name vboard shows")
	chat := fs.String("chat", "", "the chat model")
	agentList := fs.String("agents", "", "the AIs vboard offers, in order")
	generators := fs.String("generators", "", "the image, video, music, sound and speech models vboard offers, in order")
	console := fs.String("console", "", "serve the console here")
	state := fs.String("state", "", "this process's jobs and sessions")
	install := fs.Bool("install", false, "start at login")
	uninstall := fs.Bool("uninstall", false, "do not start at login any more")
	// the address may come first (as with vboard-ai): flags after it still count
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		*url, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 && *url == "" {
		*url = fs.Arg(0)
	}

	if *uninstall { // install.go
		svc, err := newService(os.Getenv)
		if err != nil {
			return err
		}
		if err := svc.uninstall(); err != nil {
			return err
		}
		fmt.Println("ai-connector vboard does not start at login any more")
		return nil
	}

	root := engine.ConfigDir()
	dir := filepath.Join(root, "vboard")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// its own jobs, sessions and approvals, apart from a plain worker's in root (both may run at once); models shared
	own := stateDir(*state, os.Getenv, filepath.Join(dir, "worker"))
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	set, err := resolveVboard(vboardSettings{URL: *url, Token: *token, Board: *board, Name: *name, Chat: *chat, Agents: *agentList, Generators: *generators}, given, os.Getenv, filepath.Join(dir, "settings.json"))
	if err != nil {
		return err
	}
	if *install { // the settings are kept: the service starts with them
		svc, err := newService(os.Getenv)
		if err != nil {
			return err
		}
		if err := svc.install(); err != nil {
			return err
		}
		fmt.Println(svc.describe())
		return nil
	}
	host := web.HostName()
	// --agents, else Claude Code beside the chat model when it is ready here (vboard_setup.go)
	agents, err := defaultAgents(set.Agents, set.Chat, func() bool { return claudeReady(own) })
	if err != nil {
		return err
	}
	connName := set.Name
	if connName == "" {
		connName = host + " (ai-connector)"
	}

	// the hub's sign-in password (only the optional console uses it): made once, kept beside the settings
	pwFile := filepath.Join(dir, "console-password")
	pw, _ := os.ReadFile(pwFile)
	if len(strings.TrimSpace(string(pw))) == 0 {
		pw = []byte(randomHex(12))
		if err := os.WriteFile(pwFile, pw, 0o600); err != nil {
			return err
		}
	}
	s, err := hub.New(hub.Config{Password: strings.TrimSpace(string(pw)), Data: dir, Tools: filepath.Join(dir, "tools")})
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	setupPage := s.EnableSetupPage("http://" + l.Addr().String()) // the Hugging Face token, on this computer only
	workerToken, err := s.LocalWorker(host, standaloneOwner)
	if err != nil {
		return fmt.Errorf("this computer as worker: %w", err)
	}
	if _, err := s.EnsureConnector(hub.ConnectorSettings{Name: connName, URL: set.URL, Token: set.Token, Board: set.Board,
		Models: hub.ConnectorModels{Chat: set.Chat}, Agents: agents, Generators: vboardGenerators(set.Generators), Owner: standaloneOwner}); err != nil {
		if f := hub.ConnectorErrorField(err); f != "" {
			return fmt.Errorf("%s: %w", f, err)
		}
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hubDone := make(chan error, 1)
	go func() { hubDone <- s.Serve(ctx, l) }()
	if *console != "" {
		cl, err := net.Listen("tcp", *console)
		if err != nil {
			return fmt.Errorf("--console: %w", err)
		}
		fmt.Printf("ai-connector console: http://%s/console/ (password %s)\n", cl.Addr(), strings.TrimSpace(string(pw)))
		go func() { _ = http.Serve(cl, s.Handler()) }()
	}
	go logConnector(ctx, s)

	_, mgr := setup(root, own)
	// downloads on request (vboard's Download), and the small models vboard needs at once
	dl := downloads(root, mgr.Catalog)
	dl.Changed = func(st engine.DownloadState) {
		if st.State == engine.DownloadDone {
			go s.RefreshWorkers() // offered now
		}
	}
	startFirstModels(dl, llm.MemBytes())
	fmt.Printf("ai-connector for vboard %s as %q (settings: %s)\nsetup page (Hugging Face token): %s\n", set.URL, connName, filepath.Join(dir, "settings.json"), setupPage)
	werr := worker(mgr, workerConfig{Hub: "http://" + l.Addr().String(), Token: workerToken}, host,
		(&web.Server{Jobs: mgr, Prompts: prompts.Open(mgr.Dir), Home: root, HomeTools: homeTools, Version: version, Agents: claudeAgents,
			Downloads: dl}).Handler())
	stop()
	<-hubDone
	mgr.Unload()
	claudeAgents.Close()
	closeSandboxes()
	return werr
}

// resolveVboard: the flags, else $VBOARD_URL / $VBOARD_TOKEN, else the settings of the last start; what was given is
// kept for the next one. given names the flags on the command line: an empty one (--board=) clears the kept value.
func resolveVboard(flags vboardSettings, given map[string]bool, getenv func(string) string, file string) (vboardSettings, error) {
	var saved vboardSettings
	if b, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(b, &saved)
	}
	pick := func(name, flag, env, saved string) string {
		if flag != "" || given[name] {
			return flag
		}
		if env != "" {
			if v := getenv(env); v != "" {
				return v
			}
		}
		return saved
	}
	out := vboardSettings{
		URL:        pick("url", flags.URL, "VBOARD_URL", saved.URL),
		Token:      pick("token", flags.Token, "VBOARD_TOKEN", saved.Token),
		Board:      pick("board", flags.Board, "VBOARD_BOARD", saved.Board),
		Name:       pick("name", flags.Name, "", saved.Name),
		Chat:       pick("chat", flags.Chat, "", saved.Chat),
		Agents:     pick("agents", flags.Agents, "", saved.Agents),
		Generators: pick("generators", flags.Generators, "", saved.Generators),
	}
	if out.URL == "" {
		return out, errors.New("no vboard address: ai-connector vboard https://vboard.example.com --token … (see --help)")
	}
	if out.Token == "" {
		return out, errors.New("no token: in vboard, \"Set up AI\" → \"Create token\", then --token … or $VBOARD_TOKEN")
	}
	if out != saved {
		b, _ := json.MarshalIndent(out, "", "  ")
		if err := os.WriteFile(file, b, 0o600); err != nil {
			return out, err
		}
	}
	return out, nil
}

// vboardAgents parses --agents ("model[=name],…"); Claude Code alone gets the chat model in front (--chat, else the
// first chat model this computer has; with none yet, Claude Code is the default until there is one).
func vboardAgents(list, chat string) ([]hub.ConnectorAgent, error) {
	var out []hub.ConnectorAgent
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		model, name, _ := strings.Cut(part, "=")
		out = append(out, hub.ConnectorAgent{Model: strings.TrimSpace(model), Name: strings.TrimSpace(name)})
	}
	if len(out) > 0 && strings.HasPrefix(out[0].Model, "claude-code/") {
		out = append([]hub.ConnectorAgent{{ID: "default", Model: chat}}, out...)
	}
	return out, nil
}

// splitList is a comma list's entries (blanks dropped).
func splitList(list string) []string {
	var out []string
	for _, part := range strings.Split(list, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// logConnector prints the connector's state when it changes: connecting, connected (as whom, offering what), errors.
func logConnector(ctx context.Context, s *hub.Server) {
	last := ""
	for {
		changed := s.ConnectorsChanged()
		for _, c := range s.Connectors() {
			line := "vboard: " + c.State
			switch {
			case c.State == "connected":
				names := make([]string, len(c.Advertised))
				for i, a := range c.Advertised {
					names[i] = a.Name
				}
				line = fmt.Sprintf("vboard: connected as %s", c.User)
				if len(names) > 0 {
					line += " — offers " + strings.Join(names, ", ")
				}
			case c.Error != "":
				line += ": " + c.Error
			}
			if line != last {
				fmt.Println(line)
				last = line
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// vboardGenerators: the --generators list; none given means every generator model on this computer's disk
// ("*": a model downloaded from vboard is offered at once), "none" means none.
func vboardGenerators(list string) []string {
	switch strings.TrimSpace(list) {
	case "":
		return []string{"*"}
	case "none":
		return nil
	}
	return splitList(list)
}
