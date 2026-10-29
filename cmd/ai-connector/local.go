package main

// ai-connector local: the models of this computer for its apps, without vboard and without a server — the hub of
// `ai-connector vboard` (vboard.go) without its connector: on a fixed loopback port, so an app can be set up with an
// address and a token once (OpenAI API, docs/api.md "Local apps").

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/philipparndt/ai-connector/internal/engine"
)

// localAddr is where `ai-connector local` listens by default (serve has 8484, the console 8485 in the examples).
const localAddr = "127.0.0.1:8486"

const localUsage = `ai-connector local — the models of this computer for its apps, no vboard, no server

usage:
  ai-connector local [--addr 127.0.0.1:8486] [--console 127.0.0.1:8485] [--state DIR]

  --addr ADDR     the OpenAI API for the apps on this computer: http://ADDR/v1 (loopback only)
  --console ADDR  also serve the ai-connector console there (it prints the password): models, workers, and a
                  client token of its own for each app
  --state DIR     its jobs and Claude Code sessions (default <files>/vboard/worker, as ai-connector vboard's)

The address and the token of the client "local apps" are in <files>/local-ai.json (0600) while it runs; the token
stays the same across restarts. It shares its files with ai-connector vboard: one of them runs at a time, the
other waits. A vboard connection kept from ai-connector vboard stays as it is, it is just not started.
`

func localCommand(args []string) error {
	fs := flag.NewFlagSet("ai-connector local", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, localUsage) }
	addr := fs.String("addr", localAddr, "the OpenAI API for the apps on this computer (loopback only)")
	console := fs.String("console", "", "serve the console here")
	state := fs.String("state", "", "this process's jobs and sessions")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("ai-connector local takes no arguments (%q); for vboard: ai-connector vboard", fs.Arg(0))
	}
	own := stateDir(*state, os.Getenv, filepath.Join(engine.ConfigDir(), "vboard", "worker"))
	return runStandalone(standaloneRun{what: "ai-connector local", own: own, addr: *addr, console: *console})
}
