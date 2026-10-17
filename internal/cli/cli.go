// Package cli implements the mcplogview command line: the MCP server and the
// configuration commands (add, list, remove, check, configure, ...).
package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/skromesch/mcplogview-go/internal/buildinfo"
)

const usage = `mcplogview - MCP server exposing local, SSH and Docker logs to AI agents

Usage:
  mcplogview                       show this help (when run in a terminal)
  mcplogview serve [--config FILE] run the MCP server (this is what agents start)

Configure:
  mcplogview configure             interactive setup: browse files/containers, add sources
  mcplogview add local  --id ID --path FILE
  mcplogview add ssh    --id ID --host H --user U --identity-file KEY --path REMOTE_FILE
  mcplogview add docker --id ID --container NAME [--host H [--user U] [--identity-file KEY]]
  mcplogview list                  show the effective sources
  mcplogview enable|disable ID...  switch sources on/off without deleting them (or --all)
  mcplogview remove ID             delete a source
  mcplogview check [ID...]         test that sources are reachable and readable
  mcplogview transport [--mode stdio|http|https-dev] [--host H] [--port N] [--new-token]
                                   show or change how the server is reached (HTTP = one central instance)
  mcplogview register [--run]      show (or run) how to register with Claude Code / other agents
  mcplogview init [--force]        write an example ./mcplogview.json
  mcplogview where                 print which config file is in effect
  mcplogview version

Common flags: --config FILE   use/edit this config file

Config lookup (first match wins):
  1. --config FILE
  2. $LOG_SOURCES_CONFIG
  3. ./mcplogview.json                 (current directory, per project)
  4. ~/.config/mcplogview/config.json  (user-wide)
  5. built-in defaults (only the server's own log)

HTTP mode (one central instance): mcplogview transport --mode http, then mcplogview serve.
Network-facing modes require a bearer token (generated for you); $MCPLOGVIEW_TOKEN overrides it.

A running server notices config changes within a few seconds (no restart), so a
central instance always serves the sources that are currently enabled.

Edit commands (add/remove/enable/disable/configure) write to the config found above, or create
./mcplogview.json. Relative paths in a config resolve against that file's directory.

Source types:
  local   a file on this machine
  ssh     a file on a remote host (system ssh, BatchMode: key or ssh-agent, no password prompt)
  docker  a container's logs, local or on a remote host over ssh (needs the docker CLI there)

In stdio mode stdout carries MCP JSON-RPC only; diagnostics go to stderr and the
self-log (default ~/.local/state/mcplogview/mcplogview-server.log).
MCP tools: list_logs, get_log_skill, read_log_chunk, follow_log
`

// stdinIsTerminal reports whether stdin is an interactive terminal. Agents
// start the server with a pipe, a human running the binary has a terminal.
var stdinIsTerminal = func() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Run executes the command line and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		if stdinIsTerminal() {
			fmt.Fprint(stdout, usage)
			return 0
		}
		return runServe(nil, stderr)
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help", "-help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "-version", "--version":
		fmt.Fprintf(stdout, "mcplogview %s\n", buildinfo.String())
		return 0
	case "serve":
		return runServe(rest, stderr)
	case "init":
		return runInit(rest, stderr)
	case "where":
		return runWhere(rest, stdout, stderr)
	case "list", "ls":
		return runList(rest, stdout, stderr)
	case "add":
		return runAdd(rest, stdout, stderr)
	case "enable", "on":
		return runToggle(rest, false, stdout, stderr)
	case "disable", "off":
		return runToggle(rest, true, stdout, stderr)
	case "remove", "rm":
		return runRemove(rest, stdout, stderr)
	case "check":
		return runCheck(rest, stdout, stderr)
	case "register":
		return runRegister(rest, stdout, stderr)
	case "transport", "mode":
		return runTransport(rest, stdout, stderr)
	case "configure", "config":
		return runConfigure(rest, stdout, stderr)
	}
	if strings.HasPrefix(cmd, "-") {
		// legacy invocation: `mcplogview --config FILE` means serve
		return runServe(args, stderr)
	}
	fmt.Fprintf(stderr, "mcplogview: unknown command %q\n\n%s", cmd, usage)
	return 2
}
