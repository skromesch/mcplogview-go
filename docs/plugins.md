# Lua plugins

A *plugin* is a small Lua script, installed by the server's administrator, that teaches mcplogview something
the built-in [source types](sources.md) cannot do: browse a tree of logs it discovers itself, search several
files at once, add an extra tool for agents, or watch the tool calls that pass through the server.

A plugin can:

- register a **source tree** that agents browse with `list_logs` (`parent=...`) and read with
  `read_log_chunk` / `follow_log`,
- register extra **tools**,
- register the usage **skill** of its source,
- react to **events** (config reload, before/after any tool call, shutdown) and deny tool calls on a
  best-effort basis (`before_tool` is advisory, not an access-control boundary; see [`mcp.on`](#mcpon)),
- use two optional **capabilities** the admin grants per plugin: `ssh` (run commands on hosts in an allowed
  network) and `fs` (read local files under allowed directories).

Plugins are off by default. With no `plugins` in the config nothing changes for existing setups.

## Where plugins live

Plugin files are looked up in the directories listed in `pluginDirs` (default: a `plugins` directory next to
the config file). For a plugin named `foo`, the first directory that contains

- `foo.lua`, or
- `foo/init.lua`

wins (in that order within a directory). A plugin that is configured but cannot be found is reported as a load
failure; the other plugins are not affected.

A plugin file runs once when it is loaded and must return a table:

```lua
return {
  api_version = 1,                  -- required; the only supported version
  setup = function(opts)            -- optional; opts is the "opts" object from the config
    -- register sources, tools, skills and hooks here
  end,
}
```

Registrations (`mcp.source.register`, `mcp.tool.register`, `mcp.skill.register`, `mcp.on`) are only allowed
while the plugin loads, that is at the top level of the file or inside `setup`. If loading fails at any point,
everything the plugin registered is discarded.

## Configuration

Two top-level keys in the [config file](configuration.md):

```json
{
  "pluginDirs": ["plugins"],
  "plugins": [
    {
      "name": "stations",
      "enabled": true,
      "allow": ["ssh"],
      "ssh": {
        "allowedCIDRs": ["192.0.2.0/24"],
        "user": "logreader",
        "identityFile": "~/.ssh/stations_key"
      },
      "timeoutSeconds": 120,
      "opts": { "subnet": "192.0.2" }
    }
  ]
}
```

### `pluginDirs`

| Default | Meaning |
|---------|---------|
| `["plugins"]` | Directories searched for plugin files, in order. Relative paths are resolved against the directory of the config file; `~/` is expanded |

### `plugins[]`

| Key | Default | Meaning |
|-----|---------|---------|
| `name` | required | Plugin name: lowercase letters, digits, `_`, `-`, starting with a letter or digit. Must be unique. It selects the file (`<name>.lua` / `<name>/init.lua`) |
| `enabled` | `true` | `false` keeps the entry in the config but does not load the plugin |
| `allow` | `[]` | Capabilities granted to the plugin: `"ssh"` and/or `"fs"`. Anything else is rejected. Without a capability, the matching `mcp.ssh` / `mcp.fs` does not exist for the plugin |
| `ssh` | none | Required when `allow` contains `"ssh"`, see below |
| `fs` | none | Required when `allow` contains `"fs"`, see below |
| `timeoutSeconds` | `0` = 120 | Time limit for one call into the plugin (loading, a tool call, a `children` / `read` call, a hook). 0 to 600. Loading and `setup` run synchronously at startup, before the server answers `initialize` |
| `opts` | none | Any JSON object; handed to `setup(opts)` as a Lua table |
| `git` | none | Repository URL `mcplogview plugin sync` installs the plugin from (see [Installing plugins from git](#installing-plugins-from-git)). No whitespace, must not start with `-` |
| `ref` | remote HEAD | Branch, tag or commit to check out. Requires `git`; same character rules |

`ssh` block (the Go side connects with these; the plugin cannot change user or key):

| Key | Default | Meaning |
|-----|---------|---------|
| `allowedCIDRs` | required, non-empty | Networks the plugin may connect to, e.g. `["192.0.2.0/24"]`. Each must be a valid CIDR |
| `user` | required | SSH user |
| `identityFile` | required | SSH private key (`~/` allowed, relative paths resolve against the config directory) |
| `port` | `22` | SSH port (0 to 65535; 0 means 22) |
| `strictHostKeyChecking` | `false` | Verify the host key, as for [ssh sources](sources.md#ssh-files) |

`fs` block:

| Key | Default | Meaning |
|-----|---------|---------|
| `roots` | required, non-empty | Local directories the plugin may read. No empty entries; relative paths resolve against the config directory |

A config with an invalid `plugins` entry is rejected as a whole, with a message naming the entry, for example
`plugins[0] (stations): ssh.allowedCIDRs must not be empty` or `duplicate plugin name: stations`.

A running server applies changes to `pluginDirs` and `plugins` (and to the set of source ids, which plugin
roots must not collide with) by reloading all plugins; other config changes leave the plugins alone. The plugin
*files* are not watched, see [Known limitations](#known-limitations).

## Installing plugins from git

A `plugins[]` entry can name a git repository instead of relying on files you copied by hand:

```text
{ "name": "hello", "git": "https://example.com/you/mcplogview-hello.git", "ref": "v1.0.0",
  "allow": ["fs"], "fs": { "roots": ["/var/log/myapp"] } }
```

- `git`: the repository URL. The repository root is the plugin directory, so it must contain `init.lua`.
- `ref`: a branch, tag or commit to check out. Without it the remote's default branch is used. `ref`
  requires `git`.
- Both are rejected when they start with `-`, or contain whitespace or a NUL character.

Installing uses the system `git` (it must be in `PATH`). Prompts are disabled, every git call times out after
120 s, and only the transports `https`, `ssh`, `git` and `file` are allowed (`ext::` and the like are
refused, also through submodules and redirects).

```text
mcplogview plugin sync [--update] [NAME...]   clone or fetch, then check out the locked commit
mcplogview plugin list                         install state and whether each plugin loads
mcplogview plugin remove NAME                  delete a plugin installed by sync
```

**`sync`** handles all plugins with a `git` URL, or only the named ones (naming a plugin without `git`, or
one that is not in the config, is an error). Each is cloned into `<first pluginDirs entry>/<name>/` (the
directory is created), so `<name>/init.lua` is found as usual, and the checked-out commit is recorded in the
lock file. Later syncs fetch but keep the locked commit; `--update` moves to the latest commit of `ref`
(branch, tag or commit; the remote's default branch when empty) and re-locks. The lock entry is also ignored
when `git` or `ref` changed in the config. One line per plugin tells what happened: `cloned`, `updated`,
`unchanged` or `failed`. A failure of one plugin does not stop the others, but the exit code is 1.

**`list`** (alias `ls`) prints a table with the columns `NAME ENABLED GIT LOCKED INSTALLED STATE LOAD`.
`LOAD` is the result of loading the plugin as the server would (`load OK` or `load error: ...`). The state is
one of:

| State | Meaning |
|-------|---------|
| `ok` | cloned, clean, and at the locked commit |
| `missing` | has `git`, but is not installed: run `sync` |
| `dirty` | the clone has local changes (or git could not report its status) |
| `mismatch` | the installed commit differs from the lock (or there is no lock entry): run `sync` |
| `foreign` | the directory exists but is not a git clone of the configured URL; `sync` and `remove` never touch it |
| `local` | the entry has no `git`: a plugin you manage yourself |

**`remove`** (alias `rm`) deletes the clone and the lock entry. It only works for plugins with a lock entry,
refuses a directory that is not a clone of the locked URL, and does not edit the config: remove the `plugins`
entry yourself, or the next sync installs it again.

**The lock file** is `plugins.lock.json` next to the config file (the working directory when no config file
is in use). It is a JSON object keyed by plugin name, with sorted keys; commit it to pin your installation:

```text
{
  "hello": {
    "git": "https://example.com/you/mcplogview-hello.git",
    "ref": "v1.0.0",
    "commit": "0123456789abcdef0123456789abcdef01234567"
  }
}
```

A `commit` that is not a full 40 or 64 digit hex hash is rejected.

> **Security:** a plugin is code that runs with your user's rights, limited only by the
> [sandbox and capabilities](#security-model) you grant. `sync` runs no plugin code, but you load it afterwards.
> Read a plugin before enabling it, prefer a fixed tag or commit as `ref`, and review the diff before every
> `sync --update`.

## Testing plugins

```text
mcplogview plugin test [-run REGEXP] DIR
```

runs every `DIR/tests/*_test.lua` against the plugin in `DIR` (the directory with `init.lua`), with no
config file, network, SSH or server. Each file runs in a fresh sandboxed Lua VM with a global `test` table.
The `-run` regexp selects the cases whose name **or** file name matches. Output is one line per case
(`ok   file: case` or `FAIL file: case: reason`) and a summary `N passed, M failed`. Exit codes: `0` all
passed, `1` a case failed (or no tests were found, or the suite could not run), `2` wrong usage or an invalid
`-run` regexp.

The `hello` example in [`examples/plugins/hello`](../examples/plugins/hello/init.lua) comes with
[tests](../examples/plugins/hello/tests/hello_test.lua):

```text
$ mcplogview plugin test examples/plugins/hello
ok   hello_test.lua: lists the files
ok   hello_test.lua: reads pages and follows appends
ok   hello_test.lua: counts lines
ok   hello_test.lua: needs opts.dir
4 passed, 0 failed
```

```lua
test.case("counts lines", function()
  local dir = test.fixture { ["app.log"] = "1\n2\n3" }
  local h = test.load { allow = { "fs" }, fs = { roots = { dir } }, opts = { dir = dir } }
  test.eq(h:tool("hello_count_lines", { file = "app.log" }), { file = "app.log", lines = 3 })
  test.errors(function() h:tool("hello_count_lines", { file = "../x" }) end, "bad file name")
end)
```

Rules:

- A file's top level only defines cases and may hold shared helpers and **globals, which are shared by the
  cases of that file** (each file has its own VM). Cases run one after another in file order; a failing case
  does not stop the others. A load error in a file counts as one failure.
- Every case has a **timeout** of two minutes (`timed out after 2m0s` is the failure). Everything a case
  creates (fixtures, loaded plugins) is removed after it.
- Everything except `test.case` and the assertions works only inside a case.

### The `test` API

| Function | Description |
|----------|-------------|
| `test.case(name, fn)` | define a case; cases cannot be nested |
| `test.fixture{ ["rel/path"] = "contents", ... }` | create a temporary directory with these files and return its path. Paths must be relative and inside it; `test.fixture{}` makes an empty one |
| `test.write(path, contents)` | create or overwrite a file (parents are created) |
| `test.append(path, contents)` | append to a file, creating it if needed |
| `test.remove(path)` | delete a file (an error if it is missing) |
| `test.load(cfg)` | load the plugin under test in a fresh host and return a handle `h`. `cfg` is a `plugins[]` entry as a table (`allow`, `fs`, `ssh`, `opts`, ...; `name` defaults to the directory name); an unknown key or an invalid entry is an error |
| `test.stub_exec(script, output)` | make `mcp.ssh.exec` return `output` for exactly this script instead of running it |
| `test.ssh_down(host)` | make SSH calls to `host` fail like an unreachable station |
| `test.exec_log()` | the scripts executed so far (list of strings) |
| `test.eq(a, b [, msg])` | deep equality (tables by content) |
| `test.ok(v [, msg])` | `v` is truthy |
| `test.contains(s, sub [, msg])` | string contains substring |
| `test.errors(fn, sub [, msg])` | `fn` raises an error containing `sub` (a plain substring) |
| `test.fail([msg])` | fail the case |

`test.write`, `test.append` and `test.remove` only accept paths **inside a fixture of the running case**.
`h:read` is the usual way to test a source that follows a growing file: read, `test.append`, read again.

The handle returned by `test.load` has these methods:

| Method | Description |
|--------|-------------|
| `h:status()` | `ok, err`: whether the plugin loaded, and the load error |
| `h:list(parent)` | the children of `parent` as a list of `{id, label, leaf, expandable, description, hint}`; without `parent`, the source roots |
| `h:read(id, cursor, max_lines)` | read a leaf; returns `{lines, cursor, state, eof}`. `max_lines` defaults to the tail size |
| `h:tool(name, args)` | call a registered tool; returns its result table |
| `h:skill(source_id)` | the registered skill text, or `nil` |

Errors raised by the plugin surface as errors in the case; wrap the call in `test.errors` to assert them.

**Cursors.** The `cursor` a read returns is the opaque string an agent would pass back; pass it to the next
`h:read`. `chunk.state` is the plugin's own cursor state (what its `read` returned as `cursor`), so you can
assert it. You may also pass a **plugin cursor state** (a table, number or boolean; the first read passes
`nil`) as the cursor, as `h:read("hello/app.log", 5, 10)` does in the example. A plugin's string state cannot
be passed this way; use the `cursor` string of the previous read.

**SSH.** Plugins with `ssh` access run against a local backend: `mcp.ssh.exec` runs the script with `sh -c`
on your machine (the host is ignored; the working directory is a temporary one), so **a POSIX `sh` is
needed** and scripts must not depend on the real station. `mcp.ssh.tail` reads a local file. Use
`test.stub_exec` to return fixed output (matched on the exact script text), `test.exec_log()` to check what ran,
and `test.ssh_down("host")` to see how the plugin handles an unreachable station (the error text lacks the
host name; the core adds it).

## How plugin sources appear to agents

Plugin sources are listed by `list_logs` next to the configured sources, always with `"expandable": true`.
An agent descends with the `parent` argument:

```text
list_logs                          -> ..., {"id": "stations", "label": "Stations", "expandable": true, "plugin": "stations"}
list_logs parent=stations          -> the plugin's first level
list_logs parent=stations/45       -> next level, ids are the parent id + "/" + child name
list_logs parent=stations/45/network
                                   -> {"id": "stations/45/network/network", "leaf": true, ...}
```

- A child id is its parent id and the child's `name` joined with `/`. The first segment is the plugin's
  registered source id; the rest is the *path* handed to the plugin as a Lua array of segments.
- Entries with `"leaf": true` are readable logs; the others have `"expandable": true`.
- `read_log_chunk`, `follow_log` and `get_log_skill` accept any plugin source id. A plugin source id has at
  most 8 segments, each at most 128 bytes, valid UTF-8, no control characters.
- `get_log_skill` returns the text registered with `mcp.skill.register` for the root; every child uses its
  root's skill.
- Cursors are issued by the plugin, wrapped by the server with the full source id, and are only valid for
  that source. A cursor from another source is rejected.
- `follow_log` on a plugin source polls the plugin's `read` until lines appear or the timeout ends.
- Plugin tools show up next to the four built-in ones in the tool list.
- **Plugin origin is marked.** `list_logs` entries of plugin sources (roots and children) and the
  `read_log_chunk` / `follow_log` results of plugin sources carry `"plugin": "<plugin name>"`, e.g.
  `{"id": "stations", ..., "expandable": true, "plugin": "stations"}`; configured sources never have this field, so
  their output is unchanged. Results of plugin-registered *tools* are not marked (the plugin shapes them
  freely).

See [Using it with agents](using-with-agents.md#list_logs) for the tools themselves.

## The `mcp` API

Plugins run in a sandboxed Lua 5.1 state (gopher-lua) with a global `mcp` table.

### `mcp.source.register`

```lua
mcp.source.register {
  id          = "demo",                 -- root id, required
  label       = "Demo logs",            -- default: the id
  description = "What this source is",  -- optional
  hint        = "Where to start",       -- optional
  children    = function(path) ... end, -- required
  read        = function(path, cursor, max_lines) ... end, -- required
}
```

- `id`: lowercase letters, digits, `_`, `-`, starting with a letter or digit, at most 128 bytes. It must not
  be used by a configured source or by another plugin's source, and not twice by the same plugin.
- `children(path)` lists the entries below a path. `path` is an array of segments *below the root* (`{}` for
  the root itself, `{"45", "communication"}` for `demo/45/communication`). Return an array of
  `{name = ..., label = ..., description = ..., leaf = ...}`. Only `name` is required; the label defaults to the
  name; `leaf = true` marks a readable log. A name must be a valid segment: non-empty, no `/`, at most 128
  bytes, no control characters. Return `{}` for an empty level.
- `read(path, cursor, max_lines)` returns up to `max_lines` lines (the server passes 1 to 500) as
  `{lines = {...}, cursor = <any data>, eof = true|false}`.
  - `cursor` handed to the function is `nil` on the first read, otherwise exactly the value (any
    JSON-like data: string, number, boolean, table) the function returned last time.
  - `lines` must be an array of strings; returning more than `max_lines` is an error.
  - `eof = true` means "nothing more right now"; `follow_log` keeps polling with the returned cursor.
- Raise an error (`error("message", 0)`) to fail a call; the agent sees `Tool error (...)` with your message.

### `mcp.tool.register`

```lua
mcp.tool.register {
  name        = "count_lines",
  description = "Count the lines of a file",
  schema      = {
    type = "object",
    properties = { path = { type = "string" } },
    required = { "path" },
  },
  handler = function(args) ... return {lines = 3} end,
}
```

- `name`: lowercase letter first, then lowercase letters, digits, `_`; at most 64 characters. It must not be
  one of the built-in tools (`list_logs`, `get_log_skill`, `read_log_chunk`, `follow_log`) or a name another
  plugin already registered.
- `schema` is a JSON Schema for the arguments, written as a Lua table. Its `type` must be `"object"` (it is
  added when missing; leaving `schema` out accepts any object). Arguments are validated against it by the
  server before the handler runs; a mismatch is reported to the agent without calling Lua.
  Write an empty schema as `{type = "object"}`, **not** `{}`: an empty Lua table converts to an array and is
  rejected.
- `handler(args)` receives the arguments as a Lua table. What it returns is converted to JSON and sent to the
  agent as the tool's text result (the same way the built-in tools reply). Returning nothing yields `null`.
  Values that are not JSON-like (functions, cyclic tables, nesting deeper than 32 levels) are errors.
- An error raised by the handler (or a timeout) becomes a tool error result: `Tool error (<name>): plugin
  <plugin>: tool <name>: <message>`.

### `mcp.skill.register`

```lua
mcp.skill.register { source = "demo", text = "# Demo logs\n\nStart at list_logs parent=demo ..." }
```

Sets the usage skill agents get from `get_log_skill` for the root `source`, which must be a root this plugin
registered (register the source first). Once per root; `text` is Markdown, like the files in
[`skillsDir`](using-with-agents.md#skills-per-source-guidance).

### `mcp.on`

```lua
mcp.on("before_tool", function(ev)
  if ev.tool == "follow_log" then
    return false, "follow_log is switched off here; page with read_log_chunk instead"
  end
end)
```

`mcp.on(event, fn)` registers a hook. Hooks run in registration order, plugins in config order.

| Event | Argument | Effect |
|-------|----------|--------|
| `config_reload` | `{sources = n}`: the number of enabled configured sources plus plugin roots | Fired on a plugin's instance after a *later* plugin reload (not for the first load) |
| `before_tool` | `{tool = "read_log_chunk", args = "sourceId=app maxLines=100"}` | Return `false` (exactly `false`) to deny the call; an optional second return value is the reason |
| `after_tool` | same, plus `error = "..."` when the call failed | Informational; the return value is ignored. Runs in the background after the response was sent, also when the request was cancelled |
| `shutdown` | none | Fired once when the server stops, before plugins are unloaded |

- `args` is the same short, log-style summary the server writes to its own log (not the raw JSON arguments). For plugin tools it is only `plugin=<name>`; the tool's own arguments are not passed to hooks.
- `before_tool` and `after_tool` fire for the built-in tools and for plugin tools.
- A denied call returns an error result to the agent: `Tool denied by plugin <name>: <reason>`. The first
  denying hook wins; later hooks do not run. Always return a reason when denying: without one the message
  is `Tool denied by plugin <name>: ` with nothing after the colon.
- Hooks **fail open**: if a hook raises an error or runs out of time, the failure is logged to the self-log and
  the call proceeds.
- One hook call gets at most **5 seconds** (or the plugin's `timeoutSeconds`, if that is shorter).

**`before_tool` is advisory, not an access-control boundary.** Use it for conveniences (switching a tool off,
steering agents, counting calls), never to keep data secret:

- It fails open on a hook error, on the 5 second timeout, *and when the plugin is busy*: hooks run through
  the same per-plugin call slot as the plugin's own tools, `children` and `read` calls, so while a slow call
  of that plugin runs, the hook waits up to 5 seconds and then the tool call goes ahead without it.
- A rule keyed on the `sourceId=` text of `args` is easy to sidestep: the same log can be reached under other
  ids (a plugin that matches path segments loosely, for example, reads the same log under `stations/45/net/net`
  as under `stations/45/network/network`).
- To keep a log away from agents, do not configure it (or do not let a plugin expose it).

While a plugin is busy, **every** tool call (on any source) can wait up to 5 seconds behind that plugin's
`before_tool` hooks; `after_tool` hooks run in the background and do not delay the response, but they occupy
the plugin's call slot like any other call.

### `mcp.ssh`

Available only with `"allow": ["ssh"]`.

```lua
local s = mcp.ssh("192.0.2.45")                    -- validates the host, returns a handle
local out = s:exec("uptime")                       -- run a command, returns stdout
local r = s:tail("/var/log/app.log", 0, 100)       -- read lines from a remote file
-- r.lines (array), r.offset (next byte offset), r.size (file size), r.eof (true at the end)
```

- **Host rules.** The host must be a plain IPv4 literal inside one of the plugin's `allowedCIDRs`. Host
  names, IPv6 addresses, zones and non-canonical forms (leading zeros) are rejected; DNS never decides
  where a plugin connects. Otherwise: `mcp.ssh: host "x" not allowed (want an IPv4 address in ...)`.
- **User and key** come from the config (`ssh.user`, `ssh.identityFile`, `ssh.port`,
  `ssh.strictHostKeyChecking`); the plugin cannot override them.
- `s:exec(cmd)`: `cmd` is a shell script run by the remote shell. At most **8 KiB**, no NUL bytes. The result
  is the command's stdout, at most **4 MiB**; more is an error (`output too large`), never a silent cut. A
  non-zero exit status raises an error naming the host; ssh's own failures (exit 255) read
  `cannot reach <host>: ...`.
- `s:tail(path, offset, max_lines)`: `path` must be absolute (no NUL or line breaks), `offset` a non-negative
  integer byte offset, `max_lines` optional (default 100, clamped to 1 to 500). It returns whole lines
  starting at `offset`; use the returned `offset` for the next call. Each call transfers at most 4 MiB.
- **Quote every value** you put into a command: `mcp.str.quote` (below). The Go side cannot know which parts
  of your command are data.
- A call is bounded by the plugin's `timeoutSeconds`, and one ssh call by 60 seconds. Connecting takes at most
  **10 seconds** (`ConnectTimeout`), and ssh keepalives (`ServerAliveInterval=5`, `ServerAliveCountMax=2`)
  notice a station that went down behind an open shared connection in about 10 seconds, instead of the call
  hanging until the 60 second limit.
- **Connection sharing.** Calls to the same host reuse one ssh connection (`ControlMaster`, kept for 60
  seconds), so a plugin that makes several calls per request does not pay for a handshake each time.

### `mcp.fs`

Available only with `"allow": ["fs"]`. Read-only access to local files under the configured `roots`.

```lua
for _, e in ipairs(mcp.fs.list("/var/log/myapp")) do   -- e.name, e.dir; sorted by name
  mcp.log.info(e.name .. (e.dir and "/" or ""))
end
local f = mcp.fs.read("/var/log/myapp/app.log", 0, 65536)  -- f.data (string), f.size (file size)
```

- `mcp.fs.list(dir)` returns an array of `{name, dir}` sorted by name. At most 10000 entries per directory
  (more is an error).
- `mcp.fs.read(path, offset, max_bytes)` reads from byte `offset` (default 0) at most `max_bytes` (default
  and maximum 1 MiB; between 1 and 1048576). `size` is the file's total size. Only regular files are read
  (FIFOs and devices are refused).
- **Containment.** The path must be absolute. It is cleaned, symlinks are resolved, and the result must lie
  inside (or be) one of the roots, which are resolved the same way. A path outside the roots (including
  through `..` or a symlink pointing out) is refused with `path outside the allowed roots`, and the error does
  not reveal whether it exists. Symlinked directories in a listing are marked `dir = true` only if their
  target is inside a root.

### Helpers

| Function | Meaning |
|----------|---------|
| `mcp.log.info(msg)`, `mcp.log.error(msg)` | Write to the server's self-log (see [Debugging](#debugging)), prefixed `plugin <name>` |
| `mcp.json.encode(value)` | Lua value to a JSON string. Arrays (and empty tables) become JSON arrays, other tables objects |
| `mcp.json.decode(text)` | JSON string to a Lua value (numbers are floats, `null` becomes `nil`) |
| `mcp.str.quote(s)` | `s` as a single-quoted POSIX shell word, safe to splice into an `exec` command |
| `mcp.str.lines(s)` | Split on `\n` into an array; a trailing newline adds no empty line; `""` gives `{}` |
| `mcp.time.shift_days("YYYY-MM-DD", n)` | The date `n` days later (negative: earlier), as `YYYY-MM-DD` |

The Lua standard library is limited, see [Security model](#security-model).

## Worked examples

### A minimal source plugin

[`examples/plugins/hello/`](../examples/plugins/hello/init.lua) is a complete version of this example (an
fs-backed source plus a `hello_count_lines` tool, configurable directory) with Lua tests in
[`tests/hello_test.lua`](../examples/plugins/hello/tests/hello_test.lua); the project's Go tests run them.

`plugins/demo.lua`, configured with `{ "name": "demo", "allow": ["fs"], "fs": { "roots": ["/var/log/myapp"] } }`:
it exposes the files of one directory as a flat list of logs.

```lua
local DIR = "/var/log/myapp"

local function children(path)
  if #path > 0 then return {} end            -- a flat tree: only the root has children
  local out = {}
  for _, e in ipairs(mcp.fs.list(DIR)) do
    if not e.dir then out[#out + 1] = { name = e.name, leaf = true } end
  end
  return out
end

local function read(path, cursor, max_lines)
  if #path ~= 1 then error("read demo/<file>", 0) end
  local offset = cursor or 0                  -- nil on the first read
  -- read only as many bytes as max_lines lines will plausibly need
  local f = mcp.fs.read(DIR .. "/" .. path[1], offset, max_lines * 512)
  local data = f.data

  -- take at most max_lines complete lines; consumed counts the bytes they use
  local lines, pos, consumed = {}, 1, 0
  while #lines < max_lines do
    local nl = data:find("\n", pos, true)
    if not nl then break end
    lines[#lines + 1] = data:sub(pos, nl - 1)
    consumed, pos = nl, nl + 1
  end
  -- the data reaches the end of the file and ends without a newline: the last line
  -- counts as a line (like the built-in sources do); it only fits if max_lines allows
  if #lines < max_lines and pos <= #data and offset + #data >= f.size then
    lines[#lines + 1] = data:sub(pos)
    consumed = #data
  end
  -- a single line longer than the buffer: hand it out in pieces so reading never stalls
  -- (a piece may split a multibyte UTF-8 character; this is an example, not production code)
  if #lines == 0 and #data > 0 and offset + #data < f.size then
    lines[1], consumed = data, #data
  end

  local next_offset = offset + consumed
  return { lines = lines, cursor = next_offset, eof = next_offset >= f.size }
end

return {
  api_version = 1,
  setup = function(opts)
    mcp.source.register {
      id = "demo", label = "Demo logs",
      hint = "list_logs parent=demo shows the files",
      children = children, read = read,
    }
    mcp.skill.register { source = "demo", text = "Files of /var/log/myapp. Read them with read_log_chunk." }
  end,
}
```

The agent then runs `list_logs parent=demo`, gets entries such as `demo/app.log`, and reads them with
`read_log_chunk sourceId=demo/app.log`.

### A tiny tool plugin

`plugins/hello.lua`, configured with `{ "name": "hello" }`:

```lua
return {
  api_version = 1,
  setup = function(opts)
    mcp.tool.register {
      name = "shout",
      description = "Upper-case a message",
      schema = { type = "object", properties = { text = { type = "string" } }, required = { "text" } },
      handler = function(args)
        return { text = args.text:upper() }
      end,
    }
  end,
}
```

### Real-world example: an ssh log-tree plugin

A larger plugin, kept in its own repository and installed with `plugin sync`, can expose the daily log files
of many small machines in one subnet:

- **Tree.** `stations/<last octet>/<dir>[/<subdir>]/<family>`: the agent types the machine's last octet, then
  browses the directories the machine's own `ls -1p` lists. A *family* is a stream of daily files
  `<family>.log.<YYYY-MM-DD>`.
- **Loose names.** Each segment is matched against the listed names (exact, then case-insensitive, then the
  single substring match); ambiguous or unknown names fail with the candidates.
- **Reads.** Without a cursor, a read returns the last `maxLines` lines of the newest file (like `tail`); with a
  cursor (`{file, offset}`) it continues forward and rolls over to the next day.
- **A search tool** greps a directory over a date window (`days`, `from`/`to`, `context`, `max_matches`), with
  every value quoted by `mcp.str.quote`.
- **Tests** run against fixture trees with `test.fixture`, `test.stub_exec("date +%F", ...)` for the remote
  clock and `test.ssh_down` for an unreachable machine.

Every read re-lists the remote directories, so a `follow_log` poll costs several ssh round-trips: such
plugins should tell agents (in their skill) to use `pollIntervalMs` 1000.

## Security model

- **Plugins are code.** They run with the privileges of the user that runs the mcplogview server. Only
  install plugins you trust. They are installed by the administrator, on disk, and nowhere else: in
  [HTTP mode](http-mode.md) there is no way to upload or enable a plugin remotely, and agents cannot change
  the plugin configuration.
- **The Go side enforces the limits**, not the plugin: which hosts an ssh plugin may reach (IPv4 in the
  allowed CIDRs), as which user and key, the size of commands and output, the roots and containment of file
  access, the number of entries and bytes of a listing or read, and the timeouts.
- **Sandbox.** A plugin gets only the Lua `base`, `table`, `string` and `math` libraries. There is no `io`,
  `os`, `package` or `debug`, and these globals are removed: `dofile`, `loadfile`, `load`, `loadstring`,
  `require`, `module`, `print`, `getfenv`, `setfenv`, `newproxy`, `_printregs`. `string.dump` is removed; `string.rep` is
  capped at 16 MiB and `string.format` widths and precisions at 10000. Each plugin has its own Lua state.
- **Shell safety is the plugin's job.** Values put into `s:exec` commands must go through `mcp.str.quote`.
  A good pattern: only names read from the remote host itself (by `ls`) and strictly validated numbers and
  dates reach a command; user text is only matched against those names in Lua.
- **Failures are isolated.** A plugin that fails to load, errors, or times out affects only itself; other
  plugins and the built-in sources keep working.
- **`before_tool` hooks are advisory.** They fail open (on errors, timeouts and a busy plugin) and can be
  sidestepped through alternative source ids, so they are not a way to protect logs; see [`mcp.on`](#mcpon).

## Known limitations

- **Calls into one plugin are serialized.** One slow ssh call delays other calls to the same plugin (they wait
  up to the plugin's timeout, or until their own request is cancelled). An unreachable station fails after the
  10 s connect timeout; a station that dies during a call on a shared connection is noticed in about 10 s
  through ssh keepalives; any single ssh call ends after 60 s at the latest.
- **Plugins load at startup, synchronously.** Loading a plugin file and running its `setup` happen before the
  server answers the MCP `initialize` request, so a slow `setup` delays startup (by up to the plugin's
  `timeoutSeconds`). Keep `setup` to registrations; do not contact remote hosts there.
- **The Lua timeout only interrupts pure-Lua loops.** Work done inside Go functions is not interrupted:
  string doubling (`s = s .. s`) or pathological patterns in `string.find` / `string.gsub` are *not* defended
  against. Plugins are trusted admin code.
- **Plugin files are not watched.** After editing a plugin, change the config (any `plugins` / `pluginDirs`
  change reloads all plugins) or restart the server.
- **`schema = {}` is rejected.** An empty Lua table is an empty JSON array, so use `schema = {type = "object"}`.
  The same holds for results: an empty Lua table is returned as `[]`.
- **A tool call racing a reload can fail** with `plugin <name>: tool <tool>: unloaded`; the agent can simply
  retry.
- **Cursors from another source are rejected** for plugin sources (a plugin source needs a cursor it issued
  itself).
- **`before_tool` cannot enforce a policy.** It fails open on a hook error, on its 5 second timeout and when
  its plugin is busy with another call, and rules keyed on `sourceId` strings can be bypassed through fuzzy
  or alternative ids (see [`mcp.on`](#mcpon)).
- **Hooks and reloads can wait.** A hanging hook costs up to 5 seconds per hook, and tool calls wait for the
  `before_tool` hooks: while a plugin with `before_tool` hooks is busy, every tool call (any source) can wait
  up to 5 seconds. `after_tool` hooks run in the background. `config_reload` and `shutdown` hooks run while
  the reload lock is held, so a hanging hook delays a reload (or shutdown) by up to 5 seconds per hook.

## Debugging

Plugin problems are written to the server's own log (the file at `logging.path`, also readable by agents as
the source `mcplogview-self`; see [Troubleshooting](troubleshooting.md)):

- `plugin <name>: load failed: ...`: the file was not found in `pluginDirs`, did not return a table with
  `api_version = 1`, raised an error, registered a duplicate id or tool name, and so on.
- `plugin <name>: loaded (N sources, M tools)`: it loaded.
- `plugin <name>: <event> hook ignored: ...`: a hook failed or timed out and the call went ahead.
- `tool=<name> denied by plugin <plugin> ...` for denied calls, and the output of `mcp.log.info` /
  `mcp.log.error`.

The load status is not yet shown by any command; look in the self-log. Enabling `logging.debug` is not needed
for these messages.
