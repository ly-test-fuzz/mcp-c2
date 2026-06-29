# MCP-C2 架构设计

本文档描述 MCP-C2 的内部设计、通信协议和核心抽象。适合贡献者、扩展开发者或深入排查问题时阅读。

---

## 系统总览

```
┌─────────────── Claude Code ───────────────┐
│  stdin/stdout (JSON-RPC / MCP)            │
└───────────────┬───────────────────────────┘
                │
┌───────────────▼───────────────────────────┐
│  mcpc2-server                             │
│  - MCP server（13 个工具）                 │
│  - 系统提示（3.8KB Instructions）          │
│  - hubapi.Client（HTTP → hub）             │
└───────────────┬───────────────────────────┘
                │  HTTP (127.0.0.1:9000)
┌───────────────▼───────────────────────────┐
│  mcpc2-hub（守护进程）                     │
│  ┌─────────────────────────────────────┐  │
│  │  HTTP API 处理器                    │  │
│  │  GET/POST /api/v1/...               │  │
│  └───────────┬─────────────────────────┘  │
│  ┌───────────▼─────────────────────────┐  │
│  │  remote.Manager                     │  │
│  │  - 会话生命周期管理                  │  │
│  │  - sendAndWait（frame ID 匹配）      │  │
│  │  - 文件传输协调                      │  │
│  └───────────┬─────────────────────────┘  │
│  ┌───────────▼─────────────────────────┐  │
│  │  transport.Hub                      │  │
│  │  - WebSocket 接入（mTLS）            │  │
│  │  - ClientSession 注册表             │  │
│  │  - 心跳检测（15s/30s 超时）          │  │
│  │  - 证书指纹白名单                    │  │
│  └───────────┬─────────────────────────┘  │
└──────────────┼────────────────────────────┘
               │  WSS (mTLS)
    ┌──────────┼──────────┐
    │          │          │
┌───▼───┐ ┌───▼───┐ ┌───▼───┐
│Client │ │Client │ │Client │
│Linux  │ │Win    │ │ESXi   │
└───────┘ └───────┘ └───────┘
```

---

## 通信协议

C2 通道使用基于 WebSocket 二进制消息的帧协议。所有帧均为 JSON。

### 帧结构

```json
{
  "type": "SESSION_OPEN",
  "id": "a1b2c3d4...",
  "client_id": "ubuntu-linux-amd64",
  "session_id": "e5f6...",
  "timestamp": "2026-06-28T14:00:00Z",
  "payload": { ... }
}
```

### 帧类型（15 种）

| 帧类型 | 方向 | 用途 |
|--------|------|------|
| `HELLO` | Client → Hub | 上报 client 身份、OS、能力 |
| `AUTH` | Hub → Client | 认证结果 |
| `HEARTBEAT` | Client → Hub | 定期心跳（15s 间隔） |
| `ACK` | Hub → Client | 心跳应答 |
| `SESSION_OPEN` | Hub → Client | 请求新建 shell 会话 |
| `SESSION_CLOSE` | Hub → Client | 终止 shell 会话 |
| `CMD_INPUT` | Hub → Client | 发送命令/文本到会话 stdin |
| `INTERRUPT` | Hub → Client | 发送 SIGINT / Ctrl-C |
| `OUTPUT_CHUNK` | Client → Hub | 流式传输会话 stdout/stderr |
| `ALIVE` | Client → Hub | 会话存活状态 |
| `FILE_UPLOAD` | 双向 | 上传文件到目标 |
| `FILE_DOWNLOAD` | 双向 | 从目标下载文件 |
| `FILE_ACK` | 双向 | 文件传输确认 |
| `ERROR` | 双向 | 错误报告 |
| `ACK` | 双向 | 通用确认 |

### Frame ID 匹配

请求帧携带唯一 ID（16 字节随机 hex）。响应方通过 `reply()` 回传相同 ID——这是 `sendAndWait` 做请求-响应匹配的机制。不使用 `reply()` 的错误响应会导致请求方超时（默认 10 秒）。

---

## 会话生命周期

```
start_session
  │
  ├─ hub: sendAndWait(SESSION_OPEN) → client
  │          │
  │   client: 打开 shell（PTY 或 pipe）
  │          │
  │          ├─ PTY 成功 → SESSION_OPEN { interactive: true }
  │          └─ PTY 失败 → pipe 降级 { interactive: false }
  │
  ├─ hub: 创建 remoteSession + Ring(1MB)
  │
run_command / send_input
  │
  ├─ hub: sendAndWait(CMD_INPUT) → client
  │          │
  │   client: 写入 stdin，立即返回 ACK
  │
read_output
  │
  ├─ hub: Ring.Read(since, maxBytes)
  │   长轮询：循环 50ms 直到有数据或超时
  │
  │   client 持续发送 OUTPUT_CHUNK:
  │     { data: [...], alive: true/false, exit_code: N }
  │          │
  │   remote.Manager: Ring.Write(data)
  │
close_session
  │
  ├─ hub: sendAndWait(SESSION_CLOSE) → client
  │          │
  │   client: cmd.Process.Kill()
  │
```

---

## 环形缓冲区与游标语义

每个会话的输出存储在 1MB 环形缓冲区中。每写入一个字节，单调游标递增。

```
环形缓冲区 (1MB)
┌────────────────────────────────────────┐
│ [旧数据] ... [新数据]                   │
│  ↑earliest     ↑latest                 │
│  cursor        cursor                  │
└────────────────────────────────────────┘
```

**读取结果字段：**

| 字段 | 含义 |
|------|------|
| `requested_since` | 你请求的游标位置 |
| `earliest_cursor` | 缓冲区中最旧字节的位置 |
| `new_cursor` | 最新字节位置（作为下次 `since` 参数） |
| `missed_bytes` | requested_since 到 earliest_cursor 之间丢失的字节数 |
| `since_status` | `ok`（正常）、`expired`（数据丢失）、`future`（游标超前）、`invalid_session` |
| `truncated_by` | `ring_buffer`（缓冲区溢出）、`max_bytes`（你设置了限制） |

**重要**：当 `since_status = "expired"` 时，返回的输出从 `earliest_cursor` 开始，不是你请求的位置。`missed_bytes` 告诉你丢失了多少。如果丢失内容关键，考虑重新执行命令。

---

## PTY 与管道模式

### PTY 模式（Linux/macOS，interactive=true）

- 使用 `creack/pty`，`Setsid: true`
- 完整伪终端：支持 Tab 补全、颜色、交互式提示
- 进程运行在独立 session 中（与 client 进程分离）
- 输出含原始 ANSI——MCP server 可按需去除 ANSI（`strip_ansi=true`）

### 管道模式（降级，interactive=false）

- 使用 `exec.Cmd` + stdin/stdout/stderr 管道
- 无终端模拟：不生成 ANSI 转义序列
- 交互式提示可能卡住（程序检测到非 TTY 输入）
- 双 goroutine 读取器合并 stdout + stderr

### 自动降级

```
打开会话
  ├─ 尝试 PTY（creack/pty.Start）
  │   └─ 成功 → interactive=true
  └─ PTY 失败（seccomp、UserCartel、权限不足）
      └─ 打开管道模式 → interactive=false
```

无需用户干预。工具报告 `interactive: false` 以便 AI 调整行为。

---

## mTLS 握手

```
Client                              Hub
  │                                  │
  │── TCP connect ──────────────────→│
  │←── ServerHello (server.crt) ────│
  │── ClientHello (client.crt) ────→│
  │←── Finished ────────────────────│
  │                                  │
  │── HELLO frame ─────────────────→│
  │     { hostname, os, arch, caps } │
  │                                  │
  │←── AUTH frame ──────────────────│
  │     { ok: true, fingerprint }    │
  │                                  │
  │  ← C2 通道建立完成 →             │
```

**证书流程：**
1. `make build` 时自动执行 `mcpc2-hub -gen-certs` 生成 CA + server + client 证书到 `certs/`
2. Client 编译时通过 `go:embed` 嵌入 `certs/ca.crt`、`certs/client.crt`、`certs/client.key`
3. Hub 启动时自动检测 certs 目录：有就用，没有就自动生成
4. Hub 用 CA 验证 client 证书；client 用 CA 验证 server 证书
5. 可选：指纹白名单（`-allowed-clients`）提供第二重验证

---

## Hub HTTP API

所有端点均在 `127.0.0.1:9000`。无认证（仅本地回环）。

### 会话

```
POST   /api/v1/sessions                          # 打开会话
GET    /api/v1/sessions?client_id=X               # 列出会话
DELETE /api/v1/sessions/:id?client_id=X           # 关闭会话
POST   /api/v1/sessions/:id/cmd?client_id=X       # 执行命令
POST   /api/v1/sessions/:id/input?client_id=X     # 发送输入
GET    /api/v1/sessions/:id/output?client_id=X&since=N&max_bytes=M&block_ms=B  # 读取输出
POST   /api/v1/sessions/:id/interrupt?client_id=X # 中断
```

### 文件

```
POST /api/v1/files/upload?client_id=X       # 上传
POST /api/v1/files/download?client_id=X     # 下载
GET  /api/v1/files/list?client_id=X&path=Y  # 列表
```

### 元数据

```
GET /api/v1/health     # hub 健康状态 + 版本 + client 数量
GET /api/v1/clients    # 列出已连接 client
GET /healthz           # 简单存活探针
```

---

## 包结构

```
cmd/
├── mcpc2-hub/main.go       — hub 守护进程入口
├── mcpc2-server/main.go    — MCP server 入口
└── mcpc2-client/main.go    — client agent 入口

internal/
├── hubapi/
│   ├── hubapi.go           — C2API 接口 + HTTP 客户端
│   └── handler.go          — REST API 处理器（hub 侧）
├── mcpserver/
│   └── server.go           — MCP server（工具、系统提示、辅助函数）
├── remote/
│   └── manager.go          — 服务端会话/文件管理器
├── transport/
│   ├── ws.go               — WebSocket hub（接入、心跳、读取循环）
│   └── dial.go             — Client 拨号器（连接、心跳、读取循环）
├── session/
│   ├── session_unix.go     — Linux/macOS PTY + 管道会话
│   └── session_windows.go  — Windows 管道会话
├── proto/
│   └── types.go            — 帧类型、载荷、ClientSummary
├── outputbuf/
│   └── ring.go             — 环形缓冲区 + 游标追踪
├── mtls/
│   └── mtls.go             — TLS 配置、指纹白名单
└── embedded/
    └── certs.go            — go:embed 证书嵌入（client 二进制）
```

---

## 并发模型

- **Hub**：每个 WebSocket 连接一个 goroutine（`readLoop`）。共享状态由 `sync.RWMutex` 保护（clients map、sessions map、pending map）。
- **Client**：三个 goroutine——读取循环、心跳定时器、ping 定时器。
- **会话**：管道模式使用两个 goroutine（stdout 读取器、stderr 读取器）。PTY 模式使用单个 goroutine 做 PTY→buffer 拷贝。
- **HTTP API**：标准 `net/http`，每个请求一个 goroutine。阻塞读取（长轮询）不会阻塞其他请求。

无全局状态。Hub、server、client 进程之间无共享内存。

---

## 关键设计决策

1. **即发即轮的指令模型**——`run_command` 立即返回。AI 通过轮询 `read_output` 获取结果。这避免长命令超时，并支持并发多个 session。

2. **基于游标的输出追踪**——字节偏移游标是单调的、无状态的，可跨越断连（client 重连 → 新 session，游标重置）。AI 不会丢失输出，只需追踪一个整数。

3. **Hub/Server 分离**——Hub 是常驻守护进程（保持 client 连接存活）。Server 是每个 Claude 实例的 stdio 代理。多个 Claude 实例共享一个 hub。

4. **证书嵌入**——Client 二进制在编译时通过 `go:embed` 捆绑证书。一个参数（`-server`）即可部署。目标机器上无需配置文件。

5. **PTY 自动降级**——PTY 可能因多种原因失败（seccomp、内核安全模块、缺少 `/dev/ptmx`）。自动管道降级保证工具在任何环境都能工作，只是交互性降低。
