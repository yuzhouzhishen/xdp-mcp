# 第 01 课复盘：启动、路由与请求进入链路

## 这份复盘的定位

这不是原始学习文档的重复版，而是结合你当前背景重新整理后的复盘稿。

目标不是把 Go 语法和第三方库源码全部吃透，而是做到：

- 知道这一层在系统里解决什么问题
- 能画出一条真实请求如何进入 tool handler
- 能用 C++ / 后端语言把它讲清楚
- 能切换成 AI Agent / MCP 的叙事方式

## 一句话总结

`main.go` 是 `xdp-mcp` 的服务组合层。它负责把配置、数据库鉴权、MQTT、遥测管理、MCP Server、HTTP 路由和优雅退出这些部分接起来，并把 URL 里的 `psn/token` 转成后续 tool handler 可读取的 `context` 信息。

## 这一层在系统里的位置

可以先把整套系统记成：

```text
MCP Client
-> HTTP 路由
-> 鉴权
-> PSN 注入 context
-> MCP Server
-> tool handler
-> MQTT / 数据库 / 遥测
```

这一课主要只看最外层四件事：

1. 服务怎么启动
2. 路由怎么分发
3. 设备身份怎么进入上下文
4. 请求怎么进入 MCP tool handler

## 这节的关键文件

- `main.go`
- `config.go`
- `auth.go`
- `skill.go`
- `handler.go`

第三方库 `mcp-go` 这次只当黑盒看输入输出，不作为主学习对象。

## main.go 到底负责什么

`main.go` 本质上不是业务实现层，而是“装配层”。

它主要做这些事：

1. 读取配置
2. 初始化数据库鉴权器
3. 初始化 MQTT 客户端
4. 初始化遥测管理器
5. 创建 MCP Server
6. 注册工具、Prompt、Resource
7. 同时暴露 SSE 和 Streamable HTTP 两种入口
8. 在路由层解析 `psn/token`
9. 把 `psn` 注入 request context 和 MCP context
10. 监听退出信号并关闭服务

如果用 C++ 来类比，它就像：

```cpp
int main() {
    Config cfg = loadConfig();
    Authenticator auth(cfg.db);
    MqttClient mqtt(cfg.mqtt);
    TelemetryManager telemetry(mqtt);
    McpServer server;
    registerTools(server, mqtt, auth, telemetry);
    HttpServer http(server);
    http.listen();
}
```

## 配置层要记什么

`config.go` 定义的不是业务状态，而是进程启动时要用的外部依赖配置。

重点字段：

- `listen_addr`
- `mqtt_broker_url`
- `mqtt_client_id`
- `mqtt_ca_file`
- `mqtt_cert_file`
- `mqtt_key_file`
- `database_url`
- `gateway_base_url`

一个值得记住的实际代码细节：

- 如果配置文件不存在，`main.go` 并不会直接退出
- 只有“文件存在但解析失败”时才退出

所以它对配置文件的处理是“可选读入”，不是绝对强依赖。

## 鉴权层在做什么

`auth.go` 里的 `Authenticator` 负责两件事：

1. 校验 `token` 是否和 `psn` 匹配
2. 校验数据库里是否真有这台设备

也就是说，安全边界不是单一的字符串比较，而是：

```text
token 对不对
+ psn 在数据库里存不存在
```

这里的 token 不是数据库查出来的，而是由：

```text
psn + 固定 salt
-> sha256
-> 取中间 8 位
```

推导出来的。

这个设计在面试里可以讲成：

> 设备访问身份由 URL 中的 `psn/token` 提供，服务端先做派生 token 校验，再做设备存在性校验，防止非法设备号或伪造路径直接进入工具层。

## 为什么要把 PSN 放进 context

这是这一课最重要的设计点之一。

系统并没有要求每个 tool 都显式传入：

```text
psn=12345678
```

而是走下面这条链路：

```text
URL 里带 /{psn}/{token}/...
-> 路由层解析
-> 鉴权通过
-> 把 psn 写入 context
-> tool handler 从 context 取 psn
```

好处：

- tool schema 更干净
- 模型不需要自己挑设备 ID
- 降低 LLM 误传设备标识的风险

代价：

- 路由层和 context 注入必须可靠
- 如果这一层没做好，handler 根本不知道自己在操作哪台设备

这套思路在后端里很常见，本质上类似：

- 用户身份写入 request context
- 租户信息写入 request context
- handler 后续直接从 context 取

## 根路由到底分了哪几类请求

`main.go` 的根路由只分这几类：

- `/{psn}/SKILL.md`
- `/{psn}/{token}/sse`
- `/{psn}/{token}/message`
- `/{psn}/{token}/mcp`

这里你只要记住：

- `SKILL.md` 是设备说明文档入口
- `sse` 和 `message` 是 SSE 模式
- `mcp` 是 Streamable HTTP 模式

其中 `extractFromPath()` 会把路径拆成：

```text
psn
token
subpath
```

后面的鉴权、路由分发、context 注入都依赖这个拆解结果。

## 一条真实请求是怎么走的

假设客户端发：

```text
POST /12345678/abcd1234/mcp
```

整体链路是：

```text
客户端请求
-> rootHandler
-> extractFromPath 拆路径
-> auth.Validate 校验 psn/token
-> 把 psn 写入 r.Context()
-> 把 r.URL.Path 改成 /mcp
-> streamServer.ServeHTTP
-> WithHTTPContextFunc 把 psn 注入 MCP context
-> MCPServer.HandleMessage
-> tools/call 分发到对应 handler
-> handler 从 context 取 psn
-> 调 MQTT 或数据库
-> 返回 MCP ToolResult
```

这条链路是这节最该背的东西。

## initialize 和 tools/call 要分开理解

### initialize 请求

如果 POST 的 JSON-RPC 方法是 `initialize`：

- `mcp-go` 会生成一个新的 `sessionID`
- 把这个 `sessionID` 放在响应头里返回给客户端
- 初始化成功后，会注册 session

这意味着：

```text
第一次 initialize
-> 服务端发 sessionID
```

### 后续 tools/call 请求

后续工具调用时：

- 客户端需要带回 `sessionID`
- 服务端会校验这个 sessionID
- 然后再继续处理工具调用

所以：

```text
先 initialize
再 tools/call
```

这和很多 RPC / 会话式协议的思路是一样的。

## session hook 在这节里起什么作用

`main.go` 注册了 session hook。

会话建立时：

- 从 context 中取出 `psn`
- 保存 `sessionID -> psn`
- 启动这台设备的遥测流

会话结束时：

- 根据 `sessionID` 找到 `psn`
- 停掉遥测流
- 删除映射

所以 session hook 不是在决定“这次请求打哪台设备”，而是在管理“这个会话和哪台设备绑定，以及会话级资源怎么处理”。

要把两件事分开：

1. 请求级身份传递：靠 context 注入
2. 会话级设备绑定：靠 session hook

## SSE 和 Streamable HTTP 到底差在哪

### SSE 模式

- 有 `/sse` 建立事件流
- 有 `/message` 发消息
- 适合长连接事件流式交互

### Streamable HTTP 模式

- 统一走 `/mcp`
- 主要用 POST 发送 JSON-RPC
- 更适合新式 MCP 客户端

面试里可以这样说：

> 这个服务同时支持 SSE 和 Streamable HTTP 两种 MCP transport，让同一套设备工具既能被传统桌面 MCP 客户端使用，也能被新式 HTTP 风格客户端使用。

## 关于第三方库，这节真正需要知道什么

这节不需要深入 `mcp-go` 的所有内部细节。

你只要知道：

- `StreamableHTTPServer` 是一个 `http.Handler`
- 它接收 `/mcp` 请求
- 它会把 JSON-RPC 消息交给 `MCPServer.HandleMessage`
- `MCPServer` 再按工具名分发到注册好的 handler

这就够了。

不要在这个阶段把时间花在：

- `mcp-go` 的所有 session 管理细节
- 所有 capability 结构
- 所有测试用例

## 这节里一个容易讲错的点

`main.go` 里有：

```go
server.WithToolCapabilities(false)
server.WithPromptCapabilities(false)
server.WithResourceCapabilities(false, false)
```

这里的 `false` 不是“关闭工具 / prompt / resource”。

更准确地说，它是在配置这些 capability 里的某些子标志，比如 `listChanged` 或 `subscribe`。因为后面代码马上就注册了工具，所以绝对不能讲成“这里把工具功能关了”。

## SKILL.md 为什么不需要 token

`/{psn}/SKILL.md` 这条路由不走 token 鉴权。

原因是：

- 它不返回实时状态
- 不做设备控制
- 它只是返回设备说明文档

但要注意一个细节：

- 它仍然需要 `psn`
- 而且会先查数据库里的 `device_model`
- 再根据型号返回不同的 `SKILL.md`

所以它不是纯静态文件，而是“按设备型号动态选说明文档”。

## C++ / 后端类比讲法

如果按你熟悉的方式理解，这一层可以类比成：

- `main.go`：服务入口和依赖装配
- `auth.go`：鉴权中间件 / 认证服务
- `rootHandler`：总路由
- `ctxKeyPSN`：请求上下文字段
- `session hook`：会话生命周期回调
- `handler.go` 注册的 tool：RPC 方法注册表
- `mqtt.go`：下游设备网关客户端

一句话总结：

> 这层本质上就是一个带设备身份注入的服务端入口层，只不过它服务的不是普通 Web API，而是 MCP 工具调用。

## AI Agent / MCP 讲法

如果按 AI Agent 项目来讲，这一层的价值是：

- 把真实设备能力包成 MCP 可访问服务
- 对模型隐藏底层设备标识细节
- 用 `psn/token + context` 做设备隔离
- 让同一套设备工具能被不同 MCP 客户端调用

一句话总结：

> 这一层相当于设备能力的 MCP 暴露层，负责把真实设备链路安全地包装成 LLM 可以调用的工具入口。

## 面试时可以怎么讲

### 后端版 30 秒

`main.go` 是整个 `xdp-mcp` 的服务组合层，负责配置加载、数据库鉴权、MQTT 初始化、MCP Server 创建和 HTTP 路由分发。请求进入时先通过 `psn/token` 做设备鉴权，再把 `psn` 注入 context，后续 tool handler 根据 context 操作对应设备。

### AI Agent 版 30 秒

`xdp-mcp` 这一层把真实充电设备的查询和控制能力暴露成 MCP 工具。`main.go` 负责把路由、鉴权、设备会话和 MCP transport 接起来，让 Claude、Codex 这类客户端能安全地访问具体设备，而不用让模型自己显式传设备 ID。

### 1 分钟版

这一层本质上是服务入口层。它先读取配置，初始化数据库鉴权器和 MQTT 客户端，再创建 MCP Server 并注册 tools、prompts、resources。路由层把 URL 里的 `psn/token` 解析出来，鉴权通过后把 `psn` 注入 context，再把请求交给 MCP Server。对于 `initialize` 请求，会生成并返回 sessionID；后续 `tools/call` 请求则带着 sessionID 进来，最后按工具名分发到对应 handler，由 handler 通过 MQTT 或数据库访问真实设备。这个设计的关键点是把设备身份从工具参数里剥离出来，让工具 schema 更干净，也降低模型误操作设备的风险。

## 面试高频问题

### 1. `main.go` 在这个项目里到底负责什么？

它不写具体业务逻辑，而是做服务装配：加载配置、初始化数据库和 MQTT、创建 MCP Server、注册工具、挂 HTTP 路由，并负责优雅退出。

### 2. 为什么 URL 里既有 `psn` 又有 `token`？

`psn` 用来标识设备，`token` 用来证明这个设备访问是合法的。服务端会同时校验 token 是否匹配，以及数据库里是否存在这个设备。

### 3. 为什么不把 `psn` 作为每个 tool 的参数？

因为设备身份是请求上下文，不是自然语言层该显式填写的业务参数。放进 context 后，tool schema 更干净，也更能减少模型传错设备 ID 的风险。

### 4. 一条 `/mcp` 请求怎么走到 tool handler？

先在根路由里解析 `psn/token` 并做鉴权，再把 `psn` 写入 request context，之后交给 `StreamableHTTPServer`。它再把 context 传进 `MCPServer.HandleMessage`，如果是 `tools/call`，就按工具名找到对应 handler。handler 再从 context 取 `psn`，最终调 MQTT 或数据库。

### 5. `initialize` 和后续 `tools/call` 有什么区别？

`initialize` 用于建立会话，服务端会生成并返回 `sessionID`；后续 `tools/call` 则使用这个 `sessionID` 继续调用工具。

### 6. session hook 做了什么？

它做的是会话级资源管理：会话建立时记录 `sessionID -> psn` 并启动遥测流，会话结束时停止遥测流并清理映射。

### 7. SSE 和 Streamable HTTP 为什么都要支持？

因为不同 MCP 客户端对 transport 的支持方式不同。支持两种 transport，可以让同一套设备工具被更多 MCP 客户端复用。

### 8. `SKILL.md` 为什么不需要鉴权？

因为它只返回设备说明，不读实时数据，也不执行控制操作，所以安全风险较低。但它仍然要根据 `psn` 查数据库里的设备型号，再返回对应文档。

### 9. 这一层更像后端，还是更像 AI Agent 项目？

两者都有。底层看，它有路由、鉴权、上下文注入、会话和异步下游调用，明显是服务端系统；对外看，它又是在把设备能力包装成 MCP 工具给 LLM 使用，所以也很适合按 AI Agent 基础设施层去讲。

## 你现在最该记住的 5 个点

1. `main.go` 是组合层，不是业务实现层。
2. 请求进入后，先鉴权，再把 `psn` 注入 context。
3. tool handler 不显式收 `psn`，而是从 context 里取。
4. `initialize` 返回 `sessionID`，后续 `tools/call` 依赖这个会话继续调用。
5. 这一层既能按后端讲，也能按 AI Agent / MCP 讲。

## 对你当前阶段的学习要求

这一课你不需要做到：

- 读懂全部 `mcp-go` 源码
- 记住所有 Go 语法细节
- 弄清第三方库所有 session 管理实现

这一课你需要做到：

- 能画出 `/mcp` 请求流转图
- 能解释为什么要 context 注入 `psn`
- 能说清 `initialize` 和 `tools/call` 的关系
- 能分别用“后端版”和“AI Agent 版”说 30 秒

## 下一课衔接

第 02 课再往里走一步，不再看“请求怎么进入”，而是看：

- tool 是怎么注册的
- schema 是怎么定义的
- 查询类和控制类工具是怎么分的
- 为什么一个工具看起来像普通函数，背后却能调真实设备
