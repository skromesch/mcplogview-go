package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/logs"
	"github.com/skromesch/mcplogview-go/internal/serverlog"
)

func connect(t *testing.T, logPath string) *mcp.ClientSession {
	t.Helper()
	cfg := config.Defaults()
	cfg.Logging.Enabled = false
	cfg.SkillsDir = t.TempDir()
	cfg.Sources = []config.Source{{Type: "local", ID: "app", Label: "App", Hint: "h", Path: logPath}}
	rt := NewRuntime(cfg, serverlog.Noop())

	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	srv := New(rt)
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content %T", res.Content[0])
	}
	return tc.Text, res.IsError
}

func TestToolsListed(t *testing.T) {
	cs := connect(t, filepath.Join(t.TempDir(), "x.log"))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "follow_log,get_log_skill,list_logs,read_log_chunk" {
		// order is not guaranteed; compare as a set
		want := map[string]bool{}
		for _, n := range ToolNames {
			want[n] = true
		}
		for _, n := range names {
			delete(want, n)
		}
		if len(want) != 0 || len(names) != len(ToolNames) {
			t.Fatalf("tools: %v", names)
		}
	}
}

func TestListAndReadAndErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.log")
	_ = os.WriteFile(p, []byte("a\nb\nc\n"), 0o644)
	cs := connect(t, p)

	text, isErr := call(t, cs, "list_logs", nil)
	if isErr || !strings.Contains(text, `"id":"app"`) || !strings.Contains(text, `"hint":"h"`) || strings.Contains(text, `"description"`) {
		t.Fatalf("list_logs: %s", text)
	}

	text, isErr = call(t, cs, "read_log_chunk", map[string]any{"sourceId": "app", "maxLines": 2})
	var chunk logs.Chunk
	if err := json.Unmarshal([]byte(text), &chunk); err != nil || isErr {
		t.Fatalf("read: %s %v", text, err)
	}
	if strings.Join(chunk.Lines, ",") != "a,b" || chunk.EOF {
		t.Fatalf("%+v", chunk)
	}

	text, _ = call(t, cs, "read_log_chunk", map[string]any{"sourceId": "app", "cursor": chunk.Cursor})
	_ = json.Unmarshal([]byte(text), &chunk)
	if strings.Join(chunk.Lines, ",") != "c" || !chunk.EOF {
		t.Fatalf("%+v", chunk)
	}

	text, isErr = call(t, cs, "read_log_chunk", map[string]any{"sourceId": "nope"})
	if !isErr || text != "Tool error (read_log_chunk): unknown source: nope" {
		t.Fatalf("unknown source: %q %v", text, isErr)
	}
	if _, isErr = call(t, cs, "read_log_chunk", map[string]any{"sourceId": "app", "maxLines": 9999}); !isErr {
		t.Fatal("out-of-range maxLines must be a tool error")
	}
	if text, isErr = call(t, cs, "get_log_skill", map[string]any{"sourceId": "app"}); isErr || !strings.Contains(text, `"origin":"fallback"`) {
		t.Fatalf("skill: %s", text)
	}
}

func TestFollowLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.log")
	_ = os.WriteFile(p, []byte("a\n"), 0o644)
	cs := connect(t, p)

	text, _ := call(t, cs, "read_log_chunk", map[string]any{"sourceId": "app"})
	var chunk logs.Chunk
	_ = json.Unmarshal([]byte(text), &chunk)

	// timeout with nothing new -> empty, eof
	start := time.Now()
	text, isErr := call(t, cs, "follow_log", map[string]any{"sourceId": "app", "cursor": chunk.Cursor, "timeoutMs": 150, "pollIntervalMs": 20})
	var res logs.Chunk
	_ = json.Unmarshal([]byte(text), &res)
	if isErr || len(res.Lines) != 0 || !res.EOF || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("idle follow: %s after %v", text, time.Since(start))
	}

	// new line arrives while following
	go func() {
		time.Sleep(60 * time.Millisecond)
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		_, _ = f.WriteString("fresh\n")
		_ = f.Close()
	}()
	text, _ = call(t, cs, "follow_log", map[string]any{"sourceId": "app", "cursor": chunk.Cursor, "timeoutMs": 3000, "pollIntervalMs": 20})
	_ = json.Unmarshal([]byte(text), &res)
	if strings.Join(res.Lines, ",") != "fresh" {
		t.Fatalf("follow: %s", text)
	}
}

// Tool and sourceId descriptions cover plugin sources, not only configured ones.
func TestToolDescriptionsCoverPluginSources(t *testing.T) {
	cs := connect(t, filepath.Join(t.TempDir(), "x.log"))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		schema, _ := json.Marshal(tool.InputSchema)
		all := tool.Description + " " + string(schema)
		if strings.Contains(all, "configured log source") {
			t.Fatalf("%s still says configured only: %s", tool.Name, all)
		}
		if tool.Name != "follow_log" && !strings.Contains(all, "plugin") {
			t.Fatalf("%s does not mention plugin sources: %s", tool.Name, all)
		}
	}
}
