-- Run with: mcplogview plugin test examples/plugins/hello

local function setup(files)
  local dir = test.fixture(files)
  local h = test.load { allow = { "fs" }, fs = { roots = { dir } }, opts = { dir = dir } }
  local ok, err = h:status()
  test.ok(ok, err)
  return h, dir
end

test.case("lists the files", function()
  local h = setup { ["app.log"] = "a\n", ["sub/x.log"] = "x\n", [".hidden"] = "h\n" }
  local es = h:list("hello")
  test.eq(#es, 1)
  test.eq(es[1].id, "hello/app.log")
  test.ok(es[1].leaf)
  test.contains(h:skill("hello"), "read_log_chunk")
end)

test.case("reads pages and follows appends", function()
  local h, dir = setup { ["app.log"] = "1\n2\n3" }
  local c = h:read("hello/app.log", nil, 2)
  test.eq(c.lines, { "1", "2" })
  test.eq(c.eof, false)
  c = h:read("hello/app.log", c.cursor, 10)
  test.eq(c.lines, { "3" })
  test.ok(c.eof)
  test.eq(c.state, 5)
  test.append(dir .. "/app.log", "\n4\n")
  test.eq(h:read("hello/app.log", 5, 10).lines, { "", "4" })
  test.errors(function() h:read("hello/app.log", -1, 10) end, "invalid cursor")
end)

test.case("counts lines", function()
  local h = setup { ["app.log"] = "1\n2\n3" }
  test.eq(h:tool("hello_count_lines", { file = "app.log" }), { file = "app.log", lines = 3 })
  test.errors(function() h:tool("hello_count_lines", { file = "../etc/passwd" }) end, "bad file name")
end)

test.case("needs opts.dir", function()
  local h = test.load { allow = { "fs" }, fs = { roots = { test.fixture {} } } }
  local ok, err = h:status()
  test.eq(ok, false)
  test.contains(err, "opts.dir")
end)
