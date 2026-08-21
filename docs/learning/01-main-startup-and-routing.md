# 第 01 课：`main.go` 启动与路由链路

## 一句话总结

`main.go` 是 `xdp-mcp` 的进程入口。它负责加载配置、连接数据库和 MQTT、创建 MCP Server、注册工具、同时暴露 SSE 和 Streamable HTTP 两种 MCP 传输方式，并在进程退出时做清理。

## 这部分在系统里的位置

`xdp-mcp` 位于 LLM 客户端和真实 IoT 设备系统之间：

```text
Claude / Cursor / Codex
  -> MCP 传输层：SSE 或 Streamable HTTP
  -> xdp-mcp HTTP 服务
  -> MCP tool handler
  -> MQTT + Protobuf
  -> 设备 / IoT 网关
```

这一课只看最外层：服务如何启动，以及一个请求如何进入 MCP Server。

## 关键代码路径

1. 加载配置。
   - `main.go:22-33`
   - `config.go:14-23`
   - 配置里包括监听地址、MQTT Broker、证书路径、数据库地址等。

2. 创建根 `context`。
   - `main.go:35-36`
   - 可以理解成整个进程的取消信号。服务退出时调用 `cancel()`，后台任务可以收到退出通知。

3. 初始化鉴权和 MQTT。
   - `main.go:38-50`
   - `NewAuthenticator` 连接 PostgreSQL。
   - `NewMQTTClient` 连接 MQTT Broker，并准备后续发布、订阅和响应分发。

4. 创建会话和遥测状态。
   - `main.go:52-56`
   - `sessionPSN` 保存 MCP session ID 到设备 PSN 的映射。
   - `TelemetryManager` 维护设备遥测流，并缓存最新端口状态和设备状态。

5. 注册 MCP 会话 hook。
   - `main.go:58-79`
   - 会话建立时，从 `context` 取出 PSN，记录 `sessionID -> psn`，并启动该设备的遥测流。
   - 会话结束时，停止遥测流并删除会话状态。

6. 创建 MCP Server。
   - `main.go:81-89`
   - Server 名称是 `ionbridge-mcp`，版本是 `1.0.0`。
   - 前面定义的 hook 在这里挂到 MCP Server 上。

7. 注册工具、Prompt 和 Resource。
   - `main.go:91-93`
   - 入口是 `registerAllToolsWithContext(...)`。
   - 具体工具在 `handler.go` 里注册。

8. 暴露两种 MCP 传输方式。
   - SSE：`main.go:95-122`
   - Streamable HTTP：`main.go:124-135`
   - SSE 适合 Claude Desktop、Cursor 等客户端。
   - Streamable HTTP 适合 Codex 等新式 MCP 客户端。

9. 自定义 HTTP 路由。
   - `main.go:137-179`
   - `/{psn}/SKILL.md`：返回设备使用说明，不需要 token。
   - `/{psn}/{token}/sse`：建立 SSE 连接。
   - `/{psn}/{token}/message`：SSE 模式下发送 JSON-RPC 消息。
   - `/{psn}/{token}/mcp`：Streamable HTTP 模式下发送 JSON-RPC 消息。

10. 优雅退出。
    - `main.go:186-202`
    - 监听 `SIGINT` 和 `SIGTERM`，收到信号后取消根 context，关闭 MCP transport 和 HTTP server。

## C++ 类比

可以把 `main.go` 理解成 C++ 服务里的 `main.cpp`：

```text
Config cfg;
DatabaseAuth auth(cfg.database_url);
MqttClient mqtt(cfg.mqtt_url);
TelemetryManager telemetry(mqtt);
McpServer server;
registerTools(server, mqtt, auth, telemetry);
HttpServer http(rootRouter);
http.listen();
```

Go 里的几个概念可以这样类比：

- `context.Context`：类似“取消令牌 + 请求上下文数据”。
- `defer auth.Close()`：类似 RAII 清理，但 Go 是函数作用域内显式声明。
- `sync.Map`：并发安全的 map，用来保存 `sessionID -> psn`。
- `go func() { ... }()`：启动一个轻量级后台协程，类似启动线程但成本更低。
- `http.HandlerFunc`：HTTP 请求回调函数。

## 为什么要把 PSN 放进 context

MCP 工具需要知道自己在操作哪台物理设备。但这个项目没有让每个工具都显式传 `psn` 参数，而是用下面这条链路：

1. URL 里带 `/{psn}/{token}/...`
2. 路由层和鉴权层解析 PSN/TOKEN
3. PSN 被写入请求 `context`
4. handler 从 `context` 中读取 PSN

这样 LLM 看到的工具更干净：

```text
get_port_details()
```

而不是：

```text
get_port_details(psn=123456789)
```

好处是模型不需要自己选择设备 ID，降低误操作风险。代价是路由和 context 注入必须正确，否则工具 handler 就不知道操作哪台设备。

## SSE 和 Streamable HTTP 的区别

SSE 模式：

- 入口：`/{PSN}/{TOKEN}/sse`
- 消息入口：`/{PSN}/{TOKEN}/message`
- 客户端维持一条长连接事件流
- 适合 Claude Desktop、Cursor 等客户端

Streamable HTTP 模式：

- 入口：`/{PSN}/{TOKEN}/mcp`
- 单个 POST endpoint 处理 JSON-RPC 消息
- 适合 Codex 这类客户端

面试表达：

> 这个服务同时支持 SSE 和 Streamable HTTP 两种 MCP transport，因此同一套设备工具可以被 Claude Desktop、Cursor、Codex 等不同客户端使用。

## 这个文件里的安全边界

设备访问 URL 里包含两个关键字段：

- `PSN`：设备序列号
- `TOKEN`：访问 token

对于 SSE 和 Streamable HTTP 请求，`auth.Validate` 会先校验 token，并确认设备存在，之后请求才会进入 MCP transport。

一个细节：

- `/{psn}/SKILL.md` 不需要 token。
- 因为它只返回设备使用说明，不返回实时数据，也不允许控制设备。

## 面试表达

短版本：

> `main.go` 是服务组合层。它初始化配置、数据库鉴权、MQTT、遥测管理器，创建 MCP Server，注册工具，并暴露 SSE 和 Streamable HTTP 两种路由。每个设备访问 URL 都携带 PSN/TOKEN，服务校验后把 PSN 注入 context，后续工具 handler 再根据 context 操作对应设备。

更强一点的版本：

> 这个设计把设备身份从工具参数里剥离出来。LLM 看到的工具 schema 更简单，而 HTTP 路由和 context 注入负责把会话绑定到具体设备。这样可以减少模型误传设备 ID 的风险，但也要求鉴权和 context 传播链路必须可靠。

## 可能被追问

1. `main.go` 在这个 MCP Server 里负责什么？
2. 为什么 URL 里既有 PSN 又有 TOKEN？
3. 为什么 PSN 放在 `context.Context` 里，而不是每个 tool 都传参数？
4. SSE 和 Streamable HTTP 的区别是什么？
5. 客户端 session 建立时发生了什么？
6. 进程收到 `SIGINT` 或 `SIGTERM` 时会做什么？
7. MCP 工具在哪里注册？
8. 为什么 `SKILL.md` 不需要 token？

## 练习答案

**Q：为什么用 context 保存 PSN？**

因为 PSN 是请求/会话身份，不是用户自然语言里应该填写的业务参数。放到 context 里可以让工具 schema 更干净，也能降低模型混淆设备 ID 的风险。

**Q：什么时候启动遥测流？**

当 MCP session 注册时，hook 从 context 读取 PSN，记录 session 映射，并调用 `telemetry.Start(...)` 启动对应设备的遥测流。

**Q：优雅退出保护了什么？**

它会取消后台任务、关闭 MCP transport、关闭 HTTP server，避免进程退出时留下未清理的网络连接或长连接会话。

## 简历候选写法

> 参与 Go 实现的 MCP Server 启动与路由链路梳理，支持 SSE 与 Streamable HTTP 两种 MCP 传输方式；通过 PSN/TOKEN 鉴权和 context 注入完成设备会话隔离，并在 MCP 会话建立时启动设备遥测流。

这句话适合放进学习总结或面试话术里。简历正文可以再压缩，不一定要写得这么长。

## 复习检查

- 能不能画出 `/{psn}/{token}/mcp` 到 `streamServer.ServeHTTP` 的路径？
- 能不能解释 `ctxKeyPSN` 为什么存在？
- 能不能解释为什么支持两种 MCP transport？
- 能不能说清楚 session hook 做了什么？
- 能不能把 auth、routing、MCP、MQTT、telemetry 分别放到正确位置？

