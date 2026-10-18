package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/skromesch/mcplogview-go/internal/config"
)

func eventHost(t *testing.T, src string) (*Host, *memLogger, string) {
	t.Helper()
	dir := t.TempDir()
	writePlugin(t, dir, "ev", src)
	log := &memLogger{}
	h := NewHost(Options{Logger: log})
	t.Cleanup(h.Close)
	h.Reload(plugins(dir, config.PluginRef{Name: "ev"}))
	if s := statusOf(t, h, "ev"); !s.OK {
		t.Fatalf("load: %+v", s)
	}
	return h, log, dir
}

func TestBeforeToolCanDeny(t *testing.T) {
	h, _, _ := eventHost(t, `return { api_version = 1, setup = function()
  mcp.on("before_tool", function(e)
    if e.tool == "read_log_chunk" then return false, "no reads: " .. e.args end
  end)
end }`)
	allow, reason := h.BeforeTool(context.Background(), "read_log_chunk", "sourceId=x")
	if allow || reason != "no reads: sourceId=x" {
		t.Fatalf("%v %q", allow, reason)
	}
	if allow, _ := h.BeforeTool(context.Background(), "list_logs", ""); !allow {
		t.Fatal("other tool denied")
	}
}

func TestBeforeToolOnlyExactFalseDenies(t *testing.T) {
	h, _, _ := eventHost(t, `return { api_version = 1, setup = function()
  mcp.on("before_tool", function() return nil, "x" end)
  mcp.on("before_tool", function() return true end)
end }`)
	if allow, _ := h.BeforeTool(context.Background(), "t", ""); !allow {
		t.Fatal("denied")
	}
}

func TestBeforeToolHookErrorFailsOpen(t *testing.T) {
	h, log, _ := eventHost(t, `return { api_version = 1, setup = function()
  mcp.on("before_tool", function() error("boom", 0) end)
  mcp.on("before_tool", function() return false, "second" end)
end }`)
	allow, reason := h.BeforeTool(context.Background(), "t", "")
	if allow || reason != "second" {
		t.Fatalf("later hook must still run: %v %q", allow, reason)
	}
	if !strings.Contains(log.text(), "boom") {
		t.Fatalf("not logged: %s", log.text())
	}
}

func TestBeforeToolHookTimeoutFailsOpen(t *testing.T) {
	old := hookTimeout
	hookTimeout = 100 * time.Millisecond
	t.Cleanup(func() { hookTimeout = old })
	h, log, _ := eventHost(t, `return { api_version = 1, setup = function()
  mcp.on("before_tool", function() while true do end end)
end }`)
	start := time.Now()
	if allow, _ := h.BeforeTool(context.Background(), "t", ""); !allow {
		t.Fatal("timeout must allow")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("hook not bounded")
	}
	if !strings.Contains(log.text(), "before_tool") {
		t.Fatalf("not logged: %s", log.text())
	}
}

func TestAfterToolSeesErrorText(t *testing.T) {
	h, log, _ := eventHost(t, `return { api_version = 1, setup = function()
  mcp.on("after_tool", function(e) mcp.log.info("after " .. e.tool .. " [" .. e.args .. "] err=" .. tostring(e.error)) end)
end }`)
	h.AfterTool(context.Background(), "read_log_chunk", "sourceId=x", errors.New("bad thing"))
	h.AfterTool(context.Background(), "list_logs", "", nil)
	txt := log.text()
	if !strings.Contains(txt, "after read_log_chunk [sourceId=x] err=bad thing") || !strings.Contains(txt, "after list_logs [] err=nil") {
		t.Fatalf("log: %s", txt)
	}
}

func TestShutdownFiresOnClose(t *testing.T) {
	h, log, _ := eventHost(t, `return { api_version = 1, setup = function()
  mcp.on("shutdown", function(...) mcp.log.info("bye " .. select("#", ...)) end)
end }`)
	h.Close()
	h.Close()
	if n := strings.Count(log.text(), "bye 0"); n != 1 {
		t.Fatalf("shutdown fired %d times: %s", n, log.text())
	}
}

func TestConfigReloadFiresOnSecondLoadOnly(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "ev", `return { api_version = 1, setup = function()
  mcp.on("config_reload", function(e) mcp.log.info("reloaded sources=" .. e.sources) end)
end }`)
	writePlugin(t, dir, "bad", `return { api_version = 1, setup = function()
  mcp.on("config_reload", function() mcp.log.info("bad reloaded") end)
  error("nope", 0)
end }`)
	log := &memLogger{}
	h := NewHost(Options{Logger: log})
	t.Cleanup(h.Close)
	cfg := plugins(dir, config.PluginRef{Name: "ev"}, config.PluginRef{Name: "bad"})
	cfg.Sources = []config.Source{{ID: "a"}}
	h.Reload(cfg)
	if strings.Contains(log.text(), "reloaded") {
		t.Fatalf("fired on first load: %s", log.text())
	}
	h.Reload(cfg)
	txt := log.text()
	if strings.Count(txt, "reloaded sources=1") != 1 || strings.Contains(txt, "bad reloaded") {
		t.Fatalf("log: %s", txt)
	}
}

func TestUnknownEventRejected(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "ev", `return { api_version = 1, setup = function()
  mcp.on("before_tool", function() return false, "leak" end)
  mcp.on("nope", function() end)
end }`)
	h := NewHost(Options{})
	t.Cleanup(h.Close)
	h.Reload(plugins(dir, config.PluginRef{Name: "ev"}))
	s := statusOf(t, h, "ev")
	if s.OK || !strings.Contains(s.Error, `unknown event "nope"`) {
		t.Fatalf("%+v", s)
	}
	if allow, _ := h.BeforeTool(context.Background(), "t", ""); !allow {
		t.Fatal("hook of failed plugin survived")
	}
}

// AfterToolAsync returns at once, survives the caller's cancelled ctx, and
// WaitAfterTool waits for the hooks it dispatched.
func TestAfterToolAsyncDetachedAndWaitable(t *testing.T) {
	h, log, _ := eventHost(t, `return { api_version = 1, setup = function()
  mcp.on("after_tool", function(e) mcp.log.info("after " .. e.tool) end)
end }`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the request is already gone
	h.AfterToolAsync(ctx, "list_logs", "", nil)
	h.WaitAfterTool()
	if !strings.Contains(log.text(), "after list_logs") {
		t.Fatalf("after_tool lost for a cancelled request: %s", log.text())
	}
}

func TestAfterToolAsyncDoesNotBlockOnSlowHook(t *testing.T) {
	h, _, _ := eventHost(t, `return { api_version = 1, setup = function()
  mcp.on("after_tool", function() while true do end end)
end }`)
	start := time.Now()
	h.AfterToolAsync(context.Background(), "list_logs", "", nil)
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("AfterToolAsync blocked for %v", d)
	}
	// Close aborts the running hook and waits for it: no goroutine outlives it.
	start = time.Now()
	h.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close waited %v for a hanging after_tool hook", d)
	}
	h.AfterToolAsync(context.Background(), "list_logs", "", nil) // after Close: a no-op
	h.WaitAfterTool()
}

// Async after_tool hooks racing Reload and Close (run with -race).
func TestAfterToolAsyncRacesReloadAndClose(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "ev", `return { api_version = 1, setup = function()
  mcp.on("after_tool", function(e) local n = 0 for i = 1, 1000 do n = n + i end end)
end }`)
	h := NewHost(Options{Logger: &memLogger{}})
	cfg := plugins(dir, config.PluginRef{Name: "ev"})
	h.Reload(cfg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			h.AfterToolAsync(context.Background(), "t", "", nil)
		}
	}()
	for i := 0; i < 5; i++ {
		h.Reload(cfg)
	}
	<-done
	h.Close()
	h.WaitAfterTool()
}
