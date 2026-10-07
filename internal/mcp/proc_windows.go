//go:build windows

package mcp

import "os/exec"

// ownGroup: Windows has no process groups to signal; the server is stopped on its own.
func ownGroup(*exec.Cmd) {}

func terminateGroup(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
func killGroup(cmd *exec.Cmd)      { _ = cmd.Process.Kill() }
