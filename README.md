# debugMcp

A C2-style MCP for **authorized security-test environments** — an SSH alternative for
targets where SSH is hard to run, designed to be driven by Claude Code. The operator
runs a long-lived **hub**; a **C2 agent** on each target calls back over a
mutually-authenticated, encrypted channel; Claude Code spawns a thin **stdio MCP shim**
per session that proxies tool calls to the hub.

> Authorization scope: remote-admin / testing infrastructure for authorized
> security-testing environments only. The transport plane is encrypted and mutually
> authenticated by default; misconfiguration here equals RCE, so defaults are safe.

## Architecture (three planes)

```
Claude Code  --stdio MCP-->  shim  --Unix socket + MAC token-->  HUB  --Noise_NNpsk0 (PSK)-->  agent (target)
                              (MCP)        (control plane / IPC)         (transport plane)        exec / PTY shell / fs
```

- **Hub** (`cmd/hub`): TCP+Noise listener for agents, agent registry, session/occupancy
  table, audit log (append-only + fsync), and a MAC-token IPC server for shims.
- **Agent** (`cmd/agent`): dials the hub, authenticates with the enrolled PSK,
  daemonizes (POSIX setsid), and serves exec / interactive shell / filesystem ops.
- **Shim** (`cmd/shim`): the only MCP-speaking component; Claude Code spawns it per
  session; proxies tool calls to the hub over IPC.

## Quick start

```bash
# 1. hub (generates + persists PSK + IPC token under ~/.debugmcp, prints them)
go run ./cmd/hub -listen 127.0.0.1:7777
#   -> prints: psk=<hex>  ipc=<path>  token=<hex>

# 2. agent on a target (use the printed PSK)
./debugmcp-agent -hub <operator-ip>:7777 -psk <hex>
#   (omit -no-daemon in production so it detaches from the launching shell)

# 3. Claude Code MCP config (stdio)
#    command: debugmcp-shim
#    env:     DBGMCP_HUB_SOCKET=<path>  DBGMCP_HUB_TOKEN=<hex>
```

To bind the agent listener on a non-loopback address (real callbacks), pass
`-allow-inbound`; the hub refuses `0.0.0.0` without it to prevent accidental exposure.

## MCP tools

| Tool | Purpose |
|---|---|
| `list_targets` | targets with live occupancy (`sessions_active`, `concurrency_cap`, `busy`) |
| `select_target` | default target for this Claude session (attribution, not isolation) |
| `exec` | one-shot command via the target login shell (`bash -lc` / `cmd /c` / `pwsh -c`); authoritative exit |
| `shell_open` / `shell_send` / `shell_read` / `shell_close` | independent interactive PTY sessions (state persists); `shell_read.completion` says how trustworthy "done" is |
| `signal` | `interrupt` (Ctrl-C) / `terminate` / `force_kill` / `quit` |
| `file_read` / `file_write` / `file_list` / `file_stat` | size-capped, binary-safe FS ops |
| `list_sessions` / `status` | cross-target session + occupancy views |

Full-duplex guidance: use `exec` for commands that exit (authoritative completion,
bash syntax via login shell); use `shell_*` for true interactivity / long-lived
processes / Ctrl-C / stateful `cd`+env across calls.

## Security model

- **Transport plane**: Noise_NNpsk0 (ChaChaPoly1304 + Curve25519 + BLAKE2b), keyed by a
  32-byte enrolled PSK. Mutual PSK confirm; forward secrecy via ephemeral DH. Wrong PSK
  → handshake fails (test-enforced).
- **Control plane (IPC)**: Unix socket `0600` + MAC token; bad token rejected
  (test-enforced).
- **Safe defaults**: listener binds loopback; `0.0.0.0` requires `--allow-inbound`;
  agent daemonizes; audit log is append-only with per-record fsync.

## Build & test

```bash
go build ./...                       # linux
GOOS=windows GOARCH=amd64 go build ./...   # cross-compile
go test ./...                        # all unit + E2E tests
go test ./internal/wire -fuzz=FuzzReadFrame -fuzztime=30s   # frame-parser fuzz
```

## Status (MVP — Phase 0/1 vertical, Linux-first)

Implemented & verified: wire protocol (+fuzz), Noise transport, hub (registry/mux/
occupancy/IPC/audit), agent (exec/shell/fs/daemonize), MCP shim (15 tools), 3 binaries.
Cross-compile clean (Windows agent is a stub until ConPTY lands).

Deferred (see `.omc/RALPLAN.md`): signed-token enrollment + PSK rotation & revocation,
audit hash-chain, relay/NAT traversal, beaconing mode, Windows ConPTY + detached run,
per-operator ACL, `upload`/`download` chunked transfer.
