package main

import (
	"fmt"
	"os"
)

// version is set at build time (-ldflags "-X main.version=…"): the hub image builds this program for the Mac
// with the hub's version.
var version = "dev"

// `mlxlab version` prints it. Handled before run's flag parsing, so it needs no setup at all.
func init() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version)
		os.Exit(0)
	}
}
