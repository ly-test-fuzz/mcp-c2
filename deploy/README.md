# 部署运维

本机（操作者侧）部署：hub 跑成 systemd service 常驻，Claude Code 通过全局 MCP 配置接入 shim。

## 前置

```bash
# 编译三端产物到 dist/
./scripts/build.sh
```

## 1. 安装二进制到 PATH

```bash
sudo install -m 0755 dist/hub/debugmcp-hub dist/hub/debugmcp-cli dist/hub/debugmcp-probe \
     dist/shim/debugmcp-shim /usr/local/bin/
```

## 2. 创建 hub state 目录

PSK / IPC token / hub.sock / 审计日志都落在这里，权限 0700。

```bash
mkdir -p ~/.debugmcp && chmod 700 ~/.debugmcp
```

## 3. 安装 systemd service（hub 常驻 + 开机自启）

`deploy/debugmcp-hub.service` 默认监听 `0.0.0.0:7777`（带 `--allow-inbound`），让目标机 agent 能回连。

```bash
sudo install -m 0644 deploy/debugmcp-hub.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now debugmcp-hub.service
```

常用命令：

```bash
sudo systemctl status debugmcp-hub        # 状态
sudo systemctl restart debugmcp-hub       # 重启
sudo journalctl -u debugmcp-hub -f        # 实时日志
```

> **安全**：`0.0.0.0:7777` 把 C2 端口暴露到网络，仅由 PSK 保护（配错即 RCE）。
> 不要把 PSK 泄漏给目标机以外的人。若 agent 不需要跨网络回连，改成 `127.0.0.1:7777`
> 并去掉 `--allow-inbound` 最安全。

## 4. 配置 Claude Code 全局 MCP（user scope）

hub 首次启动会生成 IPC token，从 state 目录读取：

```bash
TOKEN=$(cat ~/.debugmcp/ipc.token)
```

用户级（user scope）配置由 Claude Code 存在 `~/.claude.json` 的**顶层 `mcpServers`** 键（所有项目可用）。**用 CLI 写入最安全**（避免手编这个含大量状态的大文件）：

```bash
claude mcp add-json --scope user debugmcp \
  "{\"command\":\"/usr/local/bin/debugmcp-shim\",\"env\":{\"DBGMCP_HUB_SOCKET\":\"$HOME/.debugmcp/hub.sock\",\"DBGMCP_HUB_TOKEN\":\"$TOKEN\"}}"
```

验证：

```bash
claude mcp get debugmcp     # 应显示 Scope: User config
claude mcp list             # debugmcp 出现且 Connected
```

> **注意位置**：用户级 MCP 配置在 `~/.claude.json` 顶层 `mcpServers`，**不是** `~/.claude/.mcp.json`（后者 Claude Code 不读取）。手编时把条目放进顶层 `mcpServers`，不要嵌进 `projects.*.mcpServers`（那是 local scope，仅当前项目）。

重启 Claude Code 后即可使用 debugmcp 工具。

## 5. 状态查看

```bash
debugmcp-cli                # hub 是否在跑 + 连接的 agent + 活跃 session
debugmcp-cli targets        # 列出已连接的目标机
debugmcp-cli sessions       # 列出活跃 session
debugmcp-cli --json         # 裸 JSON
```

## 6. 在目标机上部署 agent

目标机需要 `debugmcp-agent`（交叉编译产物，见 `dist/client/`）和 hub 的 PSK：

```bash
# hub 日志或 ~/.debugmcp/psk.hex 里取 PSK
debugmcp-agent -hub <操作者IP>:7777 -psk <PSK_HEX>
# 默认会 daemonize 脱离父进程；调试时加 -no-daemon
```

回连成功后，本机 `debugmcp-cli targets` 即可看到该 agent。

## 凭据位置速查

| 文件 | 用途 | 谁需要 |
|------|------|--------|
| `~/.debugmcp/psk.hex` | C2 传输层 PSK（agent 注册密钥） | 仅目标机 agent |
| `~/.debugmcp/ipc.token` | shim/probe/cli 连 hub 的 MAC token | 仅本机 shim/probe/cli |
| `~/.debugmcp/hub.sock` | IPC Unix socket | 仅本机 |
| `~/.debugmcp/audit.jsonl` | 审计日志（append-only + fsync） | 仅本机查看 |

PSK 与 IPC token 均 0600，不要提交到仓库（已在 `.gitignore`）。
