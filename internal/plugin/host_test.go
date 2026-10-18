package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// writePlugin writes <dir>/<name>.lua.
func writePlugin(t *testing.T, dir, name, src string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".lua"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rootPlugin is a minimal plugin registering one root id.
func rootPlugin(id string) string {
	return `return { api_version = 1, setup = function()
  mcp.source.register{ id = "` + id + `", label = "L", children = function() return {} end,
    read = function() return { lines = {}, eof = true } end }
end }`
}

type memLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *memLogger) Info(msg string) { l.add("INFO " + msg) }
func (l *memLogger) Error(msg string, err error) {
	l.add("ERROR " + msg + " | " + err.Error())
}
func (l *memLogger) Close() {}
func (l *memLogger) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, s)
}
func (l *memLogger) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func plugins(dir string, refs ...config.PluginRef) config.Config {
	return config.Config{PluginDirs: []string{dir}, Plugins: refs}
}

func newTestHost(t *testing.T, cfg config.Config) *Host {
	t.Helper()
	h := NewHost(Options{})
	t.Cleanup(h.Close)
	h.Reload(cfg)
	return h
}

func statusOf(t *testing.T, h *Host, name string) PluginStatus {
	t.Helper()
	for _, s := range h.Status() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no status for %s: %+v", name, h.Status())
	return PluginStatus{}
}

func rootIDs(h *Host) []string {
	var ids []string
	for _, r := range h.Roots() {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestLoadPluginFromFileAndDirectory(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "one", rootPlugin("one"))
	writePlugin(t, filepath.Join(dir, "two"), "init", rootPlugin("two"))
	h := newTestHost(t, plugins(dir, config.PluginRef{Name: "one"}, config.PluginRef{Name: "two"}))
	for _, n := range []string{"one", "two"} {
		if s := statusOf(t, h, n); !s.OK {
			t.Fatalf("%s: %+v", n, s)
		}
	}
	got := strings.Join(rootIDs(h), ",")
	if got != "one,two" {
		t.Fatalf("roots: %s", got)
	}
	r := h.Roots()[0]
	if r.Plugin != "one" || r.Label != "L" {
		t.Fatalf("root: %+v", r)
	}
	if !h.Owns("one") || !h.Owns("two/x/y") || h.Owns("three") || h.Owns("onex/y") {
		t.Fatal("Owns wrong")
	}
}

func TestPluginFileSearchOrder(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	writePlugin(t, filepath.Join(d1, "p"), "init", rootPlugin("first"))
	writePlugin(t, d1, "p", rootPlugin("flat"))
	writePlugin(t, d2, "p", rootPlugin("second"))
	cfg := config.Config{PluginDirs: []string{d1, d2}, Plugins: []config.PluginRef{{Name: "p"}}}
	h := newTestHost(t, cfg)
	if got := strings.Join(rootIDs(h), ","); got != "flat" {
		t.Fatalf("roots: %s", got)
	}
}

func TestPluginNotFound(t *testing.T) {
	h := newTestHost(t, plugins(t.TempDir(), config.PluginRef{Name: "nope"}))
	if s := statusOf(t, h, "nope"); s.OK || !strings.Contains(s.Error, "not found") {
		t.Fatalf("%+v", s)
	}
}

func TestPluginWrongAPIVersionFails(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "bad", `return { api_version = 2 }`)
	writePlugin(t, dir, "none", `return 42`)
	writePlugin(t, dir, "good", rootPlugin("good"))
	log := &memLogger{}
	h := NewHost(Options{Logger: log})
	defer h.Close()
	h.Reload(plugins(dir, config.PluginRef{Name: "bad"}, config.PluginRef{Name: "none"}, config.PluginRef{Name: "good"}))
	if s := statusOf(t, h, "bad"); s.OK || !strings.Contains(s.Error, "api_version") {
		t.Fatalf("%+v", s)
	}
	if s := statusOf(t, h, "none"); s.OK || !strings.Contains(s.Error, "api_version") {
		t.Fatalf("%+v", s)
	}
	if s := statusOf(t, h, "good"); !s.OK {
		t.Fatalf("%+v", s)
	}
	if !strings.Contains(log.text(), "plugin bad") {
		t.Fatalf("log: %s", log.text())
	}
}

func TestSyntaxErrorIsolated(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "broken", `return { api_version = 1 = }`)
	writePlugin(t, dir, "good", rootPlugin("good"))
	h := newTestHost(t, plugins(dir, config.PluginRef{Name: "broken"}, config.PluginRef{Name: "good"}))
	if s := statusOf(t, h, "broken"); s.OK || s.Error == "" {
		t.Fatalf("%+v", s)
	}
	if !statusOf(t, h, "good").OK || !h.Owns("good") {
		t.Fatal("good plugin not loaded")
	}
}

func TestSetupReceivesOpts(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "p", `return { api_version = 1, setup = function(opts)
  if opts.x ~= 5 or opts.nested.y ~= "z" then error("bad opts") end
  mcp.source.register{ id = "p" .. opts.x, label = "L", children = function() return {} end,
    read = function() return { lines = {}, eof = true } end }
end }`)
	h := newTestHost(t, plugins(dir, config.PluginRef{Name: "p", Opts: map[string]any{"x": 5, "nested": map[string]any{"y": "z"}}}))
	if s := statusOf(t, h, "p"); !s.OK {
		t.Fatalf("%+v", s)
	}
	if !h.Owns("p5") {
		t.Fatalf("roots: %v", rootIDs(h))
	}
}

func TestSetupErrorRollsBackRegistrations(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "p", `return { api_version = 1, setup = function()
  mcp.source.register{ id = "early", label = "L", children = function() return {} end,
    read = function() return { lines = {}, eof = true } end }
  error("boom")
end }`)
	h := newTestHost(t, plugins(dir, config.PluginRef{Name: "p"}))
	s := statusOf(t, h, "p")
	if s.OK || !strings.Contains(s.Error, "boom") {
		t.Fatalf("%+v", s)
	}
	if h.Owns("early") || len(h.Roots()) != 0 {
		t.Fatalf("registration not rolled back: %v", rootIDs(h))
	}
}

func TestRootIDRules(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "slash", rootPlugin("a/b"))
	writePlugin(t, dir, "upper", rootPlugin("Big"))
	writePlugin(t, dir, "clash", rootPlugin("app"))
	writePlugin(t, dir, "first", rootPlugin("dup"))
	writePlugin(t, dir, "second", rootPlugin("dup"))
	cfg := plugins(dir,
		config.PluginRef{Name: "slash"}, config.PluginRef{Name: "upper"}, config.PluginRef{Name: "clash"},
		config.PluginRef{Name: "first"}, config.PluginRef{Name: "second"})
	cfg.Sources = []config.Source{{Type: config.SourceLocal, ID: "app", Label: "App", Path: "/x"}}
	h := newTestHost(t, cfg)
	for _, n := range []string{"slash", "upper", "clash", "second"} {
		if s := statusOf(t, h, n); s.OK || !strings.Contains(s.Error, "id") {
			t.Fatalf("%s: %+v", n, s)
		}
	}
	if !statusOf(t, h, "first").OK {
		t.Fatal("first should load")
	}
	if got := strings.Join(rootIDs(h), ","); got != "dup" {
		t.Fatalf("roots: %s", got)
	}
}

func TestDisabledPluginNotLoaded(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "off", rootPlugin("off"))
	off := false
	h := newTestHost(t, plugins(dir, config.PluginRef{Name: "off", Enabled: &off}))
	if h.Owns("off") || len(h.Roots()) != 0 {
		t.Fatal("disabled plugin registered a root")
	}
	for _, s := range h.Status() {
		if s.Name == "off" {
			t.Fatalf("disabled plugin in status: %+v", s)
		}
	}
}

func TestReloadReplacesPlugins(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "a", rootPlugin("a"))
	writePlugin(t, dir, "b", rootPlugin("b"))
	h := newTestHost(t, plugins(dir, config.PluginRef{Name: "a"}))
	if !h.Owns("a") {
		t.Fatal("a not loaded")
	}
	h.Reload(plugins(dir, config.PluginRef{Name: "b"}))
	if h.Owns("a") || !h.Owns("b") || len(h.Status()) != 1 {
		t.Fatalf("after reload: roots %v status %+v", rootIDs(h), h.Status())
	}
	h.Close()
	if h.Owns("b") || len(h.Status()) != 0 {
		t.Fatal("Close did not unload")
	}
}

func TestLogAPIWritesToLogger(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "p", `mcp.log.info("hello") mcp.log.error("bad thing") return { api_version = 1 }`)
	log := &memLogger{}
	h := NewHost(Options{Logger: log})
	defer h.Close()
	h.Reload(plugins(dir, config.PluginRef{Name: "p"}))
	got := log.text()
	if !strings.Contains(got, "INFO plugin p: hello") || !strings.Contains(got, "bad thing") {
		t.Fatalf("log: %s", got)
	}
}

func TestSkillUnregisteredByDefault(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "p", rootPlugin("p"))
	h := newTestHost(t, plugins(dir, config.PluginRef{Name: "p"}))
	if _, ok := h.Skill("p"); ok {
		t.Fatal("unexpected skill")
	}
}

func TestRegisterOutsideSetupRejected(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "p", `return { api_version = 1, setup = function()
  mcp.source.register{ id = "p", label = "L",
    children = function()
      mcp.source.register{ id = "late", label = "L", children = function() return {} end,
        read = function() return { lines = {}, eof = true } end }
      return {}
    end,
    read = function() return { lines = {}, eof = true } end }
end }`)
	h := newTestHost(t, plugins(dir, config.PluginRef{Name: "p"}))
	_, err := h.Children(context.Background(), "p")
	if err == nil || !strings.HasPrefix(err.Error(), "plugin p: ") || !strings.Contains(err.Error(), "setup") {
		t.Fatalf("err: %v", err)
	}
	if h.Owns("late") {
		t.Fatal("late registration accepted")
	}
}

func TestQueuedCallGivesUpWhenContextDone(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "p", `return { api_version = 1, setup = function()
  mcp.source.register{ id = "p", label = "L", children = function() return {} end,
    read = function() while true do end end }
end }`)
	h := newTestHost(t, plugins(dir, config.PluginRef{Name: "p", TimeoutSeconds: 30}))
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := h.Read(context.Background(), "p", "", 10)
		done <- err
	}()
	<-started
	time.Sleep(100 * time.Millisecond) // let the spinner take the plugin lock

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := h.Children(ctx, "p")
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("queued call: err=%v after %v", err, time.Since(start))
	}
	if !strings.HasPrefix(err.Error(), "plugin p: children: ") {
		t.Fatalf("err: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("spinner finished early: %v", err)
	default:
	}
	h.Close() // aborts the spinner
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("spinner not aborted by Close")
	}
}

func TestRootIDLengthCapped(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "ok", rootPlugin(strings.Repeat("a", 128)))
	writePlugin(t, dir, "long", rootPlugin(strings.Repeat("a", 129)))
	h := newTestHost(t, plugins(dir, config.PluginRef{Name: "ok"}, config.PluginRef{Name: "long"}))
	if !statusOf(t, h, "ok").OK {
		t.Fatal("128-byte id should load")
	}
	if s := statusOf(t, h, "long"); s.OK || !strings.Contains(s.Error, "invalid id") {
		t.Fatalf("%+v", s)
	}
}

const skillPlugin = `return { api_version = 1, setup = function()
  mcp.source.register{ id = "p", label = "L", children = function() return {} end,
    read = function() return { lines = {}, eof = true } end }
  %s
end }`

func TestSkillRegister(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "good", strings.Replace(skillPlugin, "%s", `mcp.skill.register{ source = "p", text = "# hi" }`, 1))
	h := newTestHost(t, plugins(dir, config.PluginRef{Name: "good"}))
	if text, ok := h.Skill("p"); !ok || text != "# hi" {
		t.Fatalf("skill: %q %v", text, ok)
	}
}

func TestSkillRegisterErrors(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown": `mcp.skill.register{ source = "other", text = "x" }`,
		"badtext": `mcp.skill.register{ source = "p", text = 5 }`,
		"twice":   `mcp.skill.register{ source = "p", text = "a" } mcp.skill.register{ source = "p", text = "b" }`,
	} {
		writePlugin(t, dir, name, strings.Replace(skillPlugin, "%s", body, 1))
		h := newTestHost(t, plugins(dir, config.PluginRef{Name: name}))
		if s := statusOf(t, h, name); s.OK || !strings.Contains(s.Error, "mcp.skill.register") {
			t.Fatalf("%s: %+v", name, s)
		}
		if _, ok := h.Skill("p"); ok {
			t.Fatalf("%s: skill leaked", name)
		}
	}
}

func TestSkillRolledBackWithFailedPlugin(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "bad", strings.Replace(skillPlugin, "%s", `mcp.skill.register{ source = "p", text = "x" } error("boom")`, 1))
	h := newTestHost(t, plugins(dir, config.PluginRef{Name: "bad"}))
	if s := statusOf(t, h, "bad"); s.OK {
		t.Fatal("should fail")
	}
	if _, ok := h.Skill("p"); ok {
		t.Fatal("skill not rolled back")
	}
}

// Error positions name the plugin file relative to its plugin dir, never the
// absolute path (which would leak the host's layout to agents).
func TestChunkNameHidesPluginPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "boom"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "return { api_version = 1, setup = function()\n  error(\"bad setup\")\nend }\n"
	if err := os.WriteFile(filepath.Join(dir, "boom", "init.lua"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	writePlugin(t, dir, "flat", `return { api_version = 1, setup = function()
  mcp.source.register{ id = "flat", label = "F", children = function() return {} end,
    read = function() error("bad read") end }
end }`)
	h := newTestHost(t, plugins(dir, config.PluginRef{Name: "boom"}, config.PluginRef{Name: "flat"}))

	st := statusOf(t, h, "boom")
	if st.OK || !strings.Contains(st.Error, "boom/init.lua:2:") || strings.Contains(st.Error, dir) {
		t.Fatalf("setup error must use the relative chunk name: %q", st.Error)
	}
	_, err := h.Read(context.Background(), "flat", "", 10)
	if err == nil || !strings.Contains(err.Error(), "flat.lua:3:") || strings.Contains(err.Error(), dir) {
		t.Fatalf("call error must use the relative chunk name: %v", err)
	}
}
