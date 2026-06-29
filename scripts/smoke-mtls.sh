#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

PORT=${PORT:-18443}
CERT_DIR=${CERT_DIR:-certs}
SERVER_LOG=${SERVER_LOG:-/tmp/mcpc2-server-smoke.log}
CLIENT_LOG=${CLIENT_LOG:-/tmp/mcpc2-client-smoke.log}

if [[ ! -f "$CERT_DIR/ca.crt" ]]; then
  SERVER_NAME=localhost bash scripts/generate-certs.sh "$CERT_DIR" >/dev/null
fi

go build -o /tmp/mcpc2-server ./server/cmd/mcpc2-server
go build -o /tmp/mcpc2-client ./client/cmd/mcpc2-client

rm -f "$SERVER_LOG" "$CLIENT_LOG"
/tmp/mcpc2-server -addr "127.0.0.1:$PORT" -ca "$CERT_DIR/ca.crt" -cert "$CERT_DIR/server.crt" -key "$CERT_DIR/server.key" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
cleanup() {
  kill "$SERVER_PID" 2>/dev/null || true
  if [[ -n "${CLIENT_PID:-}" ]]; then kill "$CLIENT_PID" 2>/dev/null || true; fi
}
trap cleanup EXIT
sleep 1

/tmp/mcpc2-client -server "https://127.0.0.1:$PORT/c2" -ca "$CERT_DIR/ca.crt" -cert "$CERT_DIR/client.crt" -key "$CERT_DIR/client.key" -server-name localhost -client-id smoke-client >"$CLIENT_LOG" 2>&1 &
CLIENT_PID=$!
sleep 3

clients=$(curl -sk --cert "$CERT_DIR/client.crt" --key "$CERT_DIR/client.key" "https://127.0.0.1:$PORT/clients")
CLIENTS_JSON="$clients" python3 - <<'PY'
import json, os
clients = json.loads(os.environ["CLIENTS_JSON"])
assert len(clients) == 1, clients
c = clients[0]
assert c["client_id"] == "smoke-client", c
assert c["os"], c
assert c["arch"], c
assert c["cert_fingerprint"], c
print("smoke OK", c["client_id"], c["os"], c["arch"])
PY
