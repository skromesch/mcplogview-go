# Using it with agents

What an agent sees once mcplogview is connected, and how to make that as useful as possible.

## Connecting

```bash
mcplogview register          # prints the command for your setup
mcplogview register --run    # or runs `claude mcp add` for you
```

- **stdio** (default): the agent host starts `mcplogview` itself, in its working directory, which is how a
  per-project `./mcplogview.json` is picked up. For Claude Code run `claude mcp add mcplogview -- mcplogview` inside
  the project.
- **HTTP**: the host connects to a running central instance. See [HTTP mode](http-mode.md).

## The four tools

Recommended flow: `list_logs` → `get_log_skill` → `read_log_chunk` / `follow_log`.

### `list_logs`

No arguments. Returns the sources that are switched on:

```json
{ "logs": [ { "id": "app", "label": "App log",
              "description": "Storefront backend", "hint": "Start with ERROR lines." } ] }
```

`description` and `hint` are left out when you did not set them.

### `get_log_skill`

Arguments: `sourceId`. Returns guidance for that log: how it is laid out and how to read it. See
[Skills](#skills-per-source-guidance) below.

### `read_log_chunk`

| Argument | Type | Default | |
|----------|------|---------|--|
| `sourceId` | string | required | A source id from `list_logs` |
| `cursor` | string or null | none | The `cursor` of the previous result; omit to start at the beginning |
| `maxLines` | integer 1-500 | 100 | Lines to return |

Result:

```json
{
  "lines": [
    "2026-10-17T08:15:00Z INFO server started",
    "2026-10-17T08:15:01Z ERROR payment gateway timeout"
  ],
  "cursor": "eyJpbm9kZSI6NDIxNDYwMTEzLCJvZmZzZXQiOjkyfQ==",
  "eof": false
}
```

Pass `cursor` back to get the next chunk. `eof` is `true` when this chunk reached the end of what exists *right
now*; more may arrive later.

### `follow_log`

Waits a short time for **new** lines, so an agent can reproduce a problem and watch it appear.

| Argument | Type | Default | |
|----------|------|---------|--|
| `sourceId` | string | required | |
| `cursor` | string or null | none | Where to continue; take it from an earlier `read_log_chunk` |
| `maxLines` | integer 1-500 | 100 | |
| `timeoutMs` | integer 1-30000 | 5000 | How long to wait for new lines |
| `pollIntervalMs` | integer 10-1000 | 200 | How often to check |

It returns as soon as there are lines. If nothing new arrives within `timeoutMs` it returns
`{"lines": [], "cursor": "...", "eof": true}` with a cursor to continue from.

### Errors

Problems come back as a tool result with `isError: true` and a message, not as a protocol failure:

```
Tool error (read_log_chunk): unknown source: nope
Tool error (read_log_chunk): maxLines must be between 1 and 500 (got 9999)
```

Typical causes are listed in [Troubleshooting](troubleshooting.md).

## Cursors

A cursor is an opaque string. **Agents should treat it as a token: pass back exactly what they were given.**
Inside, it records the position, and what that is depends on the source:

| Source | Cursor holds | Reading is... |
|--------|--------------|---------------|
| `local` | file identity + byte offset | restarted from the beginning if the file is replaced or truncated |
| `ssh` | host/file + byte offset | at most 4 MiB per call, ending on a whole line |
| `docker` | last timestamp + hashes of the last lines + container id | reset to the container's first line if the container is recreated |

A cursor from one source is ignored by another (reading simply starts from the beginning).

Always read large logs **in chunks** with the cursor. Do not request the whole file at once.

Docker lines come with Docker's timestamp in front, e.g. `2026-10-17T08:20:03.000000000Z INFO ...`.

## Skills: per-source guidance

`get_log_skill` returns a structured summary plus a markdown body. mcplogview builds a generic one for every
source. To give an agent real knowledge about a log (the format, what the levels mean, what to search for first),
write a **skill file**: `<skillsDir>/<sourceId>.md`, by default `log-skills/<sourceId>.md` next to the config.

```markdown
---
name: shop-app-log
sourceId: app
version: 2
hint: Errors are logged with level ERROR; payment problems mention "gateway".
tags: [backend, payments]
---
# Shop app log

Lines look like `2026-10-17T08:15:01Z ERROR message`.

- Search for `ERROR` first, then read the lines around it.
- `payment gateway timeout` means the card provider was slow, not a bug in our code.
```

Front matter keys (all optional): `name`, `sourceId` (if present it must match the file's source id), `version`
(integer, 1 or more), `hint`, `tags` (`[a, b]` or a single value). The body is returned to the agent verbatim as
`renderedMarkdown`, and `hint` overrides the source's own `hint`.

The result for the file above:

```json
{
  "sourceId": "app",
  "hint": "Errors are logged with level ERROR; payment problems mention \"gateway\".",
  "origin": "file",
  "warnings": [],
  "structured": {
    "name": "shop-app-log", "sourceId": "app", "version": 2,
    "tags": ["backend", "payments"],
    "purpose": "Storefront backend",
    "quickStart": ["Call read_log_chunk for app with maxLines=100",
                   "Use follow_log for app during reproductions"],
    "queries": ["read_log_chunk {\"sourceId\":\"app\",\"maxLines\":100}",
                "follow_log {\"sourceId\":\"app\",\"timeoutMs\":5000,\"pollIntervalMs\":200}"],
    "troubleshooting": ["If no lines are returned, verify source path/ssh connectivity.",
                        "If cursor handling is unclear, continue with returned cursor values only."]
  },
  "renderedMarkdown": "# Shop app log\n\nLines look like ..."
}
```

`origin` tells you where it came from:

| `origin` | Meaning |
|----------|---------|
| `file` | read from your skill file |
| `fallback` | no usable file: a generic skill built from the source's `label`, `description` and `hint`. `warnings` says why (e.g. `skill file not found: ...`) |
| `builtin` | the server's own log (`mcplogview-self`) has a skill about debugging mcplogview |

Broken front matter never fails the call: you get defaults plus a warning.

`description` and `hint` on the source itself already go a long way. Add a skill file for logs that need more.

## The server's own log

With `logging.enabled` (the default) the server's own log is a source called `mcplogview-self`. An agent can read it
to see which tools were called and what failed (`tool=read_log_chunk failed ...`). Set `logging.debug` to also
record raw MCP traffic, which helps when a client misbehaves, but the log may then contain log content.
