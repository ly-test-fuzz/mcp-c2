#!/usr/bin/env bash
# Stop the debugMcp backend (hub + all agents on this host).
# Uses -x (exact process-name match) so it never matches the invoking shell.
set -euo pipefail
pkill -x dbgmcp-hub   2>/dev/null || true
pkill -x dbgmcp-agent 2>/dev/null || true
sleep 0.3
pgrep -x dbgmcp-hub >/dev/null 2>&1 && echo "hub still running" || echo "hub stopped"
pgrep -x dbgmcp-agent >/dev/null 2>&1 && echo "agent still running" || echo "agent stopped"
