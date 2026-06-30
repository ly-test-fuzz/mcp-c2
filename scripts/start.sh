#!/usr/bin/env bash
# Start the persistent debugMcp backend: hub (detached via setsid+nohup so it
# survives terminal/session exits) + one local agent (self-daemonizing).
# Reuses an existing PSK/token under $STATE if present (so .mcp.json stays valid).
set -euo pipefail

BIN="${BIN:-$HOME/bin}"
STATE="${STATE:-$HOME/.debugmcp}"
LISTEN="${LISTEN:-127.0.0.1:7777}"
mkdir -p "$STATE"

if pgrep -x dbgmcp-hub >/dev/null 2>&1; then
  echo "hub already running (pid $(pgrep -x dbgmcp-hub | tr '\n' ' '))"
  exit 0
fi

# hub: detached into its own session so it outlives this shell / Claude Code.
setsid nohup "$BIN/dbgmcp-hub" -state "$STATE" -listen "$LISTEN" \
  >"$STATE/hub.log" 2>&1 </dev/null &
disown || true

# wait for the IPC socket + psk to appear
for _ in $(seq 1 50); do
  [ -S "$STATE/hub.sock" ] && [ -s "$STATE/psk.hex" ] && break
  sleep 0.1
done

PSK=$(cat "$STATE/psk.hex")
TOKEN=$(cat "$STATE/ipc.token")

# agent: self-daemonizes (POSIX setsid); launch detached.
setsid nohup "$BIN/dbgmcp-agent" -hub "$LISTEN" -psk "$PSK" \
  >"$STATE/agent.log" 2>&1 </dev/null &
disown || true

sleep 0.6
echo "hub listen=$LISTEN  state=$STATE"
echo "psk=$PSK"
echo "ipc=$STATE/hub.sock  token=$TOKEN"
echo "Claude Code .mcp.json: command=$BIN/dbgmcp-shim env={DBGMCP_HUB_SOCKET=$STATE/hub.sock DBGMCP_HUB_TOKEN=$TOKEN}"
