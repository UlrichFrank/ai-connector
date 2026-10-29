//go:build !linux && !windows

package engine

import "syscall"

func dieWithParent(*syscall.SysProcAttr) {}
