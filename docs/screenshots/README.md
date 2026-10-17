# Screenshots

The SVGs in this directory are used by the documentation. They are **generated from the real program**, not drawn:
`gen.py` runs the real `mcplogview` binary in a pseudo-terminal, feeds its output to a terminal emulator
([pyte](https://github.com/selectel/pyte)) and renders the screen as SVG (`termshot.py`).

To keep personal data out of them, the run happens in a throw-away sandbox: its own `HOME` and project (shown as
`/home/dev`), a tiny fake `docker` serving a made-up "shop" stack, made-up `~/.ssh/config` hosts, and your user
name, host name and the sandbox path are rewritten. The generator **aborts if any of them is still visible**.
The token shown in a few images is generated fresh in that sandbox and is worthless.

Regenerate after the interface changes (from the repository root):

```bash
python3 -m venv /tmp/shots && /tmp/shots/bin/pip install pyte
/tmp/shots/bin/python docs/screenshots/gen.py
```

The Docker pictures (`30-` to `32-`) come from the real compose stack and are generated separately:

```bash
/tmp/shots/bin/python docs/screenshots/gen.py --docker
```

It builds and starts the stack under the project name `mcplogview` and a container called `shop-api`, then removes both. It refuses to
run if they already exist. The host port in `docker compose ps` is rewritten to the documented `3333`.

Requires Go (to build the binary), Python 3 and `pyte`. Review the result: a changed prompt can shift the
scripted key presses (the script prints which images it wrote and fails loudly on errors, but it cannot judge
whether a picture still shows what its caption says).
