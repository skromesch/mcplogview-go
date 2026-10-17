#!/usr/bin/env python3
"""Regenerate the documentation screenshots (docs/screenshots/*.svg).

Every image is the real screen of the real `mcplogview` binary, recorded in a
pty and rendered by termshot.py. Nothing is drawn by hand.

To keep your own data out of the images the run is sandboxed:
  * HOME and the project live in a throw-away directory (shown as /home/dev),
  * `docker` is a tiny fake that serves a made-up "shop" stack,
  * ~/.ssh/config contains made-up hosts,
  * your user name, host name and the sandbox path are rewritten, and the run
    aborts if any of them is still visible in an image.

Usage (from the repo root):
    python3 -m venv /tmp/shots && /tmp/shots/bin/pip install pyte
    /tmp/shots/bin/python docs/screenshots/gen.py            # everything except the Docker pictures
    /tmp/shots/bin/python docs/screenshots/gen.py --docker   # only the Docker pictures (needs Docker; builds and
                                                             # starts the compose stack, then removes it)
"""
import getpass
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from termshot import Term, render_svg  # noqa: E402

REPO = os.path.abspath(os.path.join(HERE, "..", ".."))
OUT = HERE
DOWN, UP, ENTER, SPACE, CTRL_C, BKSP = b"\x1b[B", b"\x1b[A", b"\r", b" ", b"\x03", b"\x7f"

ROOT = os.path.realpath(tempfile.mkdtemp(prefix="mlv-shots-"))
HOME = os.path.join(ROOT, "home")
PROJ = os.path.join(HOME, "work", "shop")
BINDIR = os.path.join(ROOT, "bin")
SHIM = os.path.join(ROOT, "shim")
BIN = os.path.join(BINDIR, "mcplogview")

LEAKS = [getpass.getuser(), socket.gethostname(), socket.gethostname().split(".")[0], "/Users/", ROOT]
SUBS = [(HOME, "/home/dev"), (ROOT, "/home/dev/.tmp"), (socket.gethostname(), "shop-host.example.com"),
        (socket.gethostname().split(".")[0], "shop-host"), (getpass.getuser(), "dev")]


def write(path, text, mode=0o644):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(text)
    os.chmod(path, mode)


def build_fixtures():
    subprocess.run(["go", "build", "-o", BIN, "./cmd/mcplogview"], cwd=REPO, check=True)

    app = []
    for i, (lvl, msg) in enumerate([
        ("INFO", "server started on :8080"), ("INFO", "connected to postgres shop-db:5432"),
        ("INFO", "GET /api/products 200 12ms"), ("INFO", "GET /api/products/42 200 8ms"),
        ("WARN", "slow query (412ms): SELECT * FROM orders WHERE user_id = $1"),
        ("INFO", "POST /api/cart 201 31ms"), ("ERROR", "payment gateway timeout after 5000ms order=10293"),
        ("INFO", "retrying payment order=10293 attempt=2"), ("INFO", "payment ok order=10293"),
        ("INFO", "GET /api/orders 200 19ms")]):
        app.append(f"2026-10-17T08:15:{i:02d}Z {lvl:5} {msg}")
    write(os.path.join(PROJ, "logs", "app.log"), "\n".join(app) + "\n")
    write(os.path.join(PROJ, "logs", "access.log"), "".join(
        f'10.0.0.{i} - - [17/Oct/2026:08:16:{i:02d} +0000] "GET /index.html HTTP/1.1" 200 {1200+i}\n' for i in range(1, 9)))
    write(os.path.join(PROJ, "logs", "error.log"), "2026/10/17 08:17:01 [error] upstream timed out\n")
    write(os.path.join(PROJ, "logs", "archive", "app-2026-10-16.log"), "2026-10-16T23:59:59Z INFO rotated\n")

    # made-up ssh hosts (only the host *names* are listed by the wizard)
    write(os.path.join(HOME, ".ssh", "config"),
          "Host prod-api\n  HostName 10.0.0.12\n  User ubuntu\n  IdentityFile ~/.ssh/id_ed25519\n\n"
          "Host staging-api\n  HostName staging.example.com\n  User deploy\n  IdentityFile ~/.ssh/id_ed25519\n\n"
          "Host *\n  ServerAliveInterval 30\n")
    write(os.path.join(HOME, ".ssh", "id_ed25519"), "", 0o600)

    # fake docker serving a made-up shop stack
    logs = "".join(f"2026-10-17T08:2{i // 10}:{(i * 7) % 60:02d}.{i:03d}000000Z {m}\n" for i, m in enumerate([
        "INFO  listening on :8080", "INFO  GET /api/products 200 11ms", "INFO  GET /api/cart 200 4ms",
        "WARN  cache miss for product 42", "INFO  POST /api/checkout 200 88ms"]))
    write(os.path.join(SHIM, "shop-api.log"), logs)
    write(os.path.join(SHIM, "docker"), r'''#!/bin/sh
# Fake docker CLI for the documentation screenshots.
here=$(dirname "$0")
for last; do :; done
case "$1" in
ps)
  printf 'shop-api\tghcr.io/acme/shop-api:1.4.2\tUp 3 hours\n'
  printf 'shop-db\tpostgres:16\tUp 3 hours (healthy)\n'
  printf 'shop-nginx\tnginx:1.27\tUp 3 hours\n'
  printf 'shop-worker\tghcr.io/acme/shop-worker:1.4.2\tUp 3 hours\n'
  printf 'old-migration\talpine:3.20\tExited (0) 2 days ago\n' ;;
container)
  case "$last" in
    shop-api|shop-db|shop-nginx|shop-worker|old-migration)
      printf '%s' "$last" | od -An -tx1 | tr -d ' \n' | cut -c1-12; echo "a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5" ;;
    *) echo; echo "Error response from daemon: No such container: $last" >&2; exit 1 ;;
  esac ;;
logs) [ -f "$here/$last.log" ] && cat "$here/$last.log" || true ;;
*) echo "fake docker: unsupported: $*" >&2; exit 1 ;;
esac
''', 0o755)


def env(**extra):
    e = {"PATH": f"{SHIM}:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin", "HOME": HOME, "TERM": "xterm-256color",
         "LANG": "en_US.UTF-8", "PWD": PROJ, "USER": "dev", "MCPLOGVIEW_BROWSE_DIR": os.path.join(PROJ, "logs")}
    e.update(extra)
    return e


def shot(term, name, title, marker=None, until=None, **kw):
    """Render the screen. `marker` starts the picture at the last row beginning
    with it; `until` ends it right before the last row beginning with that."""
    top, bottom = 0, None
    rows = term.text().split("\n")
    if marker:
        idx = [i for i, l in enumerate(rows) if l.startswith(marker)]
        top = idx[-1] if idx else 0
    if until:
        idx = [i for i, l in enumerate(rows) if l.startswith(until) and i > top]
        bottom = idx[-1] if idx else None
    text = render_svg(term, title, os.path.join(OUT, name + ".svg"), subs=SUBS, top=top, bottom=bottom, **kw)
    for leak in LEAKS:
        if leak and len(leak) > 3 and leak in text:
            raise SystemExit(f"LEAK: {leak!r} is visible in {name}.svg")
    print("  wrote", name + ".svg")


def cli(args, name, title=None, rows=60, cols=400, wait=15, stdin_closed=False):
    t = Term([BIN] + args, PROJ, env(), rows=rows, cols=cols)
    t.prompt("mcplogview " + " ".join(args))
    t.pump(0.5)
    t.wait_exit(wait)
    shot(t, name, title or ("mcplogview " + " ".join(args)))
    t.kill()
    return t


def quiet(args):
    r = subprocess.run([BIN] + args, cwd=PROJ, env=env(), capture_output=True, text=True)
    if r.returncode != 0:
        raise SystemExit(f"setup command failed: {args}\n{r.stdout}{r.stderr}")


def wizard_flow():
    t = Term([BIN, "configure"], PROJ, env(), rows=70, cols=400)
    t.pump(1.8)
    shot(t, "02-wizard-first-run", "mcplogview configure")
    t.send(ENTER, wait=1.5)                                 # project scope
    shot(t, "03-wizard-menu-empty", "mcplogview configure", marker="Config:")
    t.send(ENTER, wait=1.2)                                 # Add a source
    shot(t, "04-wizard-add-kind", "mcplogview configure", marker="Config:")
    t.send(ENTER, wait=2.0)                                 # local file -> browser
    shot(t, "05-wizard-browse-files", "mcplogview configure", marker="Config:")
    t.type("app", wait=0.9)                                 # type-to-filter
    shot(t, "06-wizard-browse-filter", "mcplogview configure", marker="Config:")
    t.send(ENTER, wait=1.5)                                 # pick app.log
    shot(t, "07-wizard-details", "mcplogview configure", marker="Config:")
    t.send(ENTER, wait=0.8)                                 # id: default
    t.send(BKSP, BKSP, BKSP, wait=0.3)                      # the label is pre-filled with the id: clear it
    t.type("Shop app log", wait=0.4); t.send(ENTER, wait=0.8)       # label
    t.type("Storefront backend (Go)", wait=0.4); t.send(ENTER, wait=0.8)  # description
    t.type("Start with ERROR lines", wait=0.4); t.send(ENTER, wait=3.0)   # hint -> check + save
    shot(t, "08-wizard-saved", "mcplogview configure", marker="Config:")

    # a Docker container from the list
    t.send(ENTER, wait=1.2)                                 # Add a source
    t.send(DOWN, DOWN, ENTER, wait=2.5)                     # docker (this machine)
    shot(t, "09-wizard-docker-picker", "mcplogview configure", marker="Config:")
    t.type("shop-a", wait=0.9)
    t.send(ENTER, wait=1.5)                                 # pick shop-api
    t.send(ENTER, wait=0.8)                                 # id
    t.send(ENTER, wait=0.8)                                 # label
    t.send(ENTER, wait=0.8)                                 # description
    t.send(ENTER, wait=3.0)                                 # hint -> check + save

    # ssh host picker, then back out with Ctrl+C
    t.send(ENTER, wait=1.2)
    t.send(DOWN, ENTER, wait=1.8)                           # ssh
    shot(t, "10-wizard-ssh-host", "mcplogview configure", marker="Config:")
    t.send(CTRL_C, wait=1.2)
    shot(t, "11-wizard-back-with-ctrl-c", "mcplogview configure", marker="Config:")

    # switch sources on/off
    t.send(DOWN, ENTER, wait=1.5)
    shot(t, "12-wizard-toggle", "mcplogview configure", marker="Config:")
    t.send(DOWN, SPACE, wait=0.6)                           # second source off
    shot(t, "13-wizard-toggle-off", "mcplogview configure", marker="Config:")
    t.send(ENTER, wait=1.8)                                 # confirm: saved, back at the menu
    shot(t, "14-wizard-toggle-saved", "mcplogview configure", marker="Config:")
    t.send(DOWN, ENTER, wait=1.5)                           # reopen the toggle screen
    t.send(DOWN, SPACE, wait=0.5)                           # switch shop-api back on
    t.send(ENTER, wait=1.8)                                 # confirm, back at the menu
    return t


def docker_shots():
    """Pictures of the docker compose deployment, taken from the real stack."""
    import time
    comp_env = dict(os.environ)
    cfg, logs, ssh = (os.path.join(ROOT, d) for d in ("dk-config", "dk-logs", "dk-ssh"))
    for d in (cfg, logs, os.path.join(ssh, "keys")):
        os.makedirs(d)
    open(os.path.join(ssh, "known_hosts"), "w").close()
    shutil.copy(os.path.join(REPO, "deploy/docker/config/mcplogview.json"), cfg)
    shutil.copy(os.path.join(REPO, "deploy/docker/logs/app.log"), logs)
    comp_env.update(MCPLOGVIEW_TOKEN="docs-example-token-0123456789", MCPLOGVIEW_PORT="3334", MCPLOGVIEW_BIND="127.0.0.1",
                    MCPLOGVIEW_CONFIG=cfg, MCPLOGVIEW_LOGS=logs, MCPLOGVIEW_SSH_KEYS=os.path.join(ssh, "keys"),
                    MCPLOGVIEW_KNOWN_HOSTS=os.path.join(ssh, "known_hosts"))
    base = ["docker", "compose", "--profile", "docker"]

    def run(args, **kw):
        return subprocess.run(args, cwd=REPO, env=comp_env, capture_output=True, text=True, **kw)

    if "mcplogview" in run(["docker", "compose", "ls", "--format", "{{.Name}}"]).stdout.split():
        raise SystemExit("a compose project called 'mcplogview' is already running; stop it first")
    if run(["docker", "ps", "-aq", "--filter", "name=^shop-api$"]).stdout.strip():
        raise SystemExit("a container called 'shop-api' already exists; the pictures use that name")

    # the host port is rewritten to the documented default so the text and the pictures agree
    subs = SUBS + [("127.0.0.1:3334->3333", "127.0.0.1:3333->3333")]
    try:
        r = run(base + ["up", "-d", "--build"])
        if r.returncode:
            raise SystemExit("compose up failed:\n" + r.stdout + r.stderr)
        for _ in range(40):
            if run(["docker", "inspect", "-f", "{{.State.Health.Status}}", "mcplogview-mcplogview-1"]).stdout.strip() == "healthy":
                break
            time.sleep(1)
        run(["docker", "run", "-d", "--name", "shop-api", "alpine:latest", "sh", "-c",
             'i=0; while true; do echo "INFO GET /api/products 200 $i ms"; i=$((i+1)); sleep 0.5; done'])
        time.sleep(1.5)
        r = run(base + ["exec", "mcplogview", "mcplogview", "add", "docker", "--id", "shop-api", "--container", "shop-api",
                        "--label", "Shop API container"])
        if r.returncode:
            raise SystemExit("adding the docker source failed:\n" + r.stdout + r.stderr)

        def dshot(argv, name, title, prompt, marker=None, keys=None, **kw):
            t = Term(argv, REPO, comp_env, rows=40, cols=400)
            t.prompt(prompt)
            t.pump(3.0 if keys else 1.5)
            if not keys:
                t.wait_exit(15)
            text = render_svg(t, title, os.path.join(OUT, name + ".svg"), subs=subs,
                              top=max([i for i, l in enumerate(t.text().split("\n")) if marker and l.startswith(marker)] or [0]), **kw)
            for leak in LEAKS:
                if leak and len(leak) > 3 and leak in text:
                    line = [l for l in text.split("\n") if leak in l][0]
                    raise SystemExit(f"LEAK: {leak!r} is visible in {name}.svg, in the line: {line[:150]!r}")
            print("  wrote", name + ".svg")
            if keys:
                t.send(CTRL_C, wait=0.8)
            t.kill()

        dshot(base + ["ps"], "30-docker-compose-ps", "docker compose ps", "docker compose ps", max_cols=175)
        dshot(base + ["exec", "mcplogview", "mcplogview", "list"], "31-docker-exec-list",
              "docker compose exec mcplogview mcplogview list", "docker compose exec mcplogview mcplogview list")
        dshot(base + ["exec", "mcplogview", "mcplogview", "configure"], "32-docker-exec-configure",
              "docker compose exec mcplogview mcplogview configure", "docker compose exec mcplogview mcplogview configure",
              marker="Config:", keys=True)
    finally:
        run(base + ["down", "-v"])
        run(["docker", "rm", "-f", "shop-api"])
        run(["docker", "rmi", "-f", "mcplogview:dev"])


def main():
    build_fixtures()
    print("sandbox:", ROOT)
    try:
        cli([], "01-help", "mcplogview", rows=70)
        t = wizard_flow()
        # server mode (menu: Add, Switch, Remove, Test, Server mode, ...)
        t.send(DOWN, DOWN, DOWN, DOWN, ENTER, wait=1.5)
        shot(t, "15-wizard-server-mode", "mcplogview configure", marker="Current:")
        t.send(DOWN, ENTER, wait=1.5)                        # HTTP
        shot(t, "16-wizard-http-listen", "mcplogview configure", marker="Current:")
        t.send(DOWN, ENTER, wait=1.5)                        # all interfaces
        t.send(ENTER, wait=0.8)                              # port default
        t.send(ENTER, wait=1.5)                              # path default
        shot(t, "17-wizard-http-token", "mcplogview configure", marker="Current:")
        t.send(ENTER, wait=2.5)                              # generate token
        shot(t, "18-wizard-http-saved", "mcplogview configure", marker="✓ saved to", until="Config:")
        t.send(CTRL_C, wait=1.0)
        t.kill()

        quiet(["transport", "--port", "38333"])              # keep the demo port out of the way
        quiet(["add", "local", "--id", "legacy", "--path", "/var/log/legacy/old.log", "--no-check"])
        quiet(["disable", "legacy"])

        cli(["list"], "20-cli-list")
        quiet(["enable", "legacy"])
        cli(["check"], "21-cli-check")
        quiet(["disable", "legacy"])
        cli(["add", "docker", "--id", "shop-worker", "--container", "shop-worker", "--label", "Shop worker"], "22-cli-add-docker")
        cli(["transport"], "23-cli-transport")
        cli(["register"], "24-cli-register", rows=40)

        s = Term([BIN, "serve"], PROJ, env(), rows=12, cols=400)
        s.prompt("mcplogview serve")
        s.pump(1.8)
        shot(s, "25-cli-serve", "mcplogview serve")
        s.send(CTRL_C, wait=0.8)
        s.kill()
    finally:
        shutil.rmtree(ROOT, ignore_errors=True)


if __name__ == "__main__":
    if "--docker" in sys.argv:
        os.makedirs(ROOT, exist_ok=True)
        try:
            subprocess.run(["go", "build", "-o", BIN, "./cmd/mcplogview"], cwd=REPO, check=True)
            docker_shots()
        finally:
            shutil.rmtree(ROOT, ignore_errors=True)
    else:
        main()
