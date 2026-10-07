package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/guard"
	"github.com/philipparndt/ai-connector/internal/models"
)

// guardMain: mlxlab guard eval [-v] — runs the guard's eval set (internal/guard/testdata) on the Laya classifier, in
// this process, the way a chat job calls it (engine.TextBackend.Choose).
func guardMain(args []string) error {
	if len(args) == 0 || args[0] != "eval" {
		fmt.Fprintln(os.Stderr, "usage: mlxlab guard eval [-v]   the guard's eval set on the Laya classifier")
		return errors.New("unknown guard command")
	}
	fs := flag.NewFlagSet("mlxlab guard eval", flag.ContinueOnError)
	verbose := fs.Bool("v", false, "print every case, not only the mistakes")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	srcs, _ := models.Sources(os.Getenv("MLXLAB_MODELS_FROM"), nil)
	text := &engine.TextBackend{Store: &models.Store{Dir: filepath.Join(engine.ConfigDir(), "models"), Sources: srcs}}
	g := guard.New(text, guard.Config{})
	ctx := context.Background()
	start := time.Now()
	if _, err := g.Screen(ctx, "warm up", "warm up"); err != nil { // loads the classifier
		return err
	}
	fmt.Printf("Laya loaded in %.1f s\n", time.Since(start).Seconds())
	_, err := guard.RunEval(ctx, g, os.Stdout, *verbose)
	return err
}
