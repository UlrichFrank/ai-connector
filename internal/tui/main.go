package tui

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	osuser "os/user"
	"strings"

	"golang.org/x/term"

	"github.com/philipparndt/ai-connector/internal/client"
)

// Main is `mlxlab tui [--server URL]`: it signs in at the hub if it has to (the token is kept in
// ~/.config/mlxlab/client.json) and runs the TUI over the API.
func Main(args []string) error {
	fs := flag.NewFlagSet("mlxlab tui", flag.ContinueOnError)
	server := fs.String("server", "", "the hub, or `mlxlab serve` at http://127.0.0.1:8484 (default: the last one, else "+client.DefaultServer+")")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := client.LoadConfig()
	if err != nil {
		return fmt.Errorf("%s: %w", client.ConfigPath(), err)
	}
	s := strings.TrimRight(*server, "/")
	if s == "" {
		s = strings.TrimRight(cfg.Server, "/")
	}
	if s == "" {
		s = client.DefaultServer
	}
	token := ""
	if strings.TrimRight(cfg.Server, "/") == s {
		token = cfg.Token // a token is only good at the hub that gave it
	}
	c := client.New(s, token)
	ctx := context.Background()
	info, err := c.Info(ctx)
	if err != nil {
		return explain(s, err)
	}
	// `mlxlab serve` listens on this Mac only and wants no sign-in
	if info.Hub() {
		need := token == ""
		if !need {
			_, err := c.Jobs(ctx)
			need = errors.Is(err, client.ErrSignIn) // revoked; an offline Mac is no reason to sign in again
		}
		if need {
			if c.Token, err = signIn(ctx, s); err != nil {
				return err
			}
			if err := client.SaveConfig(client.Config{Server: s, Token: c.Token}); err != nil {
				return fmt.Errorf("keeping the token: %w", err)
			}
		}
	}
	return run(c, info)
}

// explain says what went wrong reaching the server, in words.
func explain(server string, err error) error {
	switch {
	case errors.Is(err, client.ErrSignIn):
		return fmt.Errorf("%s: not signed in (start mlxlab tui again to sign in)", server)
	case errors.Is(err, client.ErrOffline):
		return fmt.Errorf("%s: the Mac is offline", server)
	}
	return fmt.Errorf("%s: %w", server, err)
}

// askUser asks who uses this device (the hub groups devices by it); Enter takes the Mac's user.
func askUser() string {
	def := ""
	if u, err := osuser.Current(); err == nil {
		def = strings.TrimSpace(strings.SplitN(u.Name, " ", 2)[0])
		if def == "" {
			def = u.Username
		}
	}
	fmt.Fprintf(os.Stderr, "Your name [%s]: ", def)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if line = strings.TrimSpace(line); line != "" {
		return line
	}
	return def
}

// signIn asks for the password (three tries) and returns the device token.
func signIn(ctx context.Context, server string) (string, error) {
	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	if host == "" {
		host = "mlxlab"
	}
	fmt.Fprintf(os.Stderr, "Sign in to %s\n", server)
	user := askUser()
	for range 3 {
		pw, err := readPassword("Password: ")
		if err != nil {
			return "", err
		}
		tok, err := client.Login(ctx, server, pw, host+" TUI", user)
		if errors.Is(err, client.ErrPassword) {
			fmt.Fprintln(os.Stderr, "Wrong password.")
			continue
		}
		if err != nil {
			return "", explain(server, err)
		}
		return tok, nil
	}
	return "", client.ErrPassword
}

// readPassword reads a line without echo from a terminal (from a pipe: a plain line, for scripts).
func readPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
