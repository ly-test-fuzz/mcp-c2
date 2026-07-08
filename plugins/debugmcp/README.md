# debugMcp plugin

一面向**授权安全测试环境**的 C2 风格 MCP —— 为那些难以运行 SSH 的目标机提供 SSH
替代方案，设计上由 Claude Code 驱动。本 plugin 自带全平台预编译 Go 二进制，装即用，
无需 Go 环境。

> 授权范围：仅用于授权安全测试环境的远程管理 / 测试基础设施。传输层默认加密且双向
> 认证；这里的配置错误等同于 RCE，因此默认配置是安全的。

## 架构（三平面）

```
Claude Code  --stdio MCP-->  shim  --Unix socket + MAC token-->  HUB  --Noise_NNpsk0 (PSK)-->  agent（目标机）
                              (MCP)        （控制平面 / IPC）              （传输平面）           exec / PTY shell / fs
```

- **Hub**：面向 agent 的 TCP+Noise 监听器、agent 注册表、session/占用表、审计日志，
  以及面向 shim 的 MAC-token IPC 服务端。跑成 systemd service 常驻。
- **Agent**：拨号连接 hub，用注册时的 PSK 完成认证，daemonize，提供 exec / 交互式
  shell / 文件系统操作。跑在**目标机**上。
- **Shim**：唯一会说 MCP 的组件；Claude Code 每个会话拉起一个；通过 IPC 把工具调用
  代理转发给 hub。跑在**本机**。

## 四个 skill

| skill | 干什么 | 在哪台机器 |
|---|---|---|
| `install` | 装 hub+shim+cli+probe 二进制、systemd service、Claude Code 全局 MCP 配置 | **本机**（操作者侧） |
| `deploy` | 投递 agent 二进制到目标机、回连 hub、验证 | **目标机** |
| `usage` | 使用指南：选目标、跑命令、交互 shell、传文件（单文件+目录）、看状态 | **操作指南** |
| `uninstall` | 停删 systemd、删本机二进制、删 MCP 配置、可选删 state | **本机** |

典型流程：`install`（本机起 hub）→ `deploy`（目标机回连）→ `usage`（照指南操作目标机）→
`uninstall`（清本机）+ deploy 的 Reference（清目标机 agent）。

## 资产（随 plugin 自带）

```
plugins/debugmcp/assets/bin/
├── hub/                          # 本机端 (linux/amd64)
│   ├── debugmcp-hub
│   ├── debugmcp-cli
│   └── debugmcp-probe
├── shim/
│   └── debugmcp-shim             # 本机端 (linux/amd64)
└── client/                       # 目标机端 (全平台交叉编译)
    ├── linux-amd64/debugmcp-agent
    ├── linux-arm64/debugmcp-agent
    ├── windows-amd64/debugmcp-agent.exe
    └── windows-arm64/debugmcp-agent.exe

plugins/debugmcp/assets/systemd/
└── debugmcp-hub.service.template # systemd unit 模板（install 时 sed 填参）
```

体积约 29MB。版本号由 `scripts/build.sh` 从 git describe 注入，每个二进制 `-version`
可查。

## 安装

本 plugin 是一个本地 marketplace（仓库根 = marketplace 根）：

```bash
# 在 Claude Code 里：
/plugin marketplace add /home/demo/Desktop/project/debugmcp/mcp-c2
/plugin install debugmcp@debugmcp-marketplace
# 然后 /reload-plugins 或重启 Claude Code
```

装完 plugin 后，调用 `/debugmcp:install` 把本机侧（hub+shim+systemd+MCP）部署起来。

## 安全模型

- **传输层**：Noise_NNpsk0（ChaChaPoly1305 + Curve25519 + BLAKE2b），由注册时生成的
  32 字节 PSK 作为密钥。双向 PSK 确认；经瞬时 DH 提供前向保密。PSK 错误 → 握手失败。
- **控制层（IPC）**：Unix socket `0600` + MAC token；token 错误被拒。
- **安全默认值**：监听器绑定回环地址；`0.0.0.0` 需 `--allow-inbound`；agent 自动
  daemonize；审计日志 append-only，逐条 fsync。
- **暴露面**：`install` 默认让 hub 绑 `0.0.0.0:7777`（带 `--allow-inbound`）好让目标机
  agent 回连——此端口仅由 PSK 保护，**配错即 RCE**。若 agent 不跨网络，改回环最安全。

## MCP 工具（hub 提供，shim 代理）

| 工具 | 用途 |
|---|---|
| `list_targets` | 列出带实时占用信息的目标机 |
| `select_target` | 为当前 Claude 会话设置默认目标机 |
| `exec` | 一次性命令（login shell；退出码权威） |
| `shell_open` / `shell_send` / `shell_read` / `shell_close` | 交互式 PTY 会话 |
| `signal` | interrupt / terminate / force_kill / quit |
| `upload` / `download` | scp 风格路径到路径传输 |
| `list_sessions` / `status` | 跨目标机的 session + 占用视图 |

## 重编 / 重打资产

改了 Go 代码后重编并刷新 plugin 资产：

```bash
cd /home/demo/Desktop/project/debugmcp/mcp-c2
./scripts/build.sh assets plugins/debugmcp     # 全平台构建 + 整合到 assets/bin/
# 然后 Claude Code 里 /plugin update debugmcp@debugmcp-marketplace 刷新 cache
```

`build.sh` 还支持 `install [bindir]`（装本机端到 PATH）和子命令 `hub|shim|client`
（只编指定端到 `dist/`）。源码与构建细节见仓库根 `README.md` / `scripts/build.sh`。

## 状态查看（install 后）

```bash
debugmcp-cli                # hub 是否在跑 + 连接的 agent + 活跃 session
debugmcp-cli targets        # 列出已连接的目标机
debugmcp-cli sessions       # 列出活跃 session
debugmcp-cli --json         # 裸 JSON
sudo journalctl -u debugmcp-hub -f   # hub 实时日志
```

## 凭据位置速查

| 文件 | 用途 | 谁需要 |
|------|------|--------|
| `~/.debugmcp/psk.hex` | C2 传输层 PSK（agent 注册密钥） | 仅目标机 agent（deploy 时传过去） |
| `~/.debugmcp/ipc.token` | shim/probe/cli 连 hub 的 MAC token | 仅本机 shim/probe/cli（已写入 MCP 配置） |
| `~/.debugmcp/hub.sock` | IPC Unix socket | 仅本机 |
| `~/.debugmcp/audit.jsonl` | 审计日志（append-only + fsync） | 仅本机查看 |

PSK 与 IPC token 均 0600，不提交 git。`uninstall` 时 state 目录交互确认才删（删了 PSK
则已部署 agent 无法再连）。
