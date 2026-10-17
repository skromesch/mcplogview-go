// Command mcplogview is an MCP server that exposes local, SSH and Docker logs
// to agents, plus the commands to configure it.
package main

import (
	"os"

	"github.com/skromesch/mcplogview-go/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
