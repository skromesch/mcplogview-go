package plugin

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// demoPlugin is a tiny plugin: an ssh-backed source and a tool.
const demoPlugin = `
local M = { api_version = 1 }
local cfg
function M.setup(opts) cfg = opts or {} end

mcp.source.register{
  id = "demo", label = "Demo",
  children = function(path)
    if #path == 0 then
      local out = {}
      for _, n in ipairs(mcp.str.lines(mcp.ssh("127.0.0.1"):exec("ls " .. mcp.str.quote(cfg.root)))) do
        out[#out+1] = { name = n, leaf = true }
      end
      return out
    end
    return {}
  end,
  read = function(path, cursor, max)
    local off = cursor and cursor.off or 0
    local r = mcp.ssh("127.0.0.1"):tail(cfg.root .. "/" .. path[1], off, max)
    return { lines = r.lines, cursor = { off = r.offset }, eof = r.eof }
  end,
}
mcp.skill.register{ source = "demo", text = "demo skill" }
mcp.tool.register{
  name = "demo_today",
  description = "today",
  schema = { type = "object", properties = { fail = { type = "boolean" } } },
  handler = function(args)
    if args.fail then error("asked to fail") end
    return { today = (mcp.ssh("127.0.0.1"):exec("date +%F"):gsub("%s+$", "")) }
  end,
}
return M
`

const demoTests = `
local function load(root)
  return test.load{ allow = {"ssh"}, ssh = { allowedCIDRs = {"127.0.0.0/8"}, user = "u", identityFile = "/k" }, opts = { root = root } }
end

test.case("lists and reads", function()
  local root = test.fixture{ ["a.log"] = "1\n2\n3\n", ["sub/b.log"] = "x\n" }
  local h = load(root)
  local ok, err = h:status()
  test.ok(ok, err)
  local es = h:list("demo")
  test.eq(es[1].id, "demo/a.log")
  test.eq(es[1].leaf, true)
  local roots = h:list()
  test.eq(roots[1].id, "demo")
  local c = h:read("demo/a.log", nil, 2)
  test.eq(c.lines, {"1", "2"})
  test.eq(c.eof, false)
  c = h:read("demo/a.log", c.cursor, 10)
  test.eq(c.lines, {"3"})
  test.ok(c.eof)
  test.eq(h:skill("demo"), "demo skill")
  test.contains(test.exec_log()[1], "ls ")
end)

test.case("tool with stub", function()
  test.stub_exec("date +%F", "2026-10-18\n")
  local h = load(test.fixture{})
  test.eq(h:tool("demo_today", {}), { today = "2026-10-18" })
  test.errors(function() h:tool("demo_today", { fail = true }) end, "asked to fail")
end)

test.case("failing eq", function()
  test.eq({1, {2}}, {1, {3}}, "deep")
end)

test.case("sandboxed", function()
  test.ok(io == nil and os == nil, "no io/os")
  test.fail("boom")
end)
`

func writeDemoPlugin(t *testing.T, tests map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(filepath.Join(dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "init.lua"), []byte(demoPlugin), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range tests {
		if err := os.WriteFile(filepath.Join(dir, "tests", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRunTestsReportsPassAndFail(t *testing.T) {
	dir := writeDemoPlugin(t, map[string]string{"demo_test.lua": demoTests, "notatest.lua": "error('not run')"})
	var out bytes.Buffer
	res, err := RunTests(context.Background(), dir, TestOptions{Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"ok   demo_test.lua: lists and reads\n",
		"ok   demo_test.lua: tool with stub\n",
		"FAIL demo_test.lua: failing eq: ",
		"FAIL demo_test.lua: sandboxed: ",
		"boom",
		"deep",
		"2 passed, 2 failed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if res.Passed != 2 || res.Failed != 2 {
		t.Errorf("res = %+v", res)
	}
}

func TestRunTestsFilterAndLoadErrors(t *testing.T) {
	dir := writeDemoPlugin(t, map[string]string{"demo_test.lua": demoTests, "broken_test.lua": "this is not lua"})
	var out bytes.Buffer
	res, err := RunTests(context.Background(), dir, TestOptions{Out: &out, Run: regexp.MustCompile("stub")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed != 1 || res.Failed != 1 || !strings.Contains(out.String(), "FAIL broken_test.lua") {
		t.Errorf("res = %+v\n%s", res, out.String())
	}
}

func TestRunTestsFixtureCleanedAndBadLoad(t *testing.T) {
	dir := writeDemoPlugin(t, map[string]string{"x_test.lua": `
local kept
test.case("a", function()
  kept = test.fixture{ ["d/e.txt"] = "hi" }
  test.ok(kept:sub(1, 1) == "/" or kept:match("^%a:"), kept)
  local h = test.load{ name = "nope" }
  local ok, err = h:status()
  test.eq(ok, false)
  test.contains(err, "not found")
  test.errors(function() test.fixture{ ["../x"] = "no" } end, "invalid")
end)
test.case("b", function() _G.kept = kept end)
`})
	var out bytes.Buffer
	res, err := RunTests(context.Background(), dir, TestOptions{Out: &out})
	if err != nil || res.Failed != 0 || res.Passed != 2 {
		t.Fatalf("res=%+v err=%v\n%s", res, err, out.String())
	}
}

func TestRunTestsNoTests(t *testing.T) {
	dir := writeDemoPlugin(t, nil)
	_, err := RunTests(context.Background(), dir, TestOptions{Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "no tests") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunTestsRemovesFixtures(t *testing.T) {
	dir := writeDemoPlugin(t, map[string]string{"f_test.lua": `test.case("leak", function() test.fail("DIR=" .. test.fixture{ ["a/b"] = "x" }) end)`})
	var out bytes.Buffer
	if _, err := RunTests(context.Background(), dir, TestOptions{Out: &out}); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`DIR=(\S+)`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("no dir in %s", out.String())
	}
	if _, err := os.Stat(m[1]); !os.IsNotExist(err) {
		t.Fatalf("fixture %s not removed: %v", m[1], err)
	}
}

func TestRunTestsFixtureMutationAndSSHDown(t *testing.T) {
	dir := writeDemoPlugin(t, map[string]string{"m_test.lua": `
local function load(root)
  return test.load{ allow = {"ssh"}, ssh = { allowedCIDRs = {"127.0.0.0/8"}, user = "u", identityFile = "/k" }, opts = { root = root } }
end
test.case("mutate", function()
  local root = test.fixture{ ["a.log"] = "1\n" }
  local h = load(root)
  local c = h:read("demo/a.log", nil, 10)
  test.eq(c.state, { off = 2 })
  test.append(root .. "/a.log", "2\n")
  test.eq(h:read("demo/a.log", { off = 2 }, 10).lines, {"2"})
  test.eq(h:read("demo/a.log", c.cursor, 10).lines, {"2"})
  test.write(root .. "/b.log", "x\n")
  test.eq(#h:list("demo"), 2)
  test.remove(root .. "/b.log")
  test.eq(#h:list("demo"), 1)
  test.errors(function() test.write(root .. "/../escape", "no") end, "not inside a fixture")
  test.errors(function() test.write("/etc/x", "no") end, "not inside a fixture")
end)
test.case("down", function()
  local h = load(test.fixture{})
  test.ssh_down("127.0.0.1")
  test.errors(function() h:tool("demo_today", {}) end, "127.0.0.1")
end)
`})
	var out bytes.Buffer
	res, err := RunTests(context.Background(), dir, TestOptions{Out: &out})
	if err != nil || res.Failed != 0 || res.Passed != 2 {
		t.Fatalf("res=%+v err=%v\n%s", res, err, out.String())
	}
}
