# mcplogview documentation

mcplogview is an [MCP](https://modelcontextprotocol.io) server that lets AI agents read your logs: local files,
files on remote hosts over SSH, and Docker container logs. It is one static binary.

| Guide | Read it when you want to... |
|-------|-----------------------------|
| [Getting started](getting-started.md) | install it and give an agent access to your first log in ~5 minutes |
| [Interactive setup](interactive-setup.md) | walk through `mcplogview configure` screen by screen |
| [Configuration reference](configuration.md) | edit the config file by hand; every key, default and path rule |
| [Log sources](sources.md) | set up local files, SSH hosts and Docker containers; switch sources on and off |
| [Command reference](cli.md) | look up a command, flag, environment variable or exit code |
| [HTTP mode](http-mode.md) | run **one central instance** that many agents connect to, with a token |
| [Running in Docker](docker.md) | start that instance with `docker compose`, hardened, with a Docker-log proxy |
| [Using it with agents](using-with-agents.md) | understand the tools, cursors and per-source "skills" agents get |
| [Lua plugins](plugins.md) | extend it with sandboxed Lua plugins: custom source trees, tools, hooks |
| [Troubleshooting](troubleshooting.md) | fix "no config found", SSH/Docker errors, 401s, Inspector problems |

The screenshots are generated from the real program: see [screenshots/](screenshots/).
