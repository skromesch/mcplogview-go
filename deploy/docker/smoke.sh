#!/usr/bin/env bash
# End-to-end smoke test of the Docker deployment (compose.yaml).
#
# Builds the image, starts the stack on a free port with a throw-away config, and
# checks: health, token auth, a file source, a Docker source through the socket
# proxy (and that the proxy refuses to control containers), live config reload,
# and the container hardening. Set SMOKE_SSH=1 to also test an ssh source against
# a throw-away sshd container (needs network access for `apk add`).
#
#   deploy/docker/smoke.sh
#
# It uses its own compose project and containers and cleans up after itself; it
# never touches your running containers or your .env.
set -euo pipefail
cd "$(dirname "$0")/../.."

for c in docker curl python3; do command -v "$c" >/dev/null || { echo "missing: $c" >&2; exit 2; }; done

tmp=$(mktemp -d)
proj="mlvsmoke$$"
demo="mlvsmoke-demo-$$"
sshd="mlvsmoke-sshd-$$"
port=$(python3 -c "import socket;s=socket.socket();s.bind(('127.0.0.1',0));print(s.getsockname()[1])")
token="smoke-token-$(python3 -c "import secrets;print(secrets.token_hex(12))")"

mkdir -p "$tmp/config" "$tmp/logs" "$tmp/ssh/keys"
: > "$tmp/ssh/known_hosts"
cp deploy/docker/config/mcplogview.json "$tmp/config/"
cp deploy/docker/logs/app.log "$tmp/logs/"

export MCPLOGVIEW_TOKEN="$token" MCPLOGVIEW_PORT="$port" MCPLOGVIEW_BIND=127.0.0.1
# Build with the caller's uid: files in $tmp (config dir, ssh key) are then readable and writable by the
# container user, which is also the setup docs/docker.md recommends on Linux.
export MCPLOGVIEW_UID="${MCPLOGVIEW_UID:-$(id -u)}"
export MCPLOGVIEW_CONFIG="$tmp/config" MCPLOGVIEW_LOGS="$tmp/logs" MCPLOGVIEW_SSH_KEYS="$tmp/ssh/keys" MCPLOGVIEW_KNOWN_HOSTS="$tmp/ssh/known_hosts"
compose() { docker compose -p "$proj" --profile docker "$@"; }
svc="${proj}-mcplogview-1"
url="http://127.0.0.1:$port/mcp"

cleanup() {
  status=$?
  if [ $status -ne 0 ]; then echo "--- container log:"; compose logs --no-color --tail 30 mcplogview 2>&1 || true; fi
  compose down -v >/dev/null 2>&1 || true
  docker rm -f "$demo" "$sshd" >/dev/null 2>&1 || true
  rm -rf "$tmp"
  if [ $status -eq 0 ]; then echo "ALL OK"; else echo "FAILED" >&2; fi
  exit $status
}
trap cleanup EXIT

pass() { echo "  ok   $*"; }
fail() { echo "  FAIL $*" >&2; exit 1; }
expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (got '$2', want '$3')"; fi; }

# A minimal MCP client over Streamable HTTP. Reads a python snippet on stdin;
# the snippet can use call(tool, args) -> (is_error, text) and json.
mcp_run() {
  local snippet; snippet=$(cat)
  python3 - "$url" "$token" "$snippet" <<'PY'
import json, sys, urllib.request
URL, TOKEN, SNIPPET = sys.argv[1], sys.argv[2], sys.argv[3]
sid = None
def post(body):
    global sid
    h = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream", "Authorization": "Bearer " + TOKEN}
    if sid: h["Mcp-Session-Id"] = sid
    r = urllib.request.urlopen(urllib.request.Request(URL, json.dumps(body).encode(), h), timeout=20)
    sid = r.headers.get("Mcp-Session-Id") or sid
    for line in r.read().decode().splitlines():
        if line.startswith("data:"): return json.loads(line[5:])
def call(name, args=None):
    m = post({"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": name, "arguments": args or {}}})
    return m["result"].get("isError", False), m["result"]["content"][0]["text"]
post({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "smoke", "version": "1"}}})
exec(SNIPPET)
PY
}

# Retry a snippet until it prints "ok" (the running server picks config changes up within seconds).
eventually_ok() {
  local snippet; snippet=$(cat)
  local out=""
  for _ in $(seq 1 20); do
    out=$(printf '%s' "$snippet" | mcp_run 2>/dev/null) || out=bad
    if [ "$out" = ok ]; then echo ok; return 0; fi
    sleep 1
  done
  echo "$out"
}

echo "== build and start ($proj, port $port)"
compose up -d --build >/dev/null 2>&1 || { compose up -d --build; fail "compose up"; }
for _ in $(seq 1 40); do
  [ "$(docker inspect -f '{{.State.Health.Status}}' "$svc" 2>/dev/null)" = healthy ] && break
  sleep 1
done
expect_eq "container is healthy" "$(docker inspect -f '{{.State.Health.Status}}' "$svc")" healthy

echo "== hardening"
uid=$(docker exec "$svc" id -u | tr -d '\r')
if [ "$uid" != 0 ]; then pass "runs as a non-root user (uid $uid)"; else fail "runs as root"; fi
if docker exec "$svc" sh -c 'touch /x' >/dev/null 2>&1; then fail "root filesystem is writable"; else pass "root filesystem is read-only"; fi

echo "== token auth on the published port"
init='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}'
code() { curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' "$@" -d "$init" "$url"; }
expect_eq "no token -> 401" "$(code)" 401
expect_eq "wrong token -> 401" "$(code -H 'Authorization: Bearer nope')" 401
expect_eq "right token -> 200" "$(code -H "Authorization: Bearer $token")" 200

echo "== file source"
out=$(mcp_run <<'PYSNIP'
err, t = call("read_log_chunk", {"sourceId": "demo", "maxLines": 1})
print("ok" if not err and "server started" in json.loads(t)["lines"][0] else "bad")
PYSNIP
)
expect_eq "demo file readable through MCP" "$out" ok

echo "== docker source through the socket proxy"
docker run -d --name "$demo" alpine:latest sh -c 'i=0; while true; do echo "tick $i"; i=$((i+1)); sleep 0.3; done' >/dev/null
sleep 1.5
docker exec "$svc" mcplogview add docker --id ticker --container "$demo" >/dev/null 2>&1 || fail "add docker source inside the container"
pass "added from inside the container (written to the mounted config)"
out=$(eventually_ok <<'PYSNIP'
err, t = call("read_log_chunk", {"sourceId": "ticker", "maxLines": 2})
print("ok" if not err and "tick" in t else "bad")
PYSNIP
)
expect_eq "running server picked up the new source and read the container" "$out" ok
for cmd in "docker stop $demo" "docker exec $demo true" "docker run --rm alpine true"; do
  if docker exec "$svc" sh -c "$cmd" >/dev/null 2>&1; then fail "proxy allowed: $cmd"; else pass "proxy refuses: $(echo "$cmd" | cut -c1-30)"; fi
done
expect_eq "the container is still running" "$(docker inspect -f '{{.State.Running}}' "$demo")" true

if [ "${SMOKE_SSH:-0}" = 1 ]; then
  echo "== ssh source against a throw-away sshd"
  ssh-keygen -q -t ed25519 -N "" -f "$tmp/ssh/keys/id_ed25519" && chmod 600 "$tmp/ssh/keys/id_ed25519"
  docker run -d --name "$sshd" --network "${proj}_default" -e PUB="$(cat "$tmp/ssh/keys/id_ed25519.pub")" alpine:latest sh -c '
    apk add -q openssh-server >/dev/null 2>&1 && ssh-keygen -A >/dev/null
    adduser -D -s /bin/sh logreader && echo "logreader:$(head -c12 /dev/urandom | base64)" | chpasswd
    mkdir -p /home/logreader/.ssh /var/log/remote && echo "$PUB" > /home/logreader/.ssh/authorized_keys
    chown -R logreader /home/logreader && chmod 700 /home/logreader/.ssh && chmod 600 /home/logreader/.ssh/authorized_keys
    echo "remote line one" > /var/log/remote/app.log
    exec /usr/sbin/sshd -D -e' >/dev/null
  for _ in $(seq 1 40); do docker exec "$sshd" pgrep sshd >/dev/null 2>&1 && break; sleep 1; done
  sleep 1
  # shellcheck disable=SC2088  # the '~' is expanded by mcplogview inside the container
  docker exec "$svc" mcplogview add ssh --id remote --host "$sshd" --user logreader \
      --identity-file '~/.ssh/keys/id_ed25519' --path /var/log/remote/app.log >"$tmp/add.out" 2>&1 || { cat "$tmp/add.out"; fail "add ssh source"; }
  out=$(eventually_ok <<'PYSNIP'
err, t = call("read_log_chunk", {"sourceId": "remote", "maxLines": 1})
print("ok" if not err and "remote line one" in t else "bad")
PYSNIP
)
  expect_eq "ssh source readable through MCP" "$out" ok
  expect_eq "host key recorded in the mounted known_hosts" "$(grep -c "$sshd" "$tmp/ssh/known_hosts" || true)" 1
fi

echo "== secrets"
expect_eq "token never written to the self-log" "$(docker exec "$svc" grep -c "$token" /data/mcplogview/mcplogview-server.log || true)" 0
