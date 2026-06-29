# SDK Spike: modelcontextprotocol/go-sdk v1.6.1 API 支持核实

调研对象：`github.com/modelcontextprotocol/go-sdk@v1.6.1`，包路径主要为 `github.com/modelcontextprotocol/go-sdk/mcp`。

核实方式：

- 先运行 `go env GOMODCACHE`，本机 module cache 为 `/home/demo/go/pkg/mod`。
- 本地原有 `github.com/modelcontextprotocol/go-sdk@v0.8.0`，无 v1.6.1。
- 在 `/tmp` 临时 module 中通过 socks5 代理拉取 `github.com/modelcontextprotocol/go-sdk@v1.6.1`。
- 直接阅读源码 `/home/demo/go/pkg/mod/github.com/modelcontextprotocol/go-sdk@v1.6.1/mcp/*.go`，并编译验证最小 server + tool + resource + progress 示例。

## 总览

| 能力 | 状态 | 结论 |
|---|---|---|
| stdio server | SUPPORTED | 没有 `server.FromStdio`；实际 API 是 `mcp.StdioTransport{}` + `(*mcp.Server).Run(ctx, transport)`。 |
| tools | SUPPORTED | 支持低层 `Server.AddTool` 和推荐的泛型顶层 `mcp.AddTool[In, Out]`；可返回 content blocks、structuredContent、isError。 |
| resources/list, resources/read | SUPPORTED | 支持 `Server.AddResource`、`Server.AddResourceTemplate`，handler 返回 `ReadResourceResult`。 |
| progress notification | SUPPORTED | 支持 `ServerSession.NotifyProgress`；请求参数类型提供 `GetProgressToken()` / `SetProgressToken()`，token 存在 `_meta.progressToken`。 |
| tool inputSchema 定义 | SUPPORTED | 泛型 `mcp.AddTool` 会从 Go struct + `json` / `jsonschema` tag 自动推导；低层 `Server.AddTool` 可直接传 `json.RawMessage` / map / `*jsonschema.Schema`。 |
| tools/list_changed 通知 | SUPPORTED | 没有 `server.SetTools` / `NotifyToolsChanged` 这种显式 API；`AddTool` / `RemoveTools` 自动 debounce 并发送 `notifications/tools/list_changed`。 |

## 1. stdio server

状态：SUPPORTED

源码证据：

- `/home/demo/go/pkg/mod/github.com/modelcontextprotocol/go-sdk@v1.6.1/mcp/transport.go`
  - `type StdioTransport struct{}`
  - `func (*StdioTransport) Connect(context.Context) (Connection, error)`
- `/home/demo/go/pkg/mod/github.com/modelcontextprotocol/go-sdk@v1.6.1/mcp/server.go`
  - `func (s *Server) Run(ctx context.Context, t Transport) error`

没有发现 `server.FromStdio` 命名。v1.6.1 的最小启动形态是：

```go
s := mcp.NewServer(&mcp.Implementation{Name: "debugmcp", Version: "v0.0.1"}, nil)
if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
    log.Fatal(err)
}
```

`Run` 的源码注释说明：它运行一个 persistent transport，阻塞直到 client 断开或 ctx 取消；适合单 session / 一次一个 session 的 server。

## 2. tools 定义、注册 handler、返回 content blocks + isError

状态：SUPPORTED

核心 API 形态：

```go
func AddTool[In, Out any](s *Server, t *Tool, h ToolHandlerFor[In, Out])

func (s *Server) AddTool(t *Tool, h ToolHandler)
func (s *Server) RemoveTools(names ...string)

type ToolHandler func(context.Context, *CallToolRequest) (*CallToolResult, error)

type ToolHandlerFor[In, Out any] func(
    ctx context.Context,
    request *CallToolRequest,
    input In,
) (result *CallToolResult, output Out, err error)
```

`Tool` 关键字段：

```go
type Tool struct {
    Name        string `json:"name"`
    Description string `json:"description,omitempty"`
    InputSchema any    `json:"inputSchema"`
    OutputSchema any   `json:"outputSchema,omitempty"`
    Title       string `json:"title,omitempty"`
    // ... annotations, icons, _meta
}
```

`CallToolResult` 关键字段：

```go
type CallToolResult struct {
    Content           []Content `json:"content"`
    StructuredContent any       `json:"structuredContent,omitempty"`
    IsError           bool      `json:"isError,omitempty"`
}
```

Content block 示例：

```go
&mcp.TextContent{Text: "hello"}
```

推荐路径是泛型 `mcp.AddTool`：

- 自动从 `In` 推导 input schema。
- 自动 unmarshal / validate `req.Params.Arguments`。
- 如果 `Out` 不是 `any`，自动推导 output schema，并填充 `StructuredContent`。
- handler 返回 `error` 时，会把错误转为 tool-level error：填充 `CallToolResult.Content` 并设置 `IsError`，而不是 MCP protocol error。

低层路径 `Server.AddTool` 适合自定义 schema / 自己做校验：

```go
server.AddTool(&mcp.Tool{
    Name:        "greet",
    InputSchema: json.RawMessage(`{"type":"object","properties":{"user":{"type":"string"}}}`),
}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
    return &mcp.CallToolResult{
        Content: []mcp.Content{&mcp.TextContent{Text: "Hi"}},
        IsError: false,
    }, nil
})
```

## 3. resources/list 与 resources/read

状态：SUPPORTED

核心 API 形态：

```go
func (s *Server) AddResource(r *Resource, h ResourceHandler)
func (s *Server) AddResourceTemplate(t *ResourceTemplate, h ResourceHandler)
func (s *Server) RemoveResources(uris ...string)
func (s *Server) RemoveResourceTemplates(uriTemplates ...string)

type ResourceHandler func(context.Context, *ReadResourceRequest) (*ReadResourceResult, error)
```

`Resource` 关键字段：

```go
type Resource struct {
    URI         string `json:"uri"`
    Name        string `json:"name"`
    Description string `json:"description,omitempty"`
    MIMEType    string `json:"mimeType,omitempty"`
    Size        int64  `json:"size,omitempty"`
    Title       string `json:"title,omitempty"`
}
```

`ResourceTemplate` 关键字段：

```go
type ResourceTemplate struct {
    URITemplate string `json:"uriTemplate"`
    Name        string `json:"name"`
    Description string `json:"description,omitempty"`
    MIMEType    string `json:"mimeType,omitempty"`
}
```

`ReadResourceResult` / `ResourceContents`：

```go
type ReadResourceResult struct {
    Contents []*ResourceContents `json:"contents"`
}

type ResourceContents struct {
    URI      string `json:"uri"`
    MIMEType string `json:"mimeType,omitempty"`
    Text     string `json:"text,omitempty"`
    Blob     []byte `json:"blob,omitzero"`
}
```

示例：

```go
s.AddResource(
    &mcp.Resource{URI: "debugmcp://hello", Name: "hello", MIMEType: "text/plain"},
    func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
        return &mcp.ReadResourceResult{
            Contents: []*mcp.ResourceContents{{
                URI:      req.Params.URI,
                MIMEType: "text/plain",
                Text:     "hello",
            }},
        }, nil
    },
)
```

不支持时的替代路径不需要启用；本 SDK 已支持 resources。若后续因客户端兼容性不想暴露 resources，可用普通 tool 代替，例如 `read_download_chunk` / `list_files` / `stat_file`。

## 4. progress notification

状态：SUPPORTED

核心 API 形态：

```go
func (ss *ServerSession) NotifyProgress(ctx context.Context, params *ProgressNotificationParams) error

type ProgressNotificationParams struct {
    ProgressToken any     `json:"progressToken"`
    Message       string  `json:"message,omitempty"`
    Progress      float64 `json:"progress"`
    Total         float64 `json:"total,omitempty"`
}
```

请求参数中有 progress token helper，例如：

```go
func (x *CallToolParamsRaw) GetProgressToken() any
func (x *CallToolParamsRaw) SetProgressToken(t any)
func (x *CallToolParams) GetProgressToken() any
func (x *CallToolParams) SetProgressToken(t any)
```

`CallToolRequest` 是：

```go
type CallToolRequest = ServerRequest[*CallToolParamsRaw]

type ServerRequest[P Params] struct {
    Session *ServerSession
    Params  P
    Extra   *RequestExtra
}
```

因此工具 handler 里可这样发送 progress：

```go
if tok := req.Params.GetProgressToken(); tok != nil {
    _ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
        ProgressToken: tok,
        Progress:      1,
        Total:         10,
        Message:       "started",
    })
}
```

源码中 `_meta.progressToken` 的 key 是 `progressToken`，存储在 `Meta map[string]any`。`setProgressToken` 只接受 `int` / `int32` / `int64` / `string`，但从 wire 读入时 `ProgressToken any` 可承载协议值。

如果未来使用的 MCP client 不显示 progress，替代实现路径：tool result 的 content 里携带阶段摘要，或将长任务拆成可轮询状态的 tool（例如 `start_task` + `get_task_status`）。

## 5. tool inputSchema 如何定义

状态：SUPPORTED

### 5.1 推荐：Go struct + tag 自动推导

使用顶层泛型 `mcp.AddTool[In, Out]`。`In` 的字段通过 `json` tag 控制属性名，`jsonschema` tag 用作描述文本。

注意：v1.6.1 使用 `github.com/google/jsonschema-go/jsonschema`，示例中 `jsonschema` tag 不是 `required,description=...` 这种格式，而是直接写描述：

```go
type EchoInput struct {
    Message string `json:"message" jsonschema:"message to echo"`
}
```

字段是否 required 通常由字段类型和 `omitempty` 控制。SDK 自带示例也使用如下形式：

```go
type args struct {
    Name string `json:"name" jsonschema:"the person to greet"`
}
```

### 5.2 低层：直接给 JSON schema

`Tool.InputSchema any` 可传任意能 marshal 成 JSON schema 的值，例如：

```go
InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
```

源码要求：

- `InputSchema` 非 nil。
- schema 顶层 `type` 必须是 `object`。
- `OutputSchema` 如果存在，顶层 `type` 也必须是 `object`。

## 6. tools/list_changed 通知如何触发

状态：SUPPORTED

没有发现以下 API：

- `server.SetTools`
- `NotifyToolsChanged`
- `server.WithTool`
- `mcp.NewServerTool`

实际 API 是：

```go
func (s *Server) AddTool(t *Tool, h ToolHandler)
func AddTool[In, Out any](s *Server, t *Tool, h ToolHandlerFor[In, Out])
func (s *Server) RemoveTools(names ...string)
```

`AddTool` / `RemoveTools` 内部调用 `changeAndNotify(notificationToolListChanged, ...)`：

- 有已连接 sessions 时，延迟 10ms debounce 后对所有 session 发送 `notifications/tools/list_changed`。
- 如果 `ServerOptions.Capabilities.Tools.ListChanged` 被显式设置为 `false`，通知会被抑制。
- 如果未显式设置 capabilities，默认会发送 list_changed 通知。

相关源码路径：

- `/home/demo/go/pkg/mod/github.com/modelcontextprotocol/go-sdk@v1.6.1/mcp/server.go`
  - `Server.AddTool`：约 line 238
  - `mcp.AddTool`：约 line 503
  - `Server.RemoveTools`：约 line 513
  - `changeAndNotify`：约 line 630
  - `shouldSendListChangedNotification`：约 line 661

## 最小 stdio server + 一个 tool 示例

该示例已在 `/tmp` 临时 module 中用 `go run .` 编译验证通过。

```go
package main

import (
    "context"
    "log"

    "github.com/modelcontextprotocol/go-sdk/mcp"
)

type EchoInput struct {
    Message string `json:"message" jsonschema:"message to echo"`
}

func main() {
    s := mcp.NewServer(&mcp.Implementation{
        Name:    "debugmcp",
        Version: "v0.0.1",
    }, nil)

    mcp.AddTool(s, &mcp.Tool{
        Name:        "echo",
        Description: "Echo a message",
    }, func(ctx context.Context, req *mcp.CallToolRequest, in EchoInput) (*mcp.CallToolResult, any, error) {
        // 可选：若 client 请求带了 _meta.progressToken，则发送 progress notification。
        if tok := req.Params.GetProgressToken(); tok != nil {
            _ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
                ProgressToken: tok,
                Progress:      1,
                Total:         1,
                Message:       "done",
            })
        }

        return &mcp.CallToolResult{
            Content: []mcp.Content{
                &mcp.TextContent{Text: in.Message},
            },
            IsError: false,
        }, nil, nil
    })

    if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
        log.Fatal(err)
    }
}
```

## 建议给主实现者的落地选择

1. 先使用 `mcp.NewServer(..., nil)` + `mcp.AddTool` + `s.Run(ctx, &mcp.StdioTransport{})` 实现最小 stdio server。
2. 工具输入优先用 struct，让 SDK 自动生成 schema 和做输入校验；需要精确 schema 时才退到 `Server.AddTool` + `json.RawMessage`。
3. 文件下载/读取能力既可做 resources，也可做 tools。考虑 Claude Code/MCP client 的兼容性，建议保留 tool 方案作为主路径（如 `read_download_chunk`），resources 可作为增强能力。
4. 长任务在 tool handler 中读取 `req.Params.GetProgressToken()`，有 token 才调用 `req.Session.NotifyProgress`；无 token 时不要发送 progress。
5. 动态增删工具时使用 `AddTool` / `RemoveTools`，通知会自动触发；无需寻找 `SetTools` / `NotifyToolsChanged`。
