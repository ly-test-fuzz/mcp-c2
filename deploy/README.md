# 部署运维

debugMcp 已做成 Claude Code plugin，自带全平台预编译二进制，装即用、无需 Go 环境。
**推荐用 plugin 的三个 skill 完成全部部署/投递/卸载**，本文档仅作总览与手动回退参考。

## 推荐路径：Claude Code plugin

在 Claude Code 里添加本仓库为本地 marketplace，然后装 plugin：

```
/plugin marketplace add /home/demo/Desktop/project/debugmcp/mcp-c2
/plugin install debugmcp@debugmcp-marketplace
/reload-plugins
```

装完后三个 skill（重启 Claude Code 后会话可加载）：

| skill | 干什么 | 在哪台机器 |
|---|---|---|
| `/debugmcp:install` | 装 hub+shim+cli+probe 二进制、systemd service、Claude Code 全局 MCP 配置 | 本机（操作者侧） |
| `/debugmcp:deploy` | 投递 agent 二进制到目标机、回连 hub、验证 | 目标机 |
| `/debugmcp:uninstall` | 停删 systemd、删本机二进制、删 MCP 配置、可选删 state | 本机 |

典型流程：`install`（本机起 hub）→ `deploy`（目标机回连）→ 用 MCP 工具操作目标机 →
`uninstall`（清本机）+ deploy skill 的 Reference（清目标机 agent）。

plugin 自带资产：`hub/shim/cli/probe`（linux/amd64）+ 4 平台 client agent
（linux amd64/arm64、windows amd64/arm64）+ systemd unit 模板，约 29MB。
版本号由 `scripts/build.sh` 从 git describe 注入，每个二进制 `-version` 可查。

## 手动回退（不用 plugin）

若不想用 plugin、要手动部署，用编译脚本的 `install` 子命令一站式装本机端：

```bash
./scripts/build.sh install            # 构建 + 装到 /usr/local/bin（需 sudo）
```

systemd unit 用模板渲染（plugin install skill 也是这么干的）：

```bash
sed -e "s|__USER__|$(whoami)|g" \
    -e "s|__STATE_DIR__|$HOME/.debugmcp|g" \
    -e "s|__BINDIR__|/usr/local/bin|g" \
    -e "s|__LISTEN__|0.0.0.0:7777|g" \
    -e "s|__ALLOW_INBOUND__|-allow-inbound|g" \
    deploy/debugmcp-hub.service.template | sudo tee /etc/systemd/system/debugmcp-hub.service
sudo systemctl daemon-reload
sudo systemctl enable --now debugmcp-hub.service
```

Claude Code 全局 MCP 配置（user scope，所有项目可用）：

```bash
TOKEN=$(cat ~/.debugmcp/ipc.token)
claude mcp add-json --scope user debugmcp \
  "{\"command\":\"/usr/local/bin/debugmcp-shim\",\"env\":{\"DBGMCP_HUB_ADDR\":\"unix:$HOME/.debugmcp/hub.sock\",\"DBGMCP_HUB_TOKEN\":\"$TOKEN\"}}"
```

> 注意环境变量是 `DBGMCP_HUB_ADDR`（`unix:/path` 或 `tcp:host:port`），
> 旧的 `DBGMCP_HUB_SOCKET` 仍兼容但已不推荐。

验证：

```bash
claude mcp get debugmcp     # 应显示 Scope: User config
claude mcp list             # debugmcp 出现且 Connected
```

## 状态查看

```bash
debugmcp-cli                # hub 是否在跑 + 连接的 agent + 活跃 session
debugmcp-cli targets        # 列出已连接的目标机
debugmcp-cli sessions       # 列出活跃 session
debugmcp-cli --json         # 裸 JSON
sudo journalctl -u debugmcp-hub -f   # hub 实时日志
```

## 重编 / 刷新 plugin 资产

改了 Go 代码后重编并刷新 plugin 资产（再 `/plugin update debugmcp@debugmcp-marketplace`）：

```bash
./scripts/build.sh assets plugins/debugmcp     # 全平台构建 + 整合到 assets/bin/
```

## 凭据位置速查

| 文件 | 用途 | 谁需要 |
|------|------|--------|
| `~/.debugmcp/psk.hex` | C2 传输层 PSK（agent 注册密钥） | 仅目标机 agent |
| `~/.debugmcp/ipc.token` | shim/probe/cli 连 hub 的 MAC token | 仅本机 shim/probe/cli |
| `~/.debugmcp/hub.sock` | IPC Unix socket | 仅本机 |
| `~/.debugmcp/audit.jsonl` | 审计日志（append-only + fsync） | 仅本机查看 |

PSK 与 IPC token 均 0600，不要提交到仓库（已在 `.gitignore`）。

## 安全

`0.0.0.0:7777` 把 C2 端口暴露到网络，仅由 PSK 保护（配错即 RCE）。不要把 PSK 泄漏给
目标机以外的人。若 agent 不需要跨网络回连，把 `__LISTEN__` 改成 `127.0.0.1:7777` 并
删掉 `__ALLOW_INBOUND__` 行最安全。
