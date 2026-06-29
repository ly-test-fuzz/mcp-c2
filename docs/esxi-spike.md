# ESXi Spike Report

Date: 2026-06-27
Target: ESXi 8.0.3 (192.168.215.159, root access)
Status: **best-effort — binary execution restricted**

## Test

1. Static Go binary compiled: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build`
2. Binary deployed to `/tmp/mcpc2-client` on ESXi 8.0.3
3. Result: `sh: /tmp/mcpc2-client: Operation not permitted`

ESXi 8.0.3 VMkernel prohibits arbitrary binary execution in the ESXi Shell. The static binary itself is valid (same binary runs on Ubuntu 215.155), but the VMkernel security policy blocks it.

## Fallback

- ESXi targets should use **SSH fallback** (standard practice)
- Document in README: "ESXi best-effort; if direct client execution is blocked, use existing SSH access"
- The C2 host may still run on a separate Linux jumpbox within the same network to proxy commands to ESXi via SSH

## Static Binary Verification

- `CGO_ENABLED=0` static build confirmed (ldd says "not a dynamic executable")
- Cross-compiles to linux/amd64, linux/arm64, windows/amd64
