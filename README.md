# MCP-C2 — AI 驱动的远程命令与文件管理通道

[![Go Version](https://img.shields.io/badge/Go-1.21%2B-blue)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-linux%20%7C%20windows-lightgrey)]()
[![MCP](https://img.shields.io/badge/MCP-stdio%201.0-orange)](https://modelcontextprotocol.io/)

**MCP-C2** 是一个专为 AI agent（Claude Code 等）设计的远程控制通道，用于在目标机器上执行命令和传输文件。用 MCP 协议替代 SSH，采用 C2 回连架构：目标主机上的轻量 agent 反向连接到你本地的 hub，AI 通过 MCP stdio 与 hub 交互。

**为什么不用 SSH？** SSH 在 AI 驱动运维场景存在多个痛点：嵌套 shell 的命令转义问题、离线/Windows 环境配置 SSH server 的复杂度、AI 容易把 SSH 当成漏洞利用渠道跑偏。MCP-C2 提供了一套专为 AI 设计的、可审计的替代方案。

---

## 架构

```
┌──────────────────────────────────────────────────┐
│  你的机器（运行 Claude Code）                      │
│                                                  │
│  Claude Code ──(MCP stdio)──▶ mcpc2-server      │
│                                    │             │
│                              (本地 HTTP)          │
│                                    │             │
│  mcpc2-hub ◀───────────────────────┘             │
│  (守护进程) ◀──(wss:// :8443, mTLS)──┐           │
│  127.0.0.1:9000                      │           │
└──────────────────────────────────────┼───────────┘
                                       │
                         ┌─────────────┼─────────────┐
                         │             │             │
                   ┌─────▼────┐  ┌────▼─────┐  ┌────▼─────┐
                   │  Linux   │  │ Windows  │  │  ESXi    │
                   │  Client  │  │  Client  │  │  Client  │
                   │  (PTY)   │  │ (ConPTY) │  │  (pipe)  │
                   └──────────┘  └──────────┘  └──────────┘
```

**三个组件：**

| 二进制 | 角色 | 运行位置 |
|--------|------|----------|
| `mcpc2-hub` | C2 WebSocket 中心 + 本地 HTTP API | 你的机器（守护进程） |
| `mcpc2-server` | MCP stdio 代理 → hub | 你的机器（每个 Claude 实例一个） |
| `mcpc2-client` | 反向连接 agent | 目标机器 |

`mcpc2-hub` 作为守护进程常驻运行，保持所有远程客户端连接。`mcpc2-server` 是薄代理——每个 Claude Code 实例启动一个，共享同一个 hub 和 client 池。

---

## 快速开始

### 1. 编译

```bash
make build
# → 自动生成证书 + 编译三个二进制
# → bin/mcpc2-hub, bin/mcpc2-server, bin/mcpc2-client
```

证书在 `make build` 时自动生成（`certs/` 目录），并嵌入 client 二进制。默认 SAN 为 `127.0.0.1`；生产环境指定：`SERVER_IP=192.168.1.100 make build`。

### 2. 启动 hub 守护进程

```bash
# 零参数启动——自动使用 make build 生成的证书
./bin/mcpc2-hub

# 或指定 IP 让 hub 重新生成证书并启动
./bin/mcpc2-hub -server-ip 192.168.1.100

# 显式指定证书路径（有则用，无则自动生成）
./bin/mcpc2-hub -certs-dir ~/.mcpc2/certs -server-ip 192.168.1.100
```

Hub 启动时自动检测证书：如果指定路径的证书文件存在就用，不存在就自动生成。

### 3. 注册到 Claude Code

```bash
# 在项目根目录创建 .mcp.json（或放到 ~/.claude/.mcp.json 全局生效）
cat > .mcp.json << 'EOF'
{
  "mcpServers": {
    "mcp-c2": {
      "command": "/绝对路径/bin/mcpc2-server",
      "args": ["-hub", "127.0.0.1:9000"]
    }
  }
}
EOF
```

如果 hub 没启动，`mcpc2-server` 会立即退出并报清晰错误。

### 4. 部署 client 到目标机器

```bash
# 拷贝 client 到目标机器
scp bin/mcpc2-client target-machine:/tmp/

# 在目标机器上运行 —— 回连你的 hub
ssh target-machine '/tmp/mcpc2-client -server 192.168.1.100:8443'
```

### 5. 在 Claude Code 中使用

打开 Claude Code，MCP-C2 工具即可使用：

```
> list_clients                          # 查看在线目标
> start_session client_id="..." shell="/bin/bash"  # 打开 shell
> run_command ...                       # 发送命令
> read_output ...                       # 读取结果
> close_session ...                     # 清理
```

---

## 工具列表（AI 可见）

| 工具 | 说明 |
|------|------|
| `list_clients` | 列出已连接的目标机器（OS、主机名、PTY 能力） |
| `list_sessions` | 列出某个 client 上的活跃 shell 会话 |
| `start_session` | 打开交互式 shell（`bash`、`sh`、`powershell`、`cmd`） |
| `run_command` | 发送命令到会话（立即返回，不等待执行完成） |
| `read_output` | 按游标增量读取输出（支持长轮询 `block_ms`） |
| `send_input` | 向交互式会话发送 stdin 输入 |
| `interrupt_session` | 发送 Ctrl-C / SIGINT |
| `close_session` | 关闭会话并终止进程树 |
| `is_alive` | 快速检查会话是否存活 |
| `upload_file` | 上传文件到目标机器（base64 编码） |
| `download_file` | 从目标机器下载文件（内联返回） |
| `list_files` | 列出目标机器目录 |
| `health` | 服务器 + hub 健康状态 |

---

## 平台支持

| 平台 | Shell | PTY | 备注 |
|------|-------|-----|------|
| Ubuntu 24.04+ | bash/sh | ✅ 全双工 PTY | Setsid 实现，支持颜色和交互 |
| CentOS 7+ | bash/sh | ✅ 全双工 PTY | 与 Ubuntu 相同 |
| Windows 10 1809+ | powershell/cmd | ✅ ConPTY | 需要 Windows Terminal 支持 |
| Windows Server 2016 | powershell/cmd | ⚠️ 管道模式 | 命令正常执行，无颜色/交互 |
| ESXi 7/8 | sh | ⚠️ 管道模式 | UserCartel 可能阻止；VIB 打包计划中 |
| macOS (Darwin) | zsh/bash | ✅ 全双工 PTY | 开箱即用 |

**自动降级**：如果 PTY 因安全策略/seccomp 等原因失败，会话自动降级为管道模式。命令仍然正常执行，只是交互特性（Tab 补全、颜色）不可用。

---

## 安全

- **mTLS**：所有 C2 流量使用双向 TLS 1.3 认证
- **证书白名单**：SHA-256 指纹白名单，SIGHUP 热加载
- **审计日志**：每条命令、文件操作、会话事件都记录 client/session/时间戳（`--audit-log`）
- **本地 API**：Hub REST API 仅绑定 `127.0.0.1`，无网络暴露
- **无持久化**：Client agent 无磁盘痕迹，仅二进制本身
- **无横向移动**：无内置隧道、代理、提权功能

---

## 交叉编译

```bash
# Linux (amd64, arm64)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mcpc2-client-linux ./client/cmd/mcpc2-client
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o mcpc2-client-arm64 ./client/cmd/mcpc2-client

# Windows (amd64)
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o mcpc2-client.exe ./client/cmd/mcpc2-client

# macOS (amd64, arm64)
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o mcpc2-client-darwin ./client/cmd/mcpc2-client
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o mcpc2-client-darwin-arm64 ./client/cmd/mcpc2-client
```

所有二进制完全静态编译（`CGO_ENABLED=0`），不依赖 libc。

---

## 配置参数

### Hub 参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-addr` | `:8443` | C2 WebSocket 监听地址（mTLS） |
| `-api-addr` | `127.0.0.1:9000` | 本地 HTTP API 监听地址 |
| `-certs-dir` | `certs` | 证书目录（不存在/缺少则自动生成） |
| `-server-ip` | *(自动检测)* | 服务器 IP/主机名（写入证书 SAN） |
| `-ca` | *(certs-dir/ca.crt)* | CA 证书路径（显式指定则跳过自动生成） |
| `-cert` | *(certs-dir/server.crt)* | 服务器证书路径 |
| `-key` | *(certs-dir/server.key)* | 服务器私钥路径 |
| `-allowed-clients` | *(无)* | 指纹白名单文件（每行一个） |
| `-audit-log` | *(stderr)* | 审计日志文件路径 |

### Server 参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-hub` | `127.0.0.1:9000` | Hub HTTP API 地址 |

### Client 参数

| 参数 | 必填 | 说明 |
|------|------|------|
| `-server` | 是 | Hub 地址（如 `192.168.1.100:8443`） |

---

## 常见问题

**问：为什么要把 hub 和 server 分开？**
hub 是常驻守护进程，保持 client 连接存活。server 是薄 MCP stdio 代理——Claude Code 随开随关，但 hub 和 client 始终保持连接。

**问：多个 Claude Code 实例能共享同一个 hub 吗？**
能。启动一次 `mcpc2-hub`，然后运行多个 `mcpc2-server` 实例，共享同一个 client 池。

**问：Claude Code 退出后 client 会断连吗？**
不会。MCP server 进程退出（stdio 关闭），但 hub 和所有 client 连接保持。重新打开 Claude Code 即可恢复。

**问：可以把 hub API 暴露到网络吗？**
不能。API 无认证（仅用于本地回环）。确保 `-api-addr` 在 `127.0.0.1`。

**问：client 二进制有 8MB，能缩小吗？**
用 `go build -ldflags="-s -w"` 去除调试信息（约 5-6MB）。UPX 可进一步压缩（约 2-3MB）。

**问：Windows PTY 不工作？**
ConPTY 需要 Windows 10 1809+ 和 Windows Terminal。旧版 Windows 自动降级为管道模式。

**问：ESXi 提示 "operation not permitted"？**
VMware 的 UserCartel 安全模块阻止非 VIB 二进制。解决方案：VIB 打包部署，或使用 SSH 备用通道。

---

## 路线图

- [ ] Streamable HTTP / SSE transport（多 AI 宿主支持）
- [ ] Web 审计面板
- [ ] 操作者认证 + 会话归属
- [ ] ESXi VIB 打包
- [ ] 独立 TCP 文件流（大文件传输）
- [ ] MCP Tasks 原生集成
- [ ] 敏感信息自动脱敏

---

## 许可证

MIT — 详见 [LICENSE](LICENSE)。

**本工具仅供授权使用。** 你有责任确保部署行为符合适用法律并获得适当授权。
