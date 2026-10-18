package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/logs"
	"github.com/skromesch/mcplogview-go/internal/serverlog"
)

// pagerPlugin registers root "fake" with leaves x and y (four lines each,
// paged by a {n=...} cursor) and a directory "dir".
const pagerPlugin = `return { api_version = 1, setup = function(opts)
  mcp.source.register{ id = "fake", label = "Fake", description = "D", hint = "H",
    children = function(path)
      if #path == 0 then
        return { { name = "x", label = "X", leaf = true }, { name = "dir" } }
      end
      return {}
    end,
    read = function(path, cursor, max)
      local n = cursor and cursor.n or 0
      local lines = {}
      for i = n + 1, math.min(n + max, 4) do lines[#lines + 1] = table.concat(path, "/") .. ":" .. i end
      return { lines = lines, cursor = { n = n + #lines }, eof = (n + #lines) >= 4 }
    end }
  %s
end }`

func pluginCfg(t *testing.T, skill string) config.Config {
	t.Helper()
	dir := t.TempDir()
	src := strings.Replace(pagerPlugin, "%s", skill, 1)
	if err := os.WriteFile(filepath.Join(dir, "fake.lua"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Logging.Enabled = false
	cfg.SkillsDir = t.TempDir()
	cfg.PluginDirs = []string{dir}
	cfg.Plugins = []config.PluginRef{{Name: "fake"}}
	cfg.Sources = []config.Source{{Type: "local", ID: "app", Label: "App", Path: filepath.Join(t.TempDir(), "x.log")}}
	return cfg
}

func connectCfg(t *testing.T, cfg config.Config) (*mcp.ClientSession, *Runtime) {
	t.Helper()
	rt := NewRuntime(cfg, serverlog.Noop())
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

type listed struct {
	Logs []struct {
		ID, Label, Description, Hint string
		Leaf, Expandable             bool
	} `json:"logs"`
}

func list(t *testing.T, cs *mcp.ClientSession, args map[string]any) listed {
	t.Helper()
	text, isErr := call(t, cs, "list_logs", args)
	if isErr {
		t.Fatal(text)
	}
	var l listed
	if err := json.Unmarshal([]byte(text), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestListLogsIncludesPluginRootsExpandable(t *testing.T) {
	cs, _ := connectCfg(t, pluginCfg(t, ""))
	text, _ := call(t, cs, "list_logs", nil)
	l := list(t, cs, nil)
	if len(l.Logs) != 2 || l.Logs[0].ID != "app" || l.Logs[0].Expandable {
		t.Fatalf("configured source first, not expandable: %s", text)
	}
	r := l.Logs[1]
	if r.ID != "fake" || r.Label != "Fake" || r.Description != "D" || r.Hint != "H" || !r.Expandable || r.Leaf {
		t.Fatalf("root: %s", text)
	}
}

func TestListLogsParentReturnsChildrenWithJoinedIDs(t *testing.T) {
	cs, _ := connectCfg(t, pluginCfg(t, ""))
	l := list(t, cs, map[string]any{"parent": "fake"})
	if len(l.Logs) != 2 {
		t.Fatalf("%+v", l)
	}
	if x := l.Logs[0]; x.ID != "fake/x" || x.Label != "X" || !x.Leaf || x.Expandable {
		t.Fatalf("x: %+v", x)
	}
	if d := l.Logs[1]; d.ID != "fake/dir" || d.Leaf || !d.Expandable {
		t.Fatalf("dir: %+v", d)
	}
}

func TestListLogsUnknownParent(t *testing.T) {
	cs, _ := connectCfg(t, pluginCfg(t, ""))
	for _, parent := range []string{"nope", "app", "nope/x"} {
		text, isErr := call(t, cs, "list_logs", map[string]any{"parent": parent})
		if !isErr || text != "Tool error (list_logs): unknown source: "+parent {
			t.Fatalf("%s: %q %v", parent, text, isErr)
		}
	}
}

func TestReadLogChunkPluginPagesWithCursor(t *testing.T) {
	cs, _ := connectCfg(t, pluginCfg(t, ""))
	read := func(args map[string]any) logs.Chunk {
		text, isErr := call(t, cs, "read_log_chunk", args)
		var c logs.Chunk
		if err := json.Unmarshal([]byte(text), &c); isErr || err != nil {
			t.Fatalf("%s %v", text, err)
		}
		return c
	}
	c := read(map[string]any{"sourceId": "fake/x", "maxLines": 3})
	if strings.Join(c.Lines, ",") != "x:1,x:2,x:3" || c.EOF {
		t.Fatalf("%+v", c)
	}
	c = read(map[string]any{"sourceId": "fake/x", "maxLines": 3, "cursor": c.Cursor})
	if strings.Join(c.Lines, ",") != "x:4" || !c.EOF {
		t.Fatalf("%+v", c)
	}
	for _, bad := range []int{0, 501} {
		if text, isErr := call(t, cs, "read_log_chunk", map[string]any{"sourceId": "fake/x", "maxLines": bad}); !isErr || !strings.Contains(text, "maxLines") {
			t.Fatalf("maxLines %d accepted: %s", bad, text)
		}
	}
	if _, isErr := call(t, cs, "read_log_chunk", map[string]any{"sourceId": "fake/x", "cursor": "!!"}); !isErr {
		t.Fatal("bad cursor accepted")
	}
}

func TestFollowLogPluginTimesOutEmpty(t *testing.T) {
	cs, _ := connectCfg(t, pluginCfg(t, ""))
	text, _ := call(t, cs, "read_log_chunk", map[string]any{"sourceId": "fake/x", "maxLines": 500})
	var c logs.Chunk
	_ = json.Unmarshal([]byte(text), &c)

	start := time.Now()
	text, isErr := call(t, cs, "follow_log", map[string]any{"sourceId": "fake/x", "cursor": c.Cursor, "timeoutMs": 150, "pollIntervalMs": 20})
	var res logs.Chunk
	_ = json.Unmarshal([]byte(text), &res)
	if isErr || len(res.Lines) != 0 || !res.EOF || res.Cursor == "" || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("follow: %s after %v", text, time.Since(start))
	}
	// fresh data is returned immediately
	text, _ = call(t, cs, "follow_log", map[string]any{"sourceId": "fake/y", "timeoutMs": 150, "pollIntervalMs": 20})
	_ = json.Unmarshal([]byte(text), &res)
	if len(res.Lines) != 4 {
		t.Fatalf("follow fresh: %s", text)
	}
	for _, args := range []map[string]any{{"maxLines": 501}, {"timeoutMs": 0}, {"pollIntervalMs": 5}} {
		args["sourceId"] = "fake/x"
		if _, isErr := call(t, cs, "follow_log", args); !isErr {
			t.Fatalf("%v accepted", args)
		}
	}
}

func TestGetLogSkillPluginUsesRegisteredText(t *testing.T) {
	cs, _ := connectCfg(t, pluginCfg(t, `mcp.skill.register{ source = "fake", text = "# Fake skill\nuse it" }`))
	for _, id := range []string{"fake", "fake/x"} {
		text, isErr := call(t, cs, "get_log_skill", map[string]any{"sourceId": id})
		if isErr || !strings.Contains(text, `"origin":"plugin"`) || !strings.Contains(text, "# Fake skill") || !strings.Contains(text, `"sourceId":"`+id+`"`) {
			t.Fatalf("%s: %s", id, text)
		}
	}
}

func TestGetLogSkillPluginFallbackWhenNone(t *testing.T) {
	cs, _ := connectCfg(t, pluginCfg(t, ""))
	text, isErr := call(t, cs, "get_log_skill", map[string]any{"sourceId": "fake/x"})
	if isErr || !strings.Contains(text, `"origin":"fallback"`) || !strings.Contains(text, "fake/x") {
		t.Fatalf("%s", text)
	}
	if text, isErr := call(t, cs, "get_log_skill", map[string]any{"sourceId": "nope/x"}); !isErr || !strings.Contains(text, "unknown source") {
		t.Fatalf("%s", text)
	}
}

func TestApplyReloadsPluginsOnlyWhenPluginConfigChanged(t *testing.T) {
	cfg := pluginCfg(t, "")
	// counts loads through the plugin's own log call
	dir := cfg.PluginDirs[0]
	src := `return { api_version = 1, setup = function(opts)
  mcp.log.info("setup v=" .. tostring(opts.v))
  mcp.source.register{ id = "cnt", label = "C", children = function() return {} end,
    read = function() return { lines = {}, eof = true } end }
end }`
	if err := os.WriteFile(filepath.Join(dir, "cnt.lua"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Plugins = []config.PluginRef{{Name: "cnt", Opts: map[string]any{"v": 1}}}
	log := &recLogger{Logger: serverlog.Noop()}
	rt := NewRuntime(cfg, log)
	t.Cleanup(rt.Close)
	if n := log.count("setup v=1"); n != 1 {
		t.Fatalf("initial loads: %d", n)
	}
	host := rt.Plugins()

	// toggling a source / label / hint alone keeps plugin state
	off := cfg
	off.Sources = []config.Source{cfg.Sources[0]}
	off.Sources[0].Disabled = true
	off.Sources[0].Label = "Other"
	off.Sources[0].Hint = "hint"
	rt.Apply(off)
	if n := log.count("setup"); n != 1 {
		t.Fatalf("source toggle reloaded plugins: %d", n)
	}
	if rt.Plugins() != host {
		t.Fatal("host replaced")
	}

	// opts change reloads
	changed := off
	changed.Plugins = []config.PluginRef{{Name: "cnt", Opts: map[string]any{"v": 2}}}
	rt.Apply(changed)
	if n := log.count("setup v=2"); n != 1 || log.count("setup") != 2 {
		t.Fatalf("opts change: %v", log.all())
	}

	// pluginDirs change reloads
	dirs := changed
	dirs.PluginDirs = []string{dir, t.TempDir()}
	rt.Apply(dirs)
	if n := log.count("setup"); n != 3 {
		t.Fatalf("pluginDirs change: %d", n)
	}

	// adding a source id reloads (collision rules depend on the id set)
	more := dirs
	more.Sources = append(append([]config.Source{}, dirs.Sources...), config.Source{Type: "local", ID: "extra", Label: "E", Path: "/x"})
	rt.Apply(more)
	if n := log.count("setup"); n != 4 {
		t.Fatalf("source id change: %d", n)
	}
	// same again: nothing
	rt.Apply(more)
	if n := log.count("setup"); n != 4 {
		t.Fatalf("no-op apply reloaded: %d", n)
	}
}

type recLogger struct {
	serverlog.Logger
	lines []string
}

func (l *recLogger) Info(m string)           { l.lines = append(l.lines, m) }
func (l *recLogger) Error(m string, _ error) { l.lines = append(l.lines, m) }
func (l *recLogger) all() []string           { return l.lines }
func (l *recLogger) count(sub string) int {
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, "plugin cnt: "+sub) {
			n++
		}
	}
	return n
}

func TestExistingToolOutputsUnchanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.log")
	_ = os.WriteFile(p, []byte("a\n"), 0o644)
	cs := connect(t, p)
	text, isErr := call(t, cs, "list_logs", nil)
	if isErr || text != `{"logs":[{"id":"app","label":"App","hint":"h"}]}` {
		t.Fatalf("list_logs changed: %s", text)
	}
	text, _ = call(t, cs, "get_log_skill", map[string]any{"sourceId": "app"})
	var m map[string]any
	_ = json.Unmarshal([]byte(text), &m)
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "hint,origin,renderedMarkdown,sourceId,structured,warnings" {
		t.Fatalf("skill keys: %v", keys)
	}
	for _, tool := range []string{"read_log_chunk", "follow_log"} {
		args := map[string]any{"sourceId": "app"}
		if tool == "follow_log" {
			args["timeoutMs"] = 50
		}
		text, _ = call(t, cs, tool, args)
		var c logs.Chunk
		if err := json.Unmarshal([]byte(text), &c); err != nil {
			t.Fatalf("%s: %s", tool, text)
		}
		want, _ := json.Marshal(c)
		if text != string(want) || !strings.HasPrefix(text, `{"lines":["a"],"cursor":"`) {
			t.Fatalf("%s output changed: %s", tool, text)
		}
	}
}

func TestForeignCursorRejectedOnPluginSource(t *testing.T) {
	cfg := pluginCfg(t, "")
	if err := os.WriteFile(cfg.Sources[0].Path, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cs, _ := connectCfg(t, cfg)
	cursorOf := func(id string) string {
		text, isErr := call(t, cs, "read_log_chunk", map[string]any{"sourceId": id, "maxLines": 1})
		var c logs.Chunk
		if err := json.Unmarshal([]byte(text), &c); isErr || err != nil || c.Cursor == "" {
			t.Fatalf("%s: %s", id, text)
		}
		return c.Cursor
	}
	local, plug := cursorOf("app"), cursorOf("fake/x")
	for _, tc := range []struct{ name, id, cursor, want string }{
		{"local cursor on plugin", "fake/x", local, `cursor does not belong to source "fake/x"`},
		{"other plugin source cursor", "fake/y", plug, `cursor belongs to source "fake/x", not "fake/y"`},
	} {
		for _, tool := range []string{"read_log_chunk", "follow_log"} {
			args := map[string]any{"sourceId": tc.id, "cursor": tc.cursor}
			if tool == "follow_log" {
				args["timeoutMs"] = 50
			}
			text, isErr := call(t, cs, tool, args)
			if !isErr || !strings.Contains(text, tc.want) {
				t.Fatalf("%s/%s: %q %v", tc.name, tool, text, isErr)
			}
		}
	}
}

// Plugin-origin marking: list_logs entries and read/follow results of plugin
// sources carry "plugin":"<name>"; configured sources never do.
func TestPluginOriginMarked(t *testing.T) {
	cfg := pluginCfg(t, "")
	if err := os.WriteFile(cfg.Sources[0].Path, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cs, _ := connectCfg(t, cfg)
	decode := func(text string) []map[string]any {
		var l struct {
			Logs []map[string]any `json:"logs"`
		}
		if err := json.Unmarshal([]byte(text), &l); err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		return l.Logs
	}
	text, _ := call(t, cs, "list_logs", nil)
	for _, e := range decode(text) {
		_, has := e["plugin"]
		if e["id"] == "app" && has {
			t.Fatalf("configured source marked: %s", text)
		}
		if e["id"] == "fake" && e["plugin"] != "fake" {
			t.Fatalf("plugin root not marked: %s", text)
		}
	}
	text, _ = call(t, cs, "list_logs", map[string]any{"parent": "fake"})
	kids := decode(text)
	if len(kids) == 0 {
		t.Fatalf("no children: %s", text)
	}
	for _, e := range kids {
		if e["plugin"] != "fake" {
			t.Fatalf("plugin child not marked: %s", text)
		}
	}
	for _, tool := range []string{"read_log_chunk", "follow_log"} {
		args := func(id string) map[string]any {
			a := map[string]any{"sourceId": id}
			if tool == "follow_log" {
				a["timeoutMs"] = 50
			}
			return a
		}
		text, isErr := call(t, cs, tool, args("fake/x"))
		if isErr || !strings.Contains(text, `"plugin":"fake"`) {
			t.Fatalf("%s plugin result not marked: %s", tool, text)
		}
		var c logs.Chunk
		if err := json.Unmarshal([]byte(text), &c); err != nil || len(c.Lines) == 0 || c.Cursor == "" {
			t.Fatalf("%s chunk fields must stay: %s %v", tool, text, err)
		}
		text, isErr = call(t, cs, tool, args("app"))
		if isErr || strings.Contains(text, "plugin") {
			t.Fatalf("%s configured result marked: %s", tool, text)
		}
	}
}
