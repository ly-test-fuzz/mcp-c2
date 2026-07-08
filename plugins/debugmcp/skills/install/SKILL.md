---
name: install
description: "把 debugMcp 的本机侧（hub + shim + cli + probe）安装到操作者机器：装二进制到 /usr/local/bin、装 systemd service、启动 hub、配置 Claude Code 全局 MCP（user scope）。触发场景：(1) 第一次在一台操作者机器上启用 debugMcp；(2) 用户说「安装 debugmcp」「部署 hub」「配置 debugmcp MCP」「本机装 debugmcp」；(3) 升级本机 debugmcp 二进制后重新部署。触发短语：安装 debugmcp、部署本机 hub、装 hub、配置 debugmcp mcp、debugmcp install、本机安装。不触发：往目标机投递 agent（用 deploy skill）、卸载（用 uninstall skill）、只看状态（直接 debugmcp-cli）。"
---

# install — 安装 debugMcp 本机侧

把 debugMcp 的**本机侧**（操作者守护进程）装到当前机器：hub + shim + cli + probe 二进制、systemd service（常驻 + 开机自启）、Claude Code 全局 MCP 配置（user scope，所有项目可用）。装完后 hub 立即常驻，Claude Code 重启即可用 debugmcp 工具。

> 仅用于授权安全测试环境。hub 监听 `0.0.0.0:7777` 时暴露面仅由 PSK 保护，配错即 RCE。

## 资产（随本 plugin 自带，无需 Go 环境）

| 二进制 | 路径（plugin 内） | 用途 |
|---|---|---|
| debugmcp-hub | `${CLAUDE_PLUGIN_ROOT}/assets/bin/hub/debugmcp-hub` | 操作者守护进程（C2 listener + IPC server） |
| debugmcp-shim | `${CLAUDE_PLUGIN_ROOT}/assets/bin/shim/debugmcp-shim` | Claude Code 拉起的 stdio MCP 代理 |
| debugmcp-cli | `${CLAUDE_PLUGIN_ROOT}/assets/bin/hub/debugmcp-cli` | 人读状态工具 |
| debugmcp-probe | `${CLAUDE_PLUGIN_ROOT}/assets/bin/hub/debugmcp-probe` | IPC 探测工具 |

systemd 模板：`${CLAUDE_PLUGIN_ROOT}/assets/systemd/debugmcp-hub.service.template`

源码（仅改了 Go 代码要重编时才需要）：`/home/demo/Desktop/project/debugmcp/mcp-c2`（`./scripts/build.sh assets` 重打）。

## 前置条件

- Linux 操作者机（systemd）；有 sudo（demo 免密 / ubuntu 免密）
- 二进制架构匹配：本机 `linux/amd64`（plugin 自带的就是 amd64；其他架构需现场交叉编译——见末尾「非 amd64 操作者机」）
- Claude Code CLI 在 PATH（`which claude`），用于写 user-scope MCP 配置
- 旧版清理：若之前手动装过（`/usr/local/bin/debugmcp-*` 或 `~/.debugmcp/`），先跑 `/debugmcp:uninstall` 再 install，避免旧 service 残留

## 安装步骤

```bash
ROOT="${CLAUDE_PLUGIN_ROOT}"
BIN=/usr/local/bin
STATE="$HOME/.debugmcp"
USER="$(whoami)"
SVC=debugmcp-hub

# 1. 装二进制到 /usr/local/bin（需 sudo）
sudo install -m 0755 \
  "$ROOT/assets/bin/hub/debugmcp-hub" \
  "$ROOT/assets/bin/hub/debugmcp-cli" \
  "$ROOT/assets/bin/hub/debugmcp-probe" \
  "$ROOT/assets/bin/shim/debugmcp-shim" \
  "$BIN/"

# 2. 建 state 目录（PSK/token/sock/审计日志，0700）
mkdir -p "$STATE" && chmod 700 "$STATE"

# 3. 从模板生成 systemd unit（绑定 0.0.0.0:7777，让目标机 agent 回连）
#    若只在本机用 agent、不需要跨网络回连，把 LISTEN 改 127.0.0.1:7777 并删掉 ALLOW_INBOUND 行最安全
TMP=$(mktemp /tmp/debugmcp-hub.service.XXXX)
sed -e "s|__USER__|$USER|g" \
    -e "s|__STATE_DIR__|$STATE|g" \
    -e "s|__BINDIR__|$BIN|g" \
    -e "s|__LISTEN__|0.0.0.0:7777|g" \
    -e "s|__ALLOW_INBOUND__|-allow-inbound|g" \
    "$ROOT/assets/systemd/debugmcp-hub.service.template" > "$TMP"
sudo install -m 0644 "$TMP" /etc/systemd/system/$SVC.service
rm -f "$TMP"
sudo systemctl daemon-reload
sudo systemctl enable --now $SVC.service

# 4. 等 hub 起来、IPC token 就绪
for _ in $(seq 1 50); do
  [ -S "$STATE/hub.sock" ] && [ -s "$STATE/ipc.token" ] && break
  sleep 0.1
done

# 5. 配置 Claude Code 全局 MCP（user scope，所有项目可用）
TOKEN=$(cat "$STATE/ipc.token")
claude mcp add-json --scope user debugmcp \
  "{\"command\":\"$BIN/debugmcp-shim\",\"env\":{\"DBGMCP_HUB_ADDR\":\"unix:$STATE/hub.sock\",\"DBGMCP_HUB_TOKEN\":\"$TOKEN\"}}"

# 6. 验证
sudo systemctl is-active $SVC           # -> active
ss -tln | grep 7777                      # -> 0.0.0.0:7777 LISTEN
debugmcp-cli                             # -> hub: up  (connected agents: 0)
claude mcp get debugmcp                  # -> Scope: User config, Connected
```

## 选项

| 选项 | 默认 | 说明 |
|---|---|---|
| 监听地址 | `0.0.0.0:7777` | 目标机 agent 回连用。要锁回环：`127.0.0.1:7777` + 删 `-allow-inbound` |
| 二进制目录 | `/usr/local/bin` | 改则同步改 systemd unit 的 `__BINDIR__` 和 MCP `command` |
| state 目录 | `~/.debugmcp` | PSK/token/sock/审计日志，0700 |
| MCP scope | user | 所有项目可用。要单项目则改 `--scope local` |

## 凭据位置（install 后）

| 文件 | 用途 | 权限 |
|---|---|---|
| `~/.debugmcp/psk.hex` | C2 传输层 PSK（agent 注册密钥） | 0600，仅目标机 agent 需要 |
| `~/.debugmcp/ipc.token` | shim/probe/cli 连 hub 的 MAC token | 0600，已写入 MCP 配置 |
| `~/.debugmcp/hub.sock` | IPC Unix socket | 0600 |
| `~/.debugmcp/audit.jsonl` | 审计日志（append-only + fsync） | 0600 |

PSK 用于 `deploy` skill 投递 agent 时回连。**切勿**提交 git / 进日志 / 进 `ps`。

## 后续

- 投递 agent 到目标机：`/debugmcp:deploy`
- 看状态：`debugmcp-cli` / `debugmcp-cli targets` / `debugmcp-cli sessions`
- hub 日志：`sudo journalctl -u debugmcp-hub -f`
- 重启 hub：`sudo systemctl restart debugmcp-hub`
- 卸载（含二进制 + systemd + MCP 配置 + state）：`/debugmcp:uninstall`

## 升级

改了 Go 代码重编后，重打 plugin 资产再重装：

```bash
cd /home/demo/Desktop/project/debugmcp/mcp-c2
./scripts/build.sh assets plugins/debugmcp     # 重打 assets/bin/
# 然后 /plugin update debugmcp@debugmcp-marketplace 刷新 plugin cache
# 再跑本 install skill（systemd unit 会覆盖、MCP 配置不变因为 token 复用）
```

## 非 amd64 操作者机

plugin 自带的本机端二进制是 `linux/amd64`。若操作者机是 arm64 等，现场交叉编译：

```bash
cd /home/demo/Desktop/project/debugmcp/mcp-c2
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
  -o plugins/debugmcp/assets/bin/hub/debugmcp-hub ./cmd/hub
# (cli/probe/shim 同理，各指向对应 cmd/)
# 再跑本 install skill
```

## 故障排查

- **systemctl is-active 失败**：`sudo journalctl -u debugmcp-hub -n 50` 看启动报错；常见是 `refusing non-loopback bind` ——没加 `-allow-inbound` 却绑了 `0.0.0.0`，检查生成的 unit。
- **hub.sock 一直不出现**：`journalctl -u debugmcp-hub` 看 state dir 权限；`~/.debugmcp` 必须 0700 且属主是运行用户。
- **`claude mcp get debugmcp` 显示 Disconnected**：token 不匹配——确认 MCP 配置里的 `DBGMCP_HUB_TOKEN` 与 `~/.debugmcp/ipc.token` 一致；`debugmcp-probe list_targets` 用同 token 测能否连上。
- **MCP 配置写错位置**：user scope 必须在 `~/.claude.json` 顶层 `mcpServers`，用 `claude mcp add-json --scope user` 写最稳，别手编那个大文件。
