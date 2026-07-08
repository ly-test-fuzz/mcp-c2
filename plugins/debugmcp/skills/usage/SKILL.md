---
name: usage
description: "debugMcp C2 MCP 的使用指南：怎么选目标机、跑命令、开交互 shell、传文件（单文件 + 目录）、看状态。重点讲清 upload/download 在 is_dir=true/false 两种模式下的行为差异与陷阱。触发场景：(1) 第一次用 debugmcp 操作目标机、想知道有哪些工具怎么用；(2) 用户问「怎么用 debugmcp」「debugmcp 怎么传文件」「debugmcp 怎么上传目录」「is_dir 怎么选」；(3) 上传/下载出错需要核对用法。触发短语：debugmcp 用法、怎么用 debugmcp、debugmcp 上传文件、debugmcp 下载目录、is_dir、debugmcp 传目录、debugmcp scp。不触发：本机装 hub（install）、投递 agent（deploy）、卸载（uninstall）。前提：hub 在跑 + 至少一个 agent 已回连。"
---

# usage — debugMcp 使用指南

debugMcp 装好（`/debugmcp:install`）且至少一个 agent 已回连（`/debugmcp:deploy`）后，
本 skill 教你怎么**用**这套 MCP 工具操作目标机。

## 前置检查

```bash
systemctl is-active debugmcp-hub     # active
debugmcp-cli targets                 # 至少一个 agent, status=online
```

若 0 个 agent：先跑 `/debugmcp:deploy`。Claude Code 里 `list_targets` 也能看。

## 工具总览（13 个）

| 工具 | 干什么 | 何时用 |
|---|---|---|
| `list_targets` | 列已连接 agent + 占用 | 看有哪些目标机、谁在忙 |
| `select_target` | 给当前 Claude 会话设默认目标 | 一次选定，后续调用省 `target` 参数 |
| `exec` | 一次性命令（login shell，退出码权威） | 跑会自行退出的命令、列目录、看 stat |
| `shell_open` / `shell_send` / `shell_read` / `shell_close` | 交互式 PTY | 长生命周期进程、`cd`+env 跨调用保留、Ctrl-C |
| `signal` | interrupt / terminate / force_kill / quit | 控制交互式 shell |
| `upload` / `download` | 路径到路径传文件/目录 | **重点见下** |
| `list_sessions` / `status` | 跨目标机 session + 占用 | 全局视图 |

## 0. 选目标机

两种方式：

```text
# 方式 A：一次性，每个调用带 target
exec { command: "uname -a", target: "web-01" }

# 方式 B：先选定默认，后续调用省 target
select_target { target: "web-01" }
exec { command: "uname -a" }          # 自动用 web-01
upload { local_path: "/tmp/a.sh", remote_path: "/tmp/a.sh", is_dir: false }
```

`select_target` 是**归属标记不是隔离**——它只设当前 Claude 会话的默认，不影响别的会话。
未选定又省 `target` → 报 `no target selected; call select_target first`。

## 1. 跑命令：exec vs shell_*

**会自行退出的命令用 `exec`**（权威的完成判定，经 login shell 走 bash 语法）：

```text
exec { command: "uname -a" }
exec { command: "ls -la /etc/nginx", target: "web-01" }
exec { command: "cat /etc/os-release | grep VERSION", timeout_ms: 5000 }
exec { command: "systemctl status nginx" }
```

返回 `{stdout, stderr, exit_code, duration_ms}`。退出码权威——非 0 就是失败。支持管道、
重定向、`$()`，因为走 `bash -lc`（Windows 走 `cmd /c`，可 `shell:"pwsh"` 切）。

**需要真正交互 / 长生命周期 / Ctrl-C / 跨调用保留状态用 `shell_*`**：

```text
shell_open { }                      # -> {sid}  开一个 PTY 会话
shell_send { sid: "xxx", input: "cd /var/log\n" }
shell_read { sid: "xxx" }           # -> {output, completion}
shell_send { sid: "xxx", input: "tail -f /var/log/syslog\n" }
signal { sid: "xxx", sig: "interrupt" }   # Ctrl-C 停掉 tail
shell_close { sid: "xxx" }          # 释放槽位
```

`shell_read.completion` 告诉你「完成」有多可信：进程退出时是权威，否则是启发式。长生命周期
进程别等它「完成」——读一波输出就走，要停用 `signal`。

## 2. ★ 传文件：upload / download（重点）

upload/download 是 **scp 的替代**，核心参数是两个路径 + 一个 `is_dir` 布尔。
**字节流绝不进入 LLM 上下文**——只有路径 + `{size, sha256, n_entries}` 进来。

### 参数

| upload | download | 含义 |
|---|---|---|
| `local_path` | `local_path` | **操作者本机**（hub 进程能看到的）路径 |
| `remote_path` | `remote_path` | **目标机**上的路径 |
| `is_dir` | `is_dir` | true=目录走 tar 流（像 `scp -r`）；false=单文件原样传 |
| `target`（可选） | `target`（可选） | 目标机，省略用 `select_target` 的默认 |

> **local 永远指操作者本机，remote 永远指目标机**，upload/download 都一样。方向由工具名决定。

### 模式一：单文件（`is_dir: false`）

原样传一个文件。一个真正的 `.tar` 文件会被**当文件**发出去，不会自动解包。

**上传单文件到目标机：**

```text
upload {
  local_path: "/home/demo/exploits/payload.sh",
  remote_path: "/tmp/payload.sh",
  is_dir: false
}
# -> { size: 1234, sha256: "ab12...", duration_ms: 80 }
```

**从目标机下载单文件：**

```text
download {
  remote_path: "/etc/nginx/nginx.conf",
  local_path: "/home/demo/loot/nginx.conf",
  is_dir: false
}
# -> { size: 2345, sha256: "cd34...", duration_ms: 90 }
```

单文件模式的特点：
- `size` = 文件字节数，`sha256` = 文件内容的 sha256（端到端校验，agent 侧重算比对）
- `remote_path` 必须是一个**具体文件路径**，不能是目录
- 上传时目标路径的父目录必须存在（agent 不会 `mkdir -p`）；权限由源文件 mode 决定
- 下载时 `local_path` 的父目录必须存在（hub 侧 `os.Create`）

### 模式二：目录（`is_dir: true`）

递归传整棵目录树，**像 `scp -r`**：源端流式打包成 tar 流 → 传过去 → 接收端流式 untar。

**上传目录到目标机：**

```text
upload {
  local_path: "/home/demo/tools/jwrd/",
  remote_path: "/tmp/jwrd",
  is_dir: true
}
# -> { size: 8192, sha256: "ef56...", n_entries: 12, entries: [...], duration_ms: 200 }
```

**从目标机下载目录：**

```text
download {
  remote_path: "/var/log/nginx/",
  local_path: "/home/demo/loot/nginx-logs",
  is_dir: true
}
# -> { size: 20480, sha256: "...", n_entries: 8, entries: ["a.log","b.log",...], duration_ms: 150 }
```

目录模式的特点：
- `size` / `sha256` 是 **tar 字节流**（含 tar header）的统计，不是原始文件字节数——所以
  它会比 `du -sh <dir>` 大一点（header 开销）。要核对原始大小用 `exec du -sb <dir>`
- `n_entries` 是 entry 数（含目录条目），`entries` 是已落地的相对路径（commit 时由 agent 回报）
- 上传：`remote_path` 是**目标目录路径**，agent 会在目标机创建它（untar 时建）；`local_path`
  必须是已存在的目录
- 下载：`local_path` 是**本机目标目录路径**，hub 侧 untar 时创建；`remote_path` 必须是目标机上
  已存在的目录
- 权限：上传时目录 mode `0o755`；下载保留 tar 流里的原始 mode

### ⚠️ is_dir 选错的后果

| 错法 | 后果 |
|---|---|
| 传单文件却写 `is_dir: true` | hub 试图 `TarDir(文件)` → 报 `is not a directory`，失败 |
| 传目录却写 `is_dir: false` | hub 试图 `os.Open(目录)` 当文件读 → 行为异常/失败 |
| 把 `.tar` 文件当目录传（`is_dir: true`） | 当文件传（`is_dir: false`）才对——`.tar` 是文件不是目录 |
| 想传 `.tar` 并自动解包 | **debugmcp 不自动解包**。先 `upload` 传 .tar 文件（`is_dir:false`），再 `exec tar xf /tmp/x.tar` |

**判断 is_dir 的唯一可靠方法**：在操作者本机看 `local_path`（上传）或在目标机看
`remote_path`（下载）是不是目录：

```bash
# 上传前确认本机源
[ -d /home/demo/tools/jwrd ] && echo "is_dir:true" || echo "is_dir:false"

# 下载前确认目标机源
exec { command: "test -d /var/log/nginx && echo DIR || echo FILE" }
```

### 传完验证

```text
# 上传后核对目标机
exec { command: "ls -la /tmp/payload.sh && sha256sum /tmp/payload.sh" }
# 把 exec 输出的 sha256 和 upload 返回的 sha256 对比——必须一致

# 下载后核对本机
# （在本机 shell 跑，不在 debugmcp 里）sha256sum /home/demo/loot/nginx.conf
# 和 download 返回的 sha256 对比
```

单文件 sha256 是文件内容的、目录 sha256 是 tar 流的——所以**目录模式的 sha256 没法用
目标机 `sha256sum` 直接核对**，要用 `exec tar cf - /tmp/jwrd | sha256sum` 重算 tar 流，
或信 `n_entries` + 逐文件 `exec sha256sum`。

## 3. 看状态

```text
status           # hub 全局：agent 数 + 活跃 session 数
list_targets     # 每个 agent：id/hostname/platform/arch/sessions_active/busy
list_sessions    # 所有活跃 shell session：sid/op_session/state/idle
```

或本机命令行：

```bash
debugmcp-cli              # 人读总览
debugmcp-cli targets      # 目标机表
debugmcp-cli sessions     # session 表
debugmcp-cli --json       # 裸 JSON
```

## 常见陷阱

1. **`no target selected`**：没 `select_target` 也没传 `target` → 先 `select_target`。
2. **上传父目录不存在**：`exec mkdir -p /tmp/sub/dir` 再 upload，或把 `remote_path` 改到已存在目录。
3. **`.tar` 当目录传**：`.tar` 是文件，`is_dir: false`；要解包再 `exec tar xf`。
4. **目录 sha256 对不上**：目录模式的 sha256 是 tar 流的，不是文件内容的——别拿 `sha256sum <dir>` 比。
5. **大文件/大目录超时**：`exec` 有 `timeout_ms`，但 `upload`/`download` 是流式分块、无整体超时——大目录慢慢传，看 `duration_ms`。
6. **local/remote 搞反**：`local_path` 永远是**操作者本机**，`remote_path` 永远是**目标机**，方向由工具名（upload/download）决定。
7. **Windows 目标**：README 注明 Windows agent 的 ConPTY 未落地，`shell_*` 不可用（桩），`exec` 未充分验证。**优先选 Linux 目标**。
8. **shell 槽位满**：`shell_open` 返回 `{busy}` 说明该 target 的并发槽位满了——先 `shell_close` 释放，或换个 target。

## 典型工作流

```text
# 1. 选目标
list_targets
select_target { target: "web-01" }

# 2. 摸环境
exec { command: "uname -a; whoami; pwd" }
exec { command: "ls -la /opt/app" }

# 3. 传工具上去（单文件）
upload { local_path: "/home/demo/tools/scanner", remote_path: "/tmp/scanner", is_dir: false }
exec { command: "chmod +x /tmp/scanner && /tmp/scanner --version" }

# 4. 拉数据回来（目录）
exec { command: "tar czf /tmp/dump.tgz /var/log/app" }    # 先在目标机打包
download { remote_path: "/tmp/dump.tgz", local_path: "/home/demo/loot/dump.tgz", is_dir: false }

# 或直接拉整个目录（is_dir:true, 走 tar 流）
download { remote_path: "/var/log/app/", local_path: "/home/demo/loot/app-logs", is_dir: true }

# 5. 交互式调试（长生命周期进程）
shell_open { }
shell_send { sid: "...", input: "cd /opt/app && ./debug-repl\n" }
shell_read { sid: "..." }
signal { sid: "...", sig: "interrupt" }     # Ctrl-C
shell_close { sid: "..." }
```
