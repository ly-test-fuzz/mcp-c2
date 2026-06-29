# Contributing to MCP-C2

## Development Setup

```bash
# Prerequisites
- Go 1.21+
- OpenSSL (for cert generation)
- GNU Make (optional)

# Clone and build
git clone https://github.com/debugmcp/mcp-c2.git
cd mcp-c2
make build

# Generate test certificates
bash scripts/generate-certs.sh certs
```

## Architecture

```
cmd/
├── mcpc2-hub     — Daemon: C2 WebSocket hub + local HTTP API
├── mcpc2-server  — MCP stdio proxy, connects to hub
└── mcpc2-client  — Agent deployed on target machines

internal/
├── hubapi/       — HTTP client + REST API handlers
├── mcpserver/    — MCP server (tools, system prompt)
├── remote/       — Session/file manager (server-side)
├── transport/    — WebSocket hub + client dialer
├── session/      — PTY/pipe session (per-platform)
├── proto/        — Frame types and wire protocol
├── outputbuf/    — Ring buffer with cursor semantics
├── mtls/         — mTLS config and allowlist
└── embedded/     — Embedded certificates (client binary)
```

## Build

```bash
make build     # all three binaries
make test      # run tests
make vet       # static analysis
```

Cross-compilation uses `CGO_ENABLED=0` for fully static binaries.

## Testing

```bash
# Unit tests
go test ./...

# Integration smoke test (requires local certs)
make smoke
```

## Code Style

- Standard Go formatting (`gofmt` / `goimports`)
- Descriptive variable names (no single-letter except loop indices)
- Error messages are lowercase, no trailing punctuation
- Comments in English, commit messages in English

## Pull Requests

1. Fork and branch from `main`
2. Add tests for new functionality
3. Run `make test vet` before submitting
4. Keep PRs focused — one feature/fix per PR
5. Update docs if you change public API or architecture

## Commit Messages

```
pkg: short description (max 72 chars)

Longer explanation if needed. What and why, not how.
```

## Security

- **DO NOT** commit certificates or private keys
- Report security issues privately via GitHub Security Advisories
- This tool is for authorized use only — see README disclaimer

## License

MIT — see LICENSE file.
