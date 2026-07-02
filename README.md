# debugMcp

一��面向**授权安全测试环境**的 C2 风格 MCP —— 为那些难以运行 SSH 的目标机提供 SSH
替代方案，设计上由 Claude Code 驱动。操作者运行一个常驻的 **hub**；每个目标机上的
**C2 agent** 通过双向认证的加密信道回连；Claude Code 每个会话拉起一个轻量的 **stdio MCP
shim**，把工具调用代理转发给 hub。

> 授权范围：仅用于授权安全测试环境的远程管理 / 测试基础设施。传输层默认加密且双向
> 认证；这里的配置错误等同于 RCE，因此默认配置是安全的。

## 架构（三平面）

```
Claude Code  --stdio MCP-->  shim  --Unix socket + MAC token-->  HUB  --Noise_NNpsk0 (PSK)-->  agent（目标机）
                              (MCP)        （控制平面 / IPC）              （传输平面）           exec / PTY shell / fs
```

- **Hub**（`cmd/hub`）：面向 agent 的 TCP+Noise 监听器、agent 注册表、session/占用表、
  审计日志（append-only + fsync），以及面向 shim 的 MAC-token IPC 服务端。
- **Agent**（`cmd/agent`）：拨号连接 hub，用注册时的 PSK 完成认证，daemonize
  （POSIX setsid），并提供 exec / 交互式 shell / 文件系统操作。
- **Shim**（`cmd/shim`）：唯一会说 MCP 的组件；Claude Code 每个会话拉起一个；通过 IPC
  把工具调用代理转发给 hub。

## 快速开始

```bash
# 1. hub（在 ~/.debugmcp 下生成并持久化 PSK + IPC token，并打印它们）
go run ./cmd/hub -listen 127.0.0.1:7777
#   -> 输出：psk=<hex>  ipc=<path>  token=<hex>

# 2. 目标机上的 agent（用打印出来的 PSK）
./debugmcp-agent -hub <操作者-ip>:7777 -psk <hex>
#   （生产环境别加 -no-daemon，让它脱离启动它的 shell）

# 3. Claude Code MCP 配置（stdio）
#    command: debugmcp-shim
#    env:     DBGMCP_HUB_SOCKET=<path>  DBGMCP_HUB_TOKEN=<hex>
```

若要把 agent 监听器绑定到非回环地址（真正的回调），传 `-allow-inbound`；hub 在没有该
开关的情况下会拒绝 `0.0.0.0`，以防意外暴露。

## MCP 工具

| 工具 | 用途 |
|---|---|
| `list_targets` | 列出带实时占用信息的目标机（`sessions_active`、`concurrency_cap`、`busy`） |
| `select_target` | 为当前 Claude 会话设置默认目标机（归属标记，不是隔离） |
| `exec` | 通过目标机 login shell 的一次性命令（`bash -lc` / `cmd /c` / `pwsh -c`）；退出码权威 |
| `shell_open` / `shell_send` / `shell_read` / `shell_close` | 独立的交互式 PTY 会话（状态持久）；`shell_read.completion` 标识 "完成" 有多可信 |
| `signal` | `interrupt`（Ctrl-C）/ `terminate` / `force_kill` / `quit` |
| `upload` / `download` | scp 风格的路径到路径传输（文件或目录由 `is_dir` 区分）；字节流绝不进入 LLM 上下文 —— 只有 `{local_path, remote_path, is_dir}` 和 `{size, sha256, n_entries}` |
| `list_sessions` / `status` | 跨目标机的 session + 占用视图 |

全双工指引：会自行退出的命令用 `exec`（权威的完成判定，经 login shell 走 bash 语法）；
需要真正交互 / 长生命周期进程 / Ctrl-C / 跨调用保留 `cd`+env 这类有状态行为的用 `shell_*`。

文件指引：`upload`/`download` 是 scp 的替代 —— 传两个路径加 `is_dir`（true = 经流式 tar
递归传目录，类似 `scp -r`；false = 原样传单文件，所以一个真正的 `.tar` 文件会被当作文件
发出去，不会自动解包）。要列目录 / 看 stat 用 `exec ls -la` / `exec stat`。操作者侧的本地
磁盘 I/O 由 hub 完成；shim 始终是纯代理。

## 安全模型

- **传输层**：Noise_NNpsk0（ChaChaPoly1304 + Curve25519 + BLAKE2b），由注册时生成的
  32 字节 PSK 作为密钥。双向 PSK 确认；经瞬时 DH 提供前向保密。PSK 错误 → 握手失败
  （测试覆盖）。
- **控制层（IPC）**：Unix socket `0600` + MAC token；token 错误被拒（测试覆盖）。
- **安全默认值**：监听器绑定回环地址；`0.0.0.0` 需 `--allow-inbound`；agent 自动
  daemonize；审计日志 append-only，逐条 fsync。

## 构建与测试

```bash
go build ./...                       # linux
GOOS=windows GOARCH=amd64 go build ./...   # 交叉编译
go test ./...                        # 所有单测 + E2E 测试
go test ./internal/wire -fuzz=FuzzReadFrame -fuzztime=30s   # 帧解析器 fuzz
```

## 状态（MVP —— Phase 0/1 纵向打通，Linux 优先）

已实现并验证：wire 协议（+fuzz）、Noise 传输、hub（注册表/mux/占用/IPC/审计）、
agent（exec/shell/fs/daemonize）、MCP shim（13 个工具）、3 个二进制。
交叉编译干净（Windows agent 在 ConPTY 落地前是桩）。

暂缓（见 `.omc/RALPLAN.md`）：签名 token 注册 + PSK 轮换与吊销、审计哈希链、
中继/NAT 穿透、beaconing 模式、Windows ConPTY + detached 运行、按操作者的 ACL。
