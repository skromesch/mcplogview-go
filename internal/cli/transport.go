package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/configure"
)

func maskToken(t string) string {
	switch {
	case t == "":
		return "none"
	case len(t) <= 8:
		return "set"
	}
	return t[:4] + "…" + t[len(t)-4:] + " (hidden; --show-token to reveal)"
}

// printTransport summarises the transport settings.
func printTransport(w io.Writer, t config.Transport, showToken bool) {
	fmt.Fprintf(w, "mode:   %s\n", t.Mode)
	if t.Mode == config.TransportStdio {
		fmt.Fprintln(w, "        (each agent starts its own mcplogview process; no network port)")
		return
	}
	fmt.Fprintf(w, "listen: %s:%d%s\n", t.Host, t.Port, t.Path)
	tok := maskToken(t.AuthToken)
	if showToken && t.AuthToken != "" {
		tok = t.AuthToken
	}
	if env := os.Getenv(config.EnvToken); env != "" {
		tok = "from $" + config.EnvToken + " (overrides the config)"
	}
	fmt.Fprintf(w, "token:  %s\n", tok)
}

// transportChange is a requested edit of the transport settings.
type transportChange struct {
	mode, host, path string
	port             int
	newToken         bool
	noToken          bool
	allowNoAuth      bool
}

func (c transportChange) any() bool {
	return c.mode != "" || c.host != "" || c.port != 0 || c.path != "" || c.newToken || c.noToken || c.allowNoAuth
}

// errUsage marks problems with the requested change (exit code 2).
var errUsage = errors.New("usage")

func usageErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(format, a...))
}

// applyTransport validates a change against the current settings, enforces
// that a network-facing mode is protected, writes it and saves the file. It
// returns the new settings and whether a token was generated.
//
// Secure by default: switching to http/https-dev generates a token unless one
// exists or the caller explicitly allows running without.
func applyTransport(file *configure.File, c transportChange) (next config.Transport, generated bool, err error) {
	cur, err := file.Transport()
	if err != nil {
		return cur, false, err
	}
	if c.newToken && c.noToken {
		return cur, false, usageErr("--new-token and --no-token are mutually exclusive")
	}

	next = cur
	changes := map[string]any{}
	if c.mode != "" {
		switch c.mode {
		case config.TransportStdio, config.TransportHTTP, config.TransportHTTPSDev:
		default:
			return cur, false, usageErr("mode must be stdio, http or https-dev (got %q)", c.mode)
		}
		next.Mode, changes["mode"] = c.mode, c.mode
	}
	if c.host != "" {
		next.Host, changes["host"] = c.host, c.host
	}
	if c.port != 0 {
		if c.port < 1 || c.port > 65535 {
			return cur, false, usageErr("port out of range: %d", c.port)
		}
		next.Port, changes["port"] = c.port, c.port
	}
	if c.path != "" {
		next.Path, changes["path"] = c.path, c.path
	}
	if c.allowNoAuth {
		next.AllowNoAuth, changes["allowNoAuth"] = true, true
	}

	switch {
	case c.noToken:
		next.AuthToken, changes["authToken"] = "", nil
	case c.newToken || (next.Mode != config.TransportStdio && next.AuthToken == "" && !c.allowNoAuth):
		tok, err := configure.NewToken()
		if err != nil {
			return cur, false, err
		}
		next.AuthToken, changes["authToken"] = tok, tok
		generated = true
	}

	if next.Mode != config.TransportStdio && !next.IsLoopback() && next.AuthToken == "" && !next.AllowNoAuth {
		return cur, false, usageErr("refusing to expose %s:%d without a token (use --new-token, or --allow-no-auth on a trusted network)", next.Host, next.Port)
	}

	if err := file.UpdateTransport(changes); err != nil {
		return cur, false, err
	}
	if err := file.Save(); err != nil {
		return cur, false, err
	}
	return next, generated, nil
}

func runTransport(args []string, stdout, stderr io.Writer) int {
	fs, configPath := newFlags("transport", stderr)
	var c transportChange
	fs.StringVar(&c.mode, "mode", "", "stdio, http or https-dev")
	fs.StringVar(&c.host, "host", "", "listen address (127.0.0.1 = this machine only, 0.0.0.0 = network)")
	fs.IntVar(&c.port, "port", 0, "listen port")
	fs.StringVar(&c.path, "path", "", "URL path (default /mcp)")
	fs.BoolVar(&c.newToken, "new-token", false, "generate a new auth token (replaces the old one)")
	fs.BoolVar(&c.noToken, "no-token", false, "remove the auth token")
	fs.BoolVar(&c.allowNoAuth, "allow-no-auth", false, "allow listening on the network without a token")
	showTok := fs.Bool("show-token", false, "print the auth token")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	target, err := targetPath(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	file, err := configure.Open(target)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}

	if !c.any() {
		cur, err := file.Transport()
		if err != nil {
			fmt.Fprintln(stderr, "mcplogview:", err)
			return 1
		}
		fmt.Fprintf(stdout, "config: %s\n", target)
		printTransport(stdout, cur, *showTok)
		if cur.Mode != config.TransportStdio {
			fmt.Fprintln(stdout)
			reg, _ := currentRegistration(*configPath)
			reg.transport, reg.token = cur, config.ResolveToken(cur.AuthToken, os.Getenv)
			reg.print(stdout)
		}
		return 0
	}

	next, generated, err := applyTransport(file, c)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		if errors.Is(err, errUsage) {
			return 2
		}
		return 1
	}

	fmt.Fprintf(stdout, "saved %s\n", target)
	printTransport(stdout, next, generated || *showTok)
	if generated {
		fmt.Fprintln(stdout, "\nA new token was generated. Clients must send it as: Authorization: Bearer <token>")
	}
	fmt.Fprintln(stdout, "\nNote: a running server needs a restart for mode/host/port/path changes; the token is picked up live.")
	if next.Mode != config.TransportStdio {
		fmt.Fprintln(stdout)
		reg, _ := currentRegistration(*configPath)
		reg.transport, reg.token = next, config.ResolveToken(next.AuthToken, os.Getenv)
		reg.print(stdout)
	}
	return 0
}
