# Troubleshooting

First stop for anything odd: `mcplogview where` (which config is used), `mcplogview check` (can each source be
read?) and the server's own log:

```bash
tail -f ~/.local/state/mcplogview/mcplogview-server.log
```

(or ask the agent to read the `mcplogview-self` source).

## Running it

**`mcplogview` just prints help and does not start a server.**
That is intended when you run it in a terminal. Agents start it through a pipe and get the server. Start it yourself
with `mcplogview serve`.

**`no config found ...; using built-in defaults`**
No config file was found in the [usual places](configuration.md#which-file-is-used). Only the server's own log is
available. Create one with `mcplogview configure`, or run from the directory that holds `mcplogview.json`.

**The agent does not see my sources, though `mcplogview list` does.**
The agent starts mcplogview in *its* working directory. Run `mcplogview where` there, or give the agent an explicit
config: `claude mcp add mcplogview -- mcplogview serve --config /full/path/mcplogview.json`
(`mcplogview register --config FILE` prints it).

**A source is missing from `list_logs`.**
Check `mcplogview list`: its state may be `off`. `mcplogview enable ID` switches it on.

**`unknown source: X`**
The id is not among the *enabled* sources. Ask for `list_logs` again, and check for typos and disabled sources.

**I changed the config but nothing changed.**
Sources, skills and the token are applied within a few seconds. `transport` and `logging` need a restart. If the file
has a syntax error the server keeps the old configuration and logs `config reload failed` to its self-log.

**`Port already in use: 127.0.0.1:3333`**
Another process owns the port. Stop it or choose another: `mcplogview transport --port 3334`.

**`input ended before the wizard finished`**
`configure` got its answers from a pipe that ran out. It needs a terminal, or one input line per prompt.

## Local files

**`open /path: no such file or directory` / `permission denied`**
The file does not exist or the user running mcplogview cannot read it. If an *agent host* starts mcplogview, that
is the user the host runs as.

**Reading starts over after log rotation.**
Expected: when the file is replaced or truncated, the position resets to the start of the new file.

## SSH

**`Permission denied (publickey)` or a password prompt that never appears.**
mcplogview runs `ssh` with `BatchMode=yes`, so it never prompts. Use a key without a passphrase or load it into
`ssh-agent`, and test with the same arguments:
`ssh -i ~/.ssh/KEY -o BatchMode=yes user@host true`.

**`Host key verification failed`**
You set `strictHostKeyChecking: true` and the host is not in `known_hosts`. Connect once with plain `ssh host` to
add it. (With the default `false` the host key is accepted unverified; see the
[security note](sources.md#ssh-files).)

**`remote log file not found: /path`**
The path does not exist on the remote host or is not readable by that user.

**It works in my terminal but not from the agent.**
The agent host may not have your `ssh-agent` (`SSH_AUTH_SOCK`). Use a key file with `identityFile`.

## Docker

**`docker CLI not found on this machine` (or on the host)**
The `docker` command is not on `PATH` for the process. Hosts started from a desktop launcher or a service often have a
minimal `PATH`: start the host from a terminal, or set `PATH` for the service
(see [HTTP mode](http-mode.md#keeping-it-running)).

**`container not found: NAME`**
No container with that name or id exists on that Docker host. See `docker ps -a`. If you recreated it with a new
name, update the source.

**`Cannot connect to the Docker daemon` / `permission denied while trying to connect`**
Docker is not running, or the user may not use it (on Linux: add the user to the `docker` group).

**Some lines show up twice when following a very busy container.**
A deliberate trade-off to never lose a line: see [Chatty containers](sources.md#docker-containers).

## HTTP mode

**`401 Unauthorized`**
Missing or wrong token. Send `Authorization: Bearer <token>` (the `Bearer` word is required). Show the token with
`mcplogview transport --show-token`. If you set `MCPLOGVIEW_TOKEN` in the server's environment it overrides the
config, so use that value.

**`refusing to listen on 0.0.0.0 without an auth token`**
Intended: a network-facing server needs a token. `mcplogview transport --new-token`.

**The MCP Inspector fails to connect.**
Add the header `Authorization: Bearer <token>` in the Inspector, and make sure you use the URL with the path
(`http://host:3333/mcp`). Details in [HTTP mode](http-mode.md#browser-clients-mcp-inspector).

**A client cannot connect at all.**
The client must support MCP over Streamable HTTP and be pointed at the endpoint URL including its path
(default `/mcp`).

## Docker deployment

Problems specific to `docker compose` (missing token, permissions on Linux, ssh keys, the Docker proxy) are in
[Running in Docker](docker.md#troubleshooting-the-container-setup).

## Still stuck?

Turn on `"logging": { "debug": true }`, restart, reproduce, and read the self-log: it then records the raw MCP
messages (truncated to `logging.maxBodyChars`). The log may contain log content, so share it with care.
