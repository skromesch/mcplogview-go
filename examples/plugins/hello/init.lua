-- hello: a minimal example plugin. It exposes the files of one local
-- directory as a flat list of logs (source root `hello`) and adds one tool,
-- hello_count_lines. Configure it with:
--
--   { "name": "hello", "allow": ["fs"], "fs": { "roots": ["/var/log/myapp"] },
--     "opts": { "dir": "/var/log/myapp" } }

local NAME = "^[%w%._%-]+$"
local dir

local function fail(fmt, ...)
  error(string.format(fmt, ...), 0)
end

-- file checks a file name from an id or a tool argument: only plain names in
-- the directory, never paths.
local function file(name)
  if type(name) ~= "string" or not name:match(NAME) or name:sub(1, 1) == "." then
    fail("hello: bad file name '%s'", tostring(name))
  end
  return dir .. "/" .. name
end

local function children(path)
  if #path > 0 then return {} end -- a flat tree: only the root has children
  local out = {}
  for _, e in ipairs(mcp.fs.list(dir)) do
    if not e.dir and e.name:match(NAME) and e.name:sub(1, 1) ~= "." then
      out[#out + 1] = { name = e.name, leaf = true }
    end
  end
  return out
end

local function read(path, cursor, max_lines)
  if #path ~= 1 then fail("read hello/<file>") end
  local offset = cursor or 0 -- nil on the first read
  if type(offset) ~= "number" or offset < 0 or offset ~= math.floor(offset) then
    fail("hello: invalid cursor")
  end
  -- read only as many bytes as max_lines lines will plausibly need
  local f = mcp.fs.read(file(path[1]), offset, max_lines * 512)
  local data = f.data

  -- take at most max_lines complete lines; consumed counts the bytes they use
  local lines, pos, consumed = {}, 1, 0
  while #lines < max_lines do
    local nl = data:find("\n", pos, true)
    if not nl then break end
    lines[#lines + 1] = data:sub(pos, nl - 1)
    consumed, pos = nl, nl + 1
  end
  -- an unterminated last line at the end of the file counts as a line
  if #lines < max_lines and pos <= #data and offset + #data >= f.size then
    lines[#lines + 1] = data:sub(pos)
    consumed = #data
  end
  -- a single line longer than the buffer: hand it out in pieces
  if #lines == 0 and #data > 0 and offset + #data < f.size then
    lines[1], consumed = data, #data
  end

  local next_offset = offset + consumed
  return { lines = lines, cursor = next_offset, eof = next_offset >= f.size }
end

local function count_lines(args)
  local p = file(args.file)
  local n, offset = 0, 0
  while true do
    local f = mcp.fs.read(p, offset)
    local _, k = f.data:gsub("\n", "")
    n = n + k
    offset = offset + #f.data
    if offset >= f.size or #f.data == 0 then
      if #f.data > 0 and f.data:sub(-1) ~= "\n" then n = n + 1 end
      break
    end
  end
  return { file = args.file, lines = n }
end

return {
  api_version = 1,
  setup = function(opts)
    opts = opts or {}
    if type(opts.dir) ~= "string" or opts.dir:sub(1, 1) ~= "/" then
      fail("hello: opts.dir must be an absolute path")
    end
    dir = opts.dir:gsub("/+$", "")
    mcp.source.register {
      id = "hello", label = "Hello logs",
      hint = "list_logs parent=hello shows the files",
      children = children, read = read,
    }
    mcp.skill.register { source = "hello", text = "Files of " .. dir .. ". Read them with read_log_chunk." }
    mcp.tool.register {
      name = "hello_count_lines",
      description = "Count the lines of a file of the hello source",
      schema = { type = "object", properties = { file = { type = "string" } }, required = { "file" } },
      handler = count_lines,
    }
  end,
}
