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
}

// standaloneOwner owns the worker and the connector of the standalone hub: Claude Code runs on its owner's workers.
const standaloneOwner = "local"

const vboardUsage = `ai-connector vboard — vboard's local AI on this computer, no server needed

usage:
  ai-connector vboard [<vboard address>] [--token T] [--board ID] [--name NAME] [--chat MODEL] [--agents LIST]
                      [--console 127.0.0.1:8485] [--state DIR]

  <vboard address>  e.g. https://vboard.example.com (or --url, $VBOARD_URL)
  --token T         a vboard token: "Set up AI" → "Create token" ($VBOARD_TOKEN)
  --board ID        everyone on this board uses this AI (the token needs "Can edit" there)
  --name NAME       what vboard shows (default: "<this computer> (ai-connector)")
  --chat MODEL      the chat model (default: the first one this computer offers that is not Claude Code)
  --agents LIST     what vboard's chat panel offers, in order, comma-separated — the first must be a chat model,
                    e.g. "qwen3.6-35b-a3b,claude-code/sonnet,claude-code/opus" (Claude Code: installed and logged in
                    here). Default: the chat model alone. With only claude-code/… models the --chat model goes first.
  --console ADDR    also serve the ai-connector console there (it prints the password)

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
	console := fs.String("console", "", "serve the console here")
	state := fs.String("state", "", "this process's jobs and sessions")
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

	root := engine.ConfigDir()
	dir := filepath.Join(root, "vboard")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	set, err := resolveVboard(vboardSettings{URL: *url, Token: *token, Board: *board, Name: *name, Chat: *chat, Agents: *agentList}, os.Getenv, filepath.Join(dir, "settings.json"))
	if err != nil {
		return err
	}
	host := web.HostName()
	agents, err := vboardAgents(set.Agents, set.Chat)
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
	workerToken, err := s.LocalWorker(host, standaloneOwner)
	if err != nil {
		return fmt.Errorf("this computer as worker: %w", err)
	}
	if _, err := s.EnsureConnector(hub.ConnectorSettings{Name: connName, URL: set.URL, Token: set.Token, Board: set.Board,
		Models: hub.ConnectorModels{Chat: set.Chat}, Agents: agents, Owner: standaloneOwner}); err != nil {
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

	own := stateDir(*state, os.Getenv, root)
	_, mgr := setup(root, own)
	fmt.Printf("ai-connector for vboard %s as %q (settings: %s)\n", set.URL, connName, filepath.Join(dir, "settings.json"))
	werr := worker(mgr, workerConfig{Hub: "http://" + l.Addr().String(), Token: workerToken}, host,
		(&web.Server{Jobs: mgr, Prompts: prompts.Open(mgr.Dir), Home: root, HomeTools: homeTools, Version: version, Agents: claudeAgents}).Handler())
	stop()
	<-hubDone
	mgr.Unload()
	claudeAgents.Close()
	closeSandboxes()
	return werr
}

// resolveVboard: the flags, else $VBOARD_URL / $VBOARD_TOKEN, else the settings of the last start; what was given is
// kept for the next one.
func resolveVboard(flags vboardSettings, getenv func(string) string, file string) (vboardSettings, error) {
	var saved vboardSettings
	if b, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(b, &saved)
	}
	pick := func(flag, env, saved string) string {
		if flag != "" {
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
		URL:    pick(flags.URL, "VBOARD_URL", saved.URL),
		Token:  pick(flags.Token, "VBOARD_TOKEN", saved.Token),
		Board:  pick(flags.Board, "VBOARD_BOARD", saved.Board),
		Name:   pick(flags.Name, "", saved.Name),
		Chat:   pick(flags.Chat, "", saved.Chat),
		Agents: pick(flags.Agents, "", saved.Agents),
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

// vboardAgents parses --agents ("model[=name],…"); Claude Code alone gets the chat model in front (the first must be one).
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
		if chat == "" {
			return nil, errors.New("--agents: the first is the default and must be a chat model (e.g. --chat qwen3.6-35b-a3b, or a chat model first in --agents)")
		}
		out = append([]hub.ConnectorAgent{{Model: chat}}, out...)
	}
	return out, nil
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
