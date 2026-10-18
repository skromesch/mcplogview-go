package plugin

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/skromesch/mcplogview-go/internal/config"
)

type fakeSink struct {
	mu    sync.Mutex
	tools map[string]fakeTool
	ops   []string
}

type fakeTool struct {
	def  ToolDef
	call func(ctx context.Context, args map[string]any) (any, error)
}

func newFakeSink() *fakeSink { return &fakeSink{tools: map[string]fakeTool{}} }

func (s *fakeSink) Add(def ToolDef, call func(ctx context.Context, args map[string]any) (any, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[def.Name] = fakeTool{def, call}
	s.ops = append(s.ops, "add "+def.Name)
}

func (s *fakeSink) Remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tools, name)
	s.ops = append(s.ops, "remove "+name)
}

func (s *fakeSink) get(t *testing.T, name string) fakeTool {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	tl, ok := s.tools[name]
	if !ok {
		t.Fatalf("tool %q not in sink (have %v)", name, s.ops)
	}
	return tl
}

func (s *fakeSink) names() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n []string
	for k := range s.tools {
		n = append(n, k)
	}
	return strings.Join(sorted(n), ",")
}

func sorted(a []string) []string {
	for i := range a {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
	return a
}

func toolPlugin(body string) string {
	return `return { api_version = 1, setup = function() ` + body + ` end }`
}

const echoTool = `mcp.tool.register{ name = "echo", description = "Echo", handler = function(a) return { got = a.x } end }`

func hostWithSink(t *testing.T, o Options, cfg config.Config) (*Host, *fakeSink) {
	t.Helper()
	h := NewHost(o)
	t.Cleanup(h.Close)
	s := newFakeSink()
	h.SetSink(s)
	h.Reload(cfg)
	return h, s
}

func TestToolRegisteredAndCallable(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "p", toolPlugin(echoTool))
	_, s := hostWithSink(t, Options{}, plugins(dir, config.PluginRef{Name: "p"}))
	tl := s.get(t, "echo")
	if tl.def.Plugin != "p" || tl.def.Description != "Echo" || tl.def.Schema["type"] != "object" {
		t.Fatalf("def: %+v", tl.def)
	}
	res, err := tl.call(context.Background(), map[string]any{"x": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := res.(map[string]any); m["got"] != "hi" {
		t.Fatalf("result: %#v", res)
	}
}

func TestSetSinkReplaysRegisteredTools(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "p", toolPlugin(echoTool))
	h := newTestHost(t, plugins(dir, config.PluginRef{Name: "p"}))
	s := newFakeSink()
	h.SetSink(s)
	s.get(t, "echo")
}

func TestReloadRemovesPreviousToolsFirst(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "p", toolPlugin(echoTool))
	h, s := hostWithSink(t, Options{}, plugins(dir))
	if s.names() != "" {
		t.Fatalf("disabled plugin exposed: %s", s.names())
	}
	h.Reload(plugins(dir, config.PluginRef{Name: "p"}))
	if s.names() != "echo" {
		t.Fatalf("names: %s", s.names())
	}
	h.Reload(plugins(dir))
	if s.names() != "" {
		t.Fatalf("stale tool: %s (%v)", s.names(), s.ops)
	}
}

func TestFailedPluginToolsNeverExposed(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "bad", toolPlugin(echoTool+` error("boom")`))
	writePlugin(t, dir, "bad2", toolPlugin(echoTool+` mcp.tool.register{ name = "Bad Name", handler = function() end }`))
	h, s := hostWithSink(t, Options{}, plugins(dir, config.PluginRef{Name: "bad"}, config.PluginRef{Name: "bad2"}))
	for _, n := range []string{"bad", "bad2"} {
		if statusOf(t, h, n).OK {
			t.Fatalf("%s loaded", n)
		}
	}
	if len(s.ops) != 0 {
		t.Fatalf("sink touched: %v", s.ops)
	}
}

func regError(t *testing.T, o Options, body string, others ...string) string {
	t.Helper()
	dir := t.TempDir()
	refs := []config.PluginRef{}
	for i, src := range others {
		n := "other" + string(rune('a'+i))
		writePlugin(t, dir, n, toolPlugin(src))
		refs = append(refs, config.PluginRef{Name: n})
	}
	writePlugin(t, dir, "p", toolPlugin(body))
	refs = append(refs, config.PluginRef{Name: "p"})
	h := NewHost(o)
	t.Cleanup(h.Close)
	h.Reload(plugins(dir, refs...))
	s := statusOf(t, h, "p")
	if s.OK {
		t.Fatalf("expected failure for %s", body)
	}
	return s.Error
}

func TestToolRegistrationRejections(t *testing.T) {
	res := Options{ReservedToolNames: []string{"read_log_chunk"}}
	h := `handler = function() end`
	cases := []struct{ name, body, want string }{
		{"reserved", `mcp.tool.register{ name = "read_log_chunk", ` + h + ` }`, "read_log_chunk"},
		{"bad name", `mcp.tool.register{ name = "Bad", ` + h + ` }`, "invalid name"},
		{"no handler", `mcp.tool.register{ name = "ok" }`, "handler"},
		{"duplicate", `mcp.tool.register{ name = "ok", ` + h + ` } mcp.tool.register{ name = "ok", ` + h + ` }`, "twice"},
		{"array schema", `mcp.tool.register{ name = "ok", schema = { type = "array" }, ` + h + ` }`, `"object"`},
		{"bad schema", `mcp.tool.register{ name = "ok", schema = { type = "object", properties = 5 }, ` + h + ` }`, "schema"},
		{"schema not table", `mcp.tool.register{ name = "ok", schema = "x", ` + h + ` }`, "schema"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if msg := regError(t, res, c.body); !strings.Contains(msg, c.want) {
				t.Fatalf("error %q lacks %q", msg, c.want)
			}
		})
	}
	t.Run("other plugin's tool", func(t *testing.T) {
		msg := regError(t, res, `mcp.tool.register{ name = "echo", `+h+` }`, echoTool)
		if !strings.Contains(msg, "already") || !strings.Contains(msg, "othera") {
			t.Fatal(msg)
		}
	})
}

func TestToolOutsideLoadRejected(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "p", toolPlugin(`mcp.tool.register{ name = "late", handler = function() mcp.tool.register{ name = "x", handler = function() end } end }`))
	_, s := hostWithSink(t, Options{}, plugins(dir, config.PluginRef{Name: "p"}))
	_, err := s.get(t, "late").call(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "only allowed while the plugin loads") {
		t.Fatalf("err: %v", err)
	}
}

func TestToolArgsValidatedBeforeHandler(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "p", toolPlugin(`ran = 0
mcp.tool.register{ name = "needs", schema = { type = "object", required = { "q" },
  properties = { q = { type = "string" } } },
  handler = function(a) ran = ran + 1 return { ran = ran } end }`))
	_, s := hostWithSink(t, Options{}, plugins(dir, config.PluginRef{Name: "p"}))
	tl := s.get(t, "needs")
	for _, args := range []map[string]any{nil, {"q": 5.0}} {
		if _, err := tl.call(context.Background(), args); err == nil {
			t.Fatalf("args %v accepted", args)
		}
	}
	res, err := tl.call(context.Background(), map[string]any{"q": "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if m := res.(map[string]any); m["ran"] != 1.0 {
		t.Fatalf("handler ran before validation passed: %v", m)
	}
}

func TestToolHandlerErrorCarriesLuaMessage(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "p", toolPlugin(`mcp.tool.register{ name = "boom", handler = function() error("kaboom", 0) end }`))
	_, s := hostWithSink(t, Options{}, plugins(dir, config.PluginRef{Name: "p"}))
	_, err := s.get(t, "boom").call(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("err: %v", err)
	}
}

func TestToolUnconvertibleResultIsError(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "p", toolPlugin(`mcp.tool.register{ name = "fn", handler = function() return function() end end }`))
	_, s := hostWithSink(t, Options{}, plugins(dir, config.PluginRef{Name: "p"}))
	if _, err := s.get(t, "fn").call(context.Background(), nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestSinkMirrorsToolsUnderConcurrentReloadAndSetSink(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "p", toolPlugin(echoTool))
	h := newTestHost(t, plugins(dir, config.PluginRef{Name: "p"}))
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				h.SetSink(newFakeSink())
			}
		}()
	}
	for i := 0; i < 5; i++ {
		h.Reload(plugins(dir, config.PluginRef{Name: "p"}))
	}
	wg.Wait()
	s := newFakeSink()
	h.SetSink(s)
	h.Reload(plugins(dir))
	if s.names() != "" {
		t.Fatalf("stale: %s", s.names())
	}
}
