package agents

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"syscall"
	"unicode/utf16"
)

// Windows' limits on starting a process (CreateProcess): the command line at most 32,767 UTF-16 characters with its
// terminating zero, one environment variable at most 32,767. Past them Windows says only "The filename or extension
// is too long" (ERROR_FILENAME_EXCED_RANGE) — without saying what was. So the start is checked first (checkArgs,
// checkEnv) with a message that names it, and a start that fails that way anyway says the sizes (startError): the
// command line, the environment and its largest variables, the program's path and the working folder.

// winMax is what Windows takes for the command line (without its zero) and for one environment variable.
const winMax = 32766

// winErrTooLong is ERROR_FILENAME_EXCED_RANGE ("The filename or extension is too long").
const winErrTooLong = syscall.Errno(206)

// winCommandLine is the command line Go builds on Windows for cmd and args (syscall.makeCmdLine: each quoted as
// syscall.EscapeArg does, joined by spaces).
func winCommandLine(cmd string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	for _, a := range append([]string{cmd}, args...) {
		parts = append(parts, winEscape(a))
	}
	return strings.Join(parts, " ")
}

// winEscape is syscall.EscapeArg (Windows), here so that it is checked on every system.
func winEscape(s string) string {
	if s == "" {
		return `""`
	}
	needsBackslash, hasSpace := false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			needsBackslash = true
		case ' ', '\t':
			hasSpace = true
		}
	}
	if !needsBackslash && !hasSpace {
		return s
	}
	if !needsBackslash {
		return `"` + s + `"`
	}
	var b strings.Builder
	if hasSpace {
		b.WriteByte('"')
	}
	slashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		default:
			slashes = 0
		case '\\':
			slashes++
		case '"':
			for ; slashes > 0; slashes-- {
				b.WriteByte('\\')
			}
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	if hasSpace {
		for ; slashes > 0; slashes-- {
			b.WriteByte('\\')
		}
		b.WriteByte('"')
	}
	return b.String()
}

// utf16Len is s's length as Windows counts it.
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// checkArgs: the command line fits (on Windows; elsewhere the limits are far larger).
func checkArgs(cmd string, args []string) error {
	if goos != "windows" {
		return nil
	}
	if n := utf16Len(winCommandLine(cmd, args)); n > winMax {
		return fmt.Errorf("the command line for claude is %d characters, more than Windows allows (%d)", n, winMax)
	}
	return nil
}

// checkEnv: no environment variable is longer than Windows takes (on Windows).
func checkEnv(env []string) error {
	if goos != "windows" {
		return nil
	}
	for _, kv := range env {
		if n := utf16Len(kv); n > winMax {
			name, _, _ := strings.Cut(kv, "=")
			return fmt.Errorf("the environment variable %s is %d characters, more than Windows allows (%d): shorten it "+
				"(System settings → Environment variables) and start the worker again", name, n, winMax)
		}
	}
	return nil
}

// startError is a failed start of claude, on Windows with the sizes when Windows says only that something is too
// long.
func startError(err error, cmd string, args, env []string, dir string) error {
	if goos != "windows" || !(errors.Is(err, winErrTooLong) || strings.Contains(err.Error(), "too long")) {
		return fmt.Errorf("starting claude: %w", err)
	}
	return fmt.Errorf("starting claude: %w — %s", err, startSizes(cmd, args, env, dir))
}

// startSizes says how large what Windows was given is, and what is usually the cause.
func startSizes(cmd string, args, env []string, dir string) string {
	type v struct {
		name string
		n    int
	}
	total := 1
	vars := make([]v, 0, len(env))
	for _, kv := range env {
		n := utf16Len(kv)
		total += n + 1
		name, _, _ := strings.Cut(kv, "=")
		vars = append(vars, v{name, n})
	}
	sort.Slice(vars, func(i, j int) bool { return vars[i].n > vars[j].n })
	var largest []string
	for _, x := range vars[:min(3, len(vars))] {
		largest = append(largest, fmt.Sprintf("%s %d", x.name, x.n))
	}
	return fmt.Sprintf("the command line is %d of %d characters, the environment %d characters in %d variables "+
		"(the largest: %s), claude's path %d and the working folder %d characters (%s); a path past 260 characters or a "+
		"very long variable such as PATH are the usual causes",
		utf16Len(winCommandLine(cmd, args)), winMax, total, len(env), strings.Join(largest, ", "), utf16Len(cmd), utf16Len(dir), dir)
}
