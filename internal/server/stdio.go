package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/skromesch/mcplogview-go/internal/serverlog"
)

// RunStdio serves MCP over stdin/stdout. Nothing but JSON-RPC may be written
// to stdout; raw traffic is mirrored to the self-log only when debug is on.
func RunStdio(ctx context.Context, srv *mcp.Server, logger serverlog.Logger, debug bool, maxBodyChars int) error {
	var transport mcp.Transport = &mcp.StdioTransport{}
	if debug {
		transport = &mcp.LoggingTransport{Transport: transport, Writer: &logWriter{logger: logger, max: maxBodyChars}}
	}
	logger.Info("transport=stdio connected")
	return srv.Run(ctx, transport)
}

type logWriter struct {
	logger serverlog.Logger
	max    int
}

func (w *logWriter) Write(p []byte) (int, error) {
	text := strings.TrimRight(string(p), "\n")
	if len(text) > w.max {
		text = fmt.Sprintf("%s...[truncated %d chars]", text[:w.max], len(text)-w.max)
	}
	w.logger.Info("stdio raw=" + text)
	return len(p), nil
}
