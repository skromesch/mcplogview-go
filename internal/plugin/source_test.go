package plugin

import (
	"context"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// pagerPlugin serves four lines per leaf, paging with a {n=...} cursor.
const pagerPlugin = `return { api_version = 1, setup = function()
  mcp.source.register{ id = "fake", label = "Fake", description = "D", hint = "H",
    children = function(path)
      if #path == 0 then
        return { { name = "x", label = "X", leaf = true }, { name = "y", description = "dy", leaf = true }, { name = "dir" } }
      end
      return {}
    end,
    read = function(path, cursor, max)
      local n = cursor and cursor.n or 0
      local lines = {}
      for i = n + 1, math.min(n + max, 4) do lines[#lines + 1] = table.concat(path, "/") .. ":" .. i end
      return { lines = lines, cursor = { n = n + #lines }, eof = (n + #lines) >= 4 }
    end }
end }`

func pagerHost(t *testing.T, ref config.PluginRef, src string) *Host {
	t.Helper()
	dir := t.TempDir()
	writePlugin(t, dir, ref.Name, src)
	h := newTestHost(t, plugins(dir, ref))
	if s := statusOf(t, h, ref.Name); !s.OK {
		t.Fatalf("%+v", s)
	}
	return h
}

func TestChildrenAndReadWrapCursor(t *testing.T) {
	h := pagerHost(t, config.PluginRef{Name: "fake"}, pagerPlugin)
	ctx := context.Background()
	r := h.Roots()[0]
	if r != (Root{ID: "fake", Label: "Fake", Description: "D", Hint: "H", Plugin: "fake"}) {
		t.Fatalf("root: %+v", r)
	}
	kids, err := h.Children(ctx, "fake")
	if err != nil {
		t.Fatal(err)
	}
	want := []Child{{Name: "x", Label: "X", Leaf: true}, {Name: "y", Label: "y", Description: "dy", Leaf: true}, {Name: "dir", Label: "dir"}}
	if !reflect.DeepEqual(kids, want) {
		t.Fatalf("children: %+v", kids)
	}

	c1, err := h.Read(ctx, "fake/x", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c1.Lines, ",") != "x:1,x:2,x:3" || c1.EOF || c1.Cursor == "" {
		t.Fatalf("chunk1: %+v", c1)
	}
	raw, err := base64.StdEncoding.DecodeString(c1.Cursor)
	if err != nil || string(raw) != `{"s":"fake/x","c":{"n":3}}` {
		t.Fatalf("cursor: %s %v", raw, err)
	}
	c2, err := h.Read(ctx, "fake/x", c1.Cursor, 3)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c2.Lines, ",") != "x:4" || !c2.EOF {
		t.Fatalf("chunk2: %+v", c2)
	}
}

func TestCursorFromOtherSourceRejected(t *testing.T) {
	h := pagerHost(t, config.PluginRef{Name: "fake"}, pagerPlugin)
	ctx := context.Background()
	c, err := h.Read(ctx, "fake/x", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Read(ctx, "fake/y", c.Cursor, 1)
	if err == nil || !strings.Contains(err.Error(), "fake/x") || !strings.Contains(err.Error(), "fake/y") {
		t.Fatalf("err: %v", err)
	}
	// valid base64 JSON without a source tag (e.g. a local source's cursor)
	untagged := base64.StdEncoding.EncodeToString([]byte(`{"inode":1,"offset":5}`))
	if _, err := h.Read(ctx, "fake/x", untagged, 1); err == nil || !strings.Contains(err.Error(), `does not belong to source "fake/x"`) {
		t.Fatalf("untagged cursor: %v", err)
	}
	if _, err := h.Read(ctx, "fake/x", "!!notbase64", 1); err == nil || !strings.Contains(err.Error(), "cursor") {
		t.Fatalf("garbage cursor: %v", err)
	}
}

func TestSourceIDValidation(t *testing.T) {
	h := pagerHost(t, config.PluginRef{Name: "fake"}, pagerPlugin)
	ctx := context.Background()
	for _, id := range []string{
		"fake//x", "fake/x/", "fake/a/b/c/d/e/f/g/h", "fake/" + strings.Repeat("a", 129), "fake/a\nb", "nope/x",
	} {
		if _, err := h.Read(ctx, id, "", 1); err == nil {
			t.Errorf("Read(%q) accepted", id)
		}
		if _, err := h.Children(ctx, id); err == nil {
			t.Errorf("Children(%q) accepted", id)
		}
	}
	if _, err := h.Read(ctx, "fake/a/b/c/d/e/f/g", "", 1); err != nil {
		t.Errorf("8 segments rejected: %v", err)
	}
	for _, n := range []int{0, 501} {
		if _, err := h.Read(ctx, "fake/x", "", n); err == nil {
			t.Errorf("maxLines %d accepted", n)
		}
	}
}

func TestBadChildrenAndReadResults(t *testing.T) {
	h := pagerHost(t, config.PluginRef{Name: "p"}, `return { api_version = 1, setup = function()
  mcp.source.register{ id = "p", label = "L",
    children = function(path)
      if path[1] == "slash" then return { { name = "a/b" } } end
      if path[1] == "empty" then return { { name = "" } } end
      return "nope"
    end,
    read = function(path, cursor, max)
      if path[1] == "many" then return { lines = { "a", "b", "c" } } end
      if path[1] == "num" then return { lines = { {} } } end
      if path[1] == "fn" then return { lines = {}, cursor = function() end } end
      error("read failed")
    end }
end }`)
	ctx := context.Background()
	for _, id := range []string{"p", "p/slash", "p/empty"} {
		if _, err := h.Children(ctx, id); err == nil || !strings.HasPrefix(err.Error(), "plugin p: ") {
			t.Errorf("Children(%s): %v", id, err)
		}
	}
	for _, id := range []string{"p/many", "p/num", "p/fn", "p/other"} {
		if _, err := h.Read(ctx, id, "", 2); err == nil || !strings.HasPrefix(err.Error(), "plugin p: ") {
			t.Errorf("Read(%s): %v", id, err)
		}
	}
	if _, err := h.Read(ctx, "p/other", "", 2); err == nil || !strings.Contains(err.Error(), "read failed") {
		t.Errorf("lua error not surfaced: %v", err)
	}
}

func TestInfiniteLoopInReadReturnsError(t *testing.T) {
	h := pagerHost(t, config.PluginRef{Name: "p", TimeoutSeconds: 1}, `return { api_version = 1, setup = function()
  mcp.source.register{ id = "p", label = "L",
    children = function() return { { name = "ok", leaf = true } } end,
    read = function() while true do end end }
end }`)
	start := time.Now()
	_, err := h.Read(context.Background(), "p/x", "", 10)
	if err == nil || !strings.HasPrefix(err.Error(), "plugin p: ") {
		t.Fatalf("err: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
	kids, err := h.Children(context.Background(), "p")
	if err != nil || len(kids) != 1 {
		t.Fatalf("host unusable after timeout: %v %v", kids, err)
	}
}

func TestCloseAbortsRunningCall(t *testing.T) {
	h := pagerHost(t, config.PluginRef{Name: "p", TimeoutSeconds: 60}, `return { api_version = 1, setup = function()
  mcp.source.register{ id = "p", label = "L", children = function() return {} end,
    read = function() while true do end end }
end }`)
	done := make(chan error, 1)
	go func() {
		_, err := h.Read(context.Background(), "p/x", "", 10)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	h.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not abort the running call")
	}
	if _, err := h.Read(context.Background(), "p/x", "", 10); err == nil {
		t.Fatal("read after Close succeeded")
	}
}

func TestConcurrentReadsDuringReload(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "fake", pagerPlugin)
	cfg := plugins(dir, config.PluginRef{Name: "fake"})
	h := newTestHost(t, cfg)
	done := make(chan struct{})
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			for {
				select {
				case <-done:
					errs <- nil
					return
				default:
				}
				// Errors are expected while a reload swaps plugins; only races matter.
				_, _ = h.Read(context.Background(), "fake/x", "", 2)
				_, _ = h.Children(context.Background(), "fake")
				_ = h.Roots()
			}
		}()
	}
	for i := 0; i < 20; i++ {
		h.Reload(cfg)
	}
	close(done)
	for i := 0; i < 4; i++ {
		<-errs
	}
	if _, err := h.Read(context.Background(), "fake/x", "", 2); err != nil {
		t.Fatal(err)
	}
}

func TestHelperAPIs(t *testing.T) {
	h := pagerHost(t, config.PluginRef{Name: "p"}, `return { api_version = 1, setup = function()
  mcp.source.register{ id = "p", label = "L", children = function() return {} end,
    read = function()
      local d = mcp.json.decode('{"a":[1,2],"b":"x"}')
      local ls = mcp.str.lines("one\ntwo\n")
      local ok, err = pcall(mcp.time.shift_days, "2026-13-01", 1)
      return { lines = {
        mcp.str.quote("it's"),
        mcp.json.encode({ k = "v" }),
        tostring(d.a[2]) .. d.b,
        tostring(#ls) .. ls[1] .. ls[2],
        #mcp.str.lines("") .. "",
        mcp.time.shift_days("2026-03-01", -1),
        mcp.time.shift_days("2026-12-31", 1),
        tostring(ok),
      }, eof = true }
    end }
end }`)
	c, err := h.Read(context.Background(), "p/x", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`'it'\''s'`, `{"k":"v"}`, "2x", "2onetwo", "0", "2026-02-28", "2027-01-01", "false"}
	if !reflect.DeepEqual(c.Lines, want) {
		t.Fatalf("got %q", c.Lines)
	}
}
