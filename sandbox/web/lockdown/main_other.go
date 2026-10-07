//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "lockdown runs only in the Linux VMs of the web sandbox")
	os.Exit(2)
}
