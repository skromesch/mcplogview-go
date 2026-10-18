package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/serverlog"
)

type lockedLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *lockedLogger) Info(m string)             { l.add(m) }
func (l *lockedLogger) Error(m string, err error) { l.add(m + " | " + err.Error()) }
func (l *lockedLogger) Close()                    {}
func (l *lockedLogger) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, s)
}
func (l *lockedLogger) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

var _ serverlog.Logger = (*lockedLogger)(nil)

func connectCfgLog(t *testing.T, cfg config.Config, log serverlog.Logger) (*mcp.ClientSession, *Runtime) {
	t.Helper()
	rt := NewRuntime(cfg, log)
	t.Cleanup(rt.Close)
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := New(rt).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, rt
}

const hookSrc = `return { api_version = 1, setup = function()
  mcp.tool.register{ name = "echo", handler = function(a) ran = (ran or 0) + 1 return { ran = ran } end }
  mcp.on("before_tool", function(e)
    if e.tool == "echo" or e.args:find("sourceId=blocked", 1, true) then return false, "policy" end
  end)
  mcp.on("after_tool", function(e) mcp.log.info("after " .. e.tool .. " " .. tostring(e.error)) end)
end }`

func TestBeforeToolDenialSkipsHandler(t *testing.T) {
	cfg, _ := toolsCfg(t, hookSrc)
	cs, _ := connectCfg(t, cfg)
	text, isErr := call(t, cs, "echo", nil)
	if !isErr || text != "Tool denied by plugin tp: policy" {
		t.Fatalf("%v %q", isErr, text)
	}
	text, isErr = call(t, cs, "read_log_chunk", map[string]any{"sourceId": "blocked"})
	if !isErr || text != "Tool denied by plugin tp: policy" {
		t.Fatalf("%v %q", isErr, text)
	}
}

func TestAfterToolFiresForBuiltinAndPluginTools(t *testing.T) {
	log := &lockedLogger{}
	cfg, _ := toolsCfg(t, strings.Replace(hookSrc, `e.tool == "echo" or `, "", 1))
	cs, rt := connectCfgLog(t, cfg, log)
	call(t, cs, "echo", nil)
	call(t, cs, "read_log_chunk", map[string]any{"sourceId": "missing"})
	rt.Plugins().WaitAfterTool() // after_tool runs in the background
	if log.count("after echo nil") != 1 || log.count("after read_log_chunk unknown source: missing") != 1 {
		t.Fatalf("log: %v", log.lines)
	}
}

// A slow after_tool hook must not hold back the tool's response.
func TestSlowAfterToolDoesNotDelayResponse(t *testing.T) {
	cfg, _ := toolsCfg(t, `return { api_version = 1, setup = function()
  mcp.on("after_tool", function() while true do end end)
end }`)
	cs, _ := connectCfgLog(t, cfg, &lockedLogger{})
	start := time.Now()
	if text, isErr := call(t, cs, "list_logs", nil); isErr {
		t.Fatal(text)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("response waited %v for a hanging after_tool hook", d)
	}
}
