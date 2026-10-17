# Configuration reference

The config is one JSON file. You can create and edit it with [`mcplogview configure`](interactive-setup.md) or
the [`add` / `remove` / `enable` / `transport` commands](cli.md); this page is for editing it by hand and for
looking up what a key does.

## Which file is used

First match wins:

| # | Source | Notes |
|---|--------|-------|
| 1 | `--config FILE` | must exist |
| 2 | `$LOG_SOURCES_CONFIG` | must exist |
| 3 | `./mcplogview.json` | current directory: a per-project config |
| 4 | `~/.config/mcplogview/config.json` | user-wide (`$XDG_CONFIG_HOME` is honoured) |
| 5 | built-in defaults | only the server's own log source |

A path given explicitly (1, 2) that does not exist is an error, not a silent fall-through.
`mcplogview where` prints the file in effect.

## Paths

- **Relative paths** in a config (`logging.path`, `skillsDir`, local source `path`, `identityFile`,
  `httpsKey`, `httpsCert`) are resolved against **the directory of the config file**, not the directory you
  started the program in. `~/` is expanded.
- **Remote paths** (the `path` of an `ssh` source) are used as written.
- Edit commands store local paths as absolute paths.

## Minimal config

```json
{
  "sources": [
    { "id": "app", "label": "App log", "path": "logs/app.log" }
  ]
}
```

Everything not mentioned has a default, so this is a complete, working config.

## A config using everything

[`examples/full.json`](examples/full.json), checked by the project's tests:

```json
{
  "transport": { "mode": "stdio" },
  "logging": {
    "enabled": true,
    "debug": false,
    "path": "~/.local/state/mcplogview/mcplogview-server.log",
    "maxFileSize": 10485760,
    "maxBackups": 5
  },
  "skillsDir": "log-skills",
  "sources": [
    { "type": "local", "id": "app", "label": "Application log",
      "description": "Main backend service log", "hint": "Start with ERROR lines.",
      "path": "logs/app.log" },
    { "type": "ssh", "id": "prod-api", "label": "Prod API log",
      "host": "prod-api", "port": 22, "user": "ubuntu",
      "identityFile": "~/.ssh/id_ed25519", "strictHostKeyChecking": true,
      "path": "/var/log/myapp/api.log" },
    { "type": "docker", "id": "shop-api", "label": "Shop API container",
      "container": "shop-api" },
    { "type": "docker", "id": "prod-worker", "label": "Prod worker container",
      "container": "worker", "host": "prod-api", "user": "ubuntu",
      "identityFile": "~/.ssh/id_ed25519" },
    { "type": "local", "id": "old-service", "label": "Old service (switched off)",
      "path": "/var/log/old-service.log", "disabled": true }
  ]
}
```

## `sources`

An array of log sources. Ids must be unique. See [Log sources](sources.md) for how each type behaves.

| Key | Types | Required | Meaning |
|-----|-------|----------|---------|
| `type` | all | no (default `local`) | `local`, `ssh` or `docker` |
| `id` | all | yes | Unique id the agent uses. Letters, digits, `_`, `.`, `-` are safe |
| `label` | all | yes | Human readable name |
| `description` | all | no | What the log is; shown in `list_logs` and the generated skill |
| `hint` | all | no | Where to start reading; shown to the agent |
| `disabled` | all | no | `true` hides the source from agents without deleting it |
| `path` | `local`, `ssh` | yes | The log file (remote path for `ssh`) |
| `host` | `ssh` (required), `docker` (optional) | | Host name, IP or `~/.ssh/config` alias. A `docker` source with a `host` is read over SSH |
| `user` | `ssh` (required), `docker` | | SSH user |
| `identityFile` | `ssh` (required), `docker` | | SSH private key (`~/` allowed) |
| `port` | `ssh`, `docker` | no | SSH port. `ssh` sources default to 22; for `docker` leave it out to use your SSH config |
| `strictHostKeyChecking` | `ssh`, `docker` | no (default `false`) | Verify the host key. See the security note in [Log sources](sources.md#ssh-files) |
| `container` | `docker` | yes | Container name or id (letters, digits, `_`, `.`, `-`) |

## `transport`

How agents reach the server. The default is stdio: each agent starts its own process.

| Key | Default | Meaning |
|-----|---------|---------|
| `mode` | `stdio` | `stdio`, `http` (Streamable HTTP) or `https-dev` (HTTPS) |
| `host` | `127.0.0.1` | Listen address for http modes. `0.0.0.0` = all interfaces |
| `port` | `3333` | Listen port (1-65535) |
| `path` | `/mcp` | URL path of the endpoint |
| `authToken` | none | Bearer token required on every HTTP request (min. 16 chars). Overridden by `$MCPLOGVIEW_TOKEN` |
| `allowedOrigins` | none | Extra browser origins allowed without a token; `"*"` allows any |
| `allowNoAuth` | `false` | Permit listening on a non-loopback address without a token |
| `httpsKey`, `httpsCert` | none | PEM files for `https-dev`. If both are set they are used; otherwise (or if they cannot be loaded) a self-signed certificate is generated |

See [HTTP mode](http-mode.md) for how these fit together. A config with an `authToken` is written with
`0600` permissions. Changes to `transport` need a restart of a running server.

## `logging`

The server's own log, written to a file (never to stdout: in stdio mode stdout carries MCP messages only).
It is also offered to agents as a source called `mcplogview-self`, handy for debugging mcplogview itself.

| Key | Default | Meaning |
|-----|---------|---------|
| `enabled` | `true` | Write the self-log and expose it as a source |
| `debug` | `false` | Also log raw MCP traffic (stdio) and HTTP details. This can include log content |
| `path` | `~/.local/state/mcplogview/mcplogview-server.log` | Log file (`$XDG_STATE_HOME` is honoured) |
| `maxBodyChars` | `4000` | Truncation of logged raw messages (128-20000) |
| `selfSourceId` | `mcplogview-self` | Id of the self-log source |
| `selfSourceLabel` | `MCP Server Self Log` | Its label |
| `maxFileSize` | `10485760` | Rotate at this size in bytes (1024 to 1 GiB) |
| `maxBackups` | `5` | Rotated files to keep (`.1` ... `.N`) |

## `skillsDir`

Directory with optional per-source guidance files, `<skillsDir>/<sourceId>.md`. Default: `log-skills` next to
the config file. See [Using it with agents](using-with-agents.md#skills-per-source-guidance).

## Changing the config while the server runs

A running server re-reads the file every couple of seconds and applies changes to **sources** and **skills**
without a restart or dropping connections (this is how [switching sources on and off](sources.md#switching-sources-on-and-off)
works for a central instance). A file that does not parse is logged to the self-log and ignored: the
previous sources stay active. `transport` and `logging` changes need a restart; the self-log notes when
you changed them.
