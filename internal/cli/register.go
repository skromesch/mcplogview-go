package cli

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// registration describes how an agent connects to this server.
type registration struct {
	bin        string           // path of the mcplogview binary (stdio)
	configPath string           // explicit --config for stdio, may be empty
	transport  config.Transport // effective transport settings
	token      string           // effective auth token, may be empty
}

func (r registration) isHTTP() bool { return r.transport.Mode != config.TransportStdio }

// url is the address clients should use. A wildcard listen address is replaced
// by this machine's host name, since 0.0.0.0 is not something a client can dial.
func (r registration) url() string {
	scheme := "http"
	if r.transport.Mode == config.TransportHTTPSDev {
		scheme = "https"
	}
	host := strings.Trim(r.transport.Host, "[]")
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		if h, err := os.Hostname(); err == nil {
			host = h
		} else {
			host = "<this-machine>"
		}
	}
	return fmt.Sprintf("%s://%s%s", scheme, net.JoinHostPort(host, strconv.Itoa(r.transport.Port)), r.transport.Path)
}

// claudeArgs are the arguments for `claude mcp add`.
func (r registration) claudeArgs() []string {
	if r.isHTTP() {
		args := []string{"mcp", "add", "--transport", "http", "mcplogview", r.url()}
		if r.token != "" {
			args = append(args, "--header", "Authorization: Bearer "+r.token)
		}
		return args
	}
	args := []string{"mcp", "add", "mcplogview", "--", r.bin}
	if r.configPath != "" {
		args = append(args, "serve", "--config", r.configPath)
	}
	return args
}

func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t\"'$`\\") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

// print writes human-readable registration instructions.
func (r registration) print(w io.Writer) {
	if r.isHTTP() {
		fmt.Fprintf(w, "This server runs over HTTP: start it once (e.g. in tmux or as a service) with\n\n  mcplogview serve\n\n")
		fmt.Fprintf(w, "Claude Code:\n\n  claude %s\n\n", shellJoin(r.claudeArgs()[0:]))
		fmt.Fprintf(w, "Other MCP clients (the exact JSON shape differs per client):\n\n")
		fmt.Fprintf(w, "  \"mcplogview\": { \"type\": \"http\", \"url\": %q", r.url())
		if r.token != "" {
			fmt.Fprintf(w, ", \"headers\": { \"Authorization\": %q }", "Bearer "+r.token)
		}
		fmt.Fprintf(w, " }\n\n")
		if r.transport.Mode == config.TransportHTTPSDev {
			fmt.Fprintln(w, "Note: https-dev uses a self-signed certificate; clients must be told to trust it.")
		} else if !r.transport.IsLoopback() {
			fmt.Fprintln(w, "Note: plain HTTP sends the token unencrypted. Use it only on a trusted network,")
			fmt.Fprintln(w, "through an SSH tunnel, or behind a TLS-terminating reverse proxy.")
		}
		if r.token == "" {
			fmt.Fprintln(w, "Note: no auth token is set; anyone who can reach the port can read your logs.")
		}
		return
	}

	fmt.Fprintf(w, "Claude Code (run inside the project; it picks up ./mcplogview.json from there):\n\n  claude %s\n\n", shellJoin(r.claudeArgs()))
	fmt.Fprintf(w, "Other MCP clients (Claude Desktop, Cursor, ...), add to their mcpServers config:\n\n")
	jsonArgs := ""
	if r.configPath != "" {
		jsonArgs = fmt.Sprintf(`, "args": ["serve", "--config", %q]`, r.configPath)
	}
	fmt.Fprintf(w, "  \"mcplogview\": { \"command\": %q%s }\n\n", r.bin, jsonArgs)
	if r.configPath == "" {
		fmt.Fprintln(w, "Without --config the server uses ./mcplogview.json of the directory the agent starts it in,")
		fmt.Fprintln(w, "then ~/.config/mcplogview/config.json. For a fixed file: mcplogview register --config FILE")
		fmt.Fprintln(w, "To run one central instance for everyone instead: mcplogview transport --mode http")
	}
}

// currentRegistration reads the effective config (and env token) to describe
// how agents should connect.
func currentRegistration(configPath string) (registration, error) {
	bin, err := os.Executable()
	if err != nil {
		bin = "mcplogview"
	}
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}
	cwd, _ := os.Getwd()
	cfg, _, _, err := config.LoadEffective(configPath, os.Getenv, cwd)
	if err != nil {
		return registration{}, err
	}
	reg := registration{
		bin:       bin,
		transport: cfg.Transport,
		token:     config.ResolveToken(cfg.Transport.AuthToken, os.Getenv),
	}
	if configPath != "" {
		reg.configPath = absPath(configPath)
	}
	return reg, nil
}

func runRegister(args []string, stdout, stderr io.Writer) int {
	fs, configPath := newFlags("register", stderr)
	run := fs.Bool("run", false, "run `claude mcp add` now")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	reg, err := currentRegistration(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}

	if *run {
		if _, err := exec.LookPath("claude"); err != nil {
			fmt.Fprintln(stderr, "mcplogview: `claude` CLI not found in PATH")
			return 1
		}
		cmd := exec.Command("claude", reg.claudeArgs()...)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode()
			}
			fmt.Fprintln(stderr, "mcplogview:", err)
			return 1
		}
		return 0
	}
	reg.print(stdout)
	return 0
}
