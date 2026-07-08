---
name: uninstall
description: "从本机（操作者侧）卸载 debugMcp：停并删 systemd service、删 /usr/local/bin 下的 hub/shim/cli/probe 二进制、删 Claude Code 全局 MCP 配置（user scope）、可选删 state 目录（PSK/token/审计日志）。触发场景：(1) 彻底清掉本机 debugmcp；(2) 重装前先清旧；(3) 用户说「卸载 debugmcp」「删掉 hub」「移除 debugmcp mcp」。触发短语：卸载 debugmcp、删 hub、移除 debugmcp mcp、debugmcp uninstall、停掉 debugmcp 服务。不触发：清目标机上的 agent（用 deploy skill 的 Reference）、只停 hub 不删（直接 systemctl stop）、本机状态查看（直接 debugmcp-cli）。注意：uninstall 只清本机，不清目标机 agent。"
---

# uninstall — 从本机卸载 debugMcp

从**本机（操作者侧）**卸载 debugMcp 的全部痕迹：停并删 systemd service、删 `/usr/local/bin` 下的 hub/shim/cli/probe 二进制、删 Claude Code 全局 MCP 配置（user scope）、可选删 state 目录（PSK/token/审计日志）。

> **注意**：uninstall 只清**本机**。目标机上跑的 agent 不受影响——目标机 agent 的清理见 deploy skill 的 [Reference: 目标机清理](../deploy/docs/REFERENCE.md#目标机清理)。

## 前置条件

- 之前用 `/debugmcp:install` 装过本机侧（systemd service + 二进制 + MCP 配置）
- 有 sudo（删 `/usr/local/bin` 和 `/etc/systemd/system/` 需 root；demo 免密 / ubuntu 免密）
- Claude Code CLI 在 PATH（删 MCP 配置用）

## 卸载步骤

```bash
SVC=debugmcp-hub
BIN=/usr/local/bin
STATE="$HOME/.debugmcp"

# 1. 停并禁用 systemd service（幂等：不存在不报错）
sudo systemctl disable --now $SVC.service 2>/dev/null || true
sudo rm -f /etc/systemd/system/$SVC.service
sudo systemctl daemon-reload

# 2. 删本机二进制
sudo rm -f "$BIN"/debugmcp-{hub,shim,cli,probe}

# 3. 删 Claude Code 全局 MCP 配置（user scope）
claude mcp remove debugmcp --scope user 2>/dev/null || true

# 4. 可选：删 state 目录（PSK / token / hub.sock / 审计日志）
#    ⚠️ 删了 psk.hex 后，所有已部署的目标机 agent 都无法再回连（PSK 丢了）！
#    若只想重装、保留 PSK 让现有 agent 继续工作，跳过这步。
read -r -p "删除 state 目录 $STATE（PSK 会丢，已部署 agent 无法再连）？[y/N] " ans
[[ "$ans" == "y" || "$ans" == "Y" ]] && rm -rf "$STATE" && echo "已删 $STATE" || echo "保留 $STATE"

# 5. 杀残留进程（保险）
pkill -x debugmcp-hub 2>/dev/null || true
pkill -x debugmcp-shim 2>/dev/null || true

# 6. 验证
systemctl is-active $SVC 2>&1 | grep -q active && echo "❌ service 仍在" || echo "✓ service 已停删"
ls "$BIN"/debugmcp-* 2>/dev/null && echo "❌ 二进制残留" || echo "✓ 二进制已删"
claude mcp get debugmcp 2>&1 | grep -q . && echo "（MCP 配置: 仍有或已清，看上面输出）" || echo "✓ MCP 配置已删"
```

## 选项

| 选项 | 默认 | 说明 |
|---|---|---|
| state 目录 | 交互确认 | 删则 PSK 丢、已部署 agent 无法再连；保留则重装后可复用 PSK |
| 二进制目录 | `/usr/local/bin` | install 时若改过这里同步改 |
| MCP scope | user | install 时若改成 local，这里也改 `--scope local` |

## 不删什么

- **plugin 本身的 assets**：`${CLAUDE_PLUGIN_ROOT}/assets/bin/` 是 plugin 资产，删了 plugin 就废了。uninstall 只删**装到系统里的副本**（`/usr/local/bin/`、`/etc/systemd/system/`、MCP 配置）。
- **目标机上的 agent**：目标机 agent 不受本 skill 影响。要清目标机 agent 见 deploy skill 的 [Reference: 目标机清理](../deploy/docs/REFERENCE.md#目标机清理)。
- **Claude Code 的 plugin 安装记录**：要连 plugin 一起卸，用 `/plugin uninstall debugmcp@debugmcp-marketplace`（本 skill 不碰 plugin 注册）。

## 彻底清理（含目标机 agent + plugin）

```bash
# 1. 本 skill：清本机 hub/shim/systemd/MCP/state
# 2. deploy skill Reference：清每个目标机上的 agent 进程 + 二进制 + PSK 副本
# 3. Claude Code 里：/plugin uninstall debugmcp@debugmcp-marketplace
# 4. 可选：/plugin marketplace remove debugmcp-marketplace
```

## 故障排查

- **`systemctl disable` 报 unit 不存在**：正常，service 没装或已删；`|| true` 吞掉即可。
- **二进制删不掉（Permission denied）**：漏了 sudo；`/usr/local/bin` 写需要 root。
- **`claude mcp remove` 报 not found**：MCP 配置本来就没装或在 local scope；`claude mcp list` 看实际 scope。
- **state 目录删不掉**：`~/.debugmcp/hub.sock` 是 socket，`rm -rf` 能处理；若 hub 还在跑先 `pkill -x debugmcp-hub`。
