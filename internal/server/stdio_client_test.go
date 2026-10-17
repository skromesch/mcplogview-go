package server

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Drives the real binary the way an agent host does: subprocess + stdio.
func TestRealBinaryOverStdio(t *testing.T) {
	bin, dir := os.Getenv("MCPLOGVIEW_BIN"), os.Getenv("MCPLOGVIEW_DIR")
	if bin == "" {
		t.Skip("MCPLOGVIEW_BIN not set")
	}
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_log_chunk", Arguments: map[string]any{"sourceId": "a"}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, `"e1","e2"`) {
		t.Fatalf("%s", text)
	}
}
