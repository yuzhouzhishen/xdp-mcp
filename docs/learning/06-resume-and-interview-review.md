# 第 06 课：简历项目与面试总复习

## 一句话总结

这个项目应该被定位为“面向智能设备的 MCP Server / Agent 工具网关”，核心价值是把真实设备的查询、诊断、控制能力封装成 LLM 可调用工具，而不是泛泛地说“做了一个 AI 项目”。

## 项目一句话介绍

推荐版本：

> 这是一个面向 IonBridge 智能充电设备的 MCP Server，用 Go 实现。它把设备查询、端口控制、充电策略配置、USB-C/PD 诊断和遥测状态封装为 LLM 可调用工具，使 Claude、Cursor、Codex 等 MCP 客户端可以通过自然语言访问真实设备。

更短版本：

> 这个项目是智能充电设备的 Agent 工具网关，把底层 MQTT/Protobuf 设备协议封装成 MCP Tools，供 LLM 客户端查询、诊断和控制真实设备。

## 推荐简历写法

```text
智能设备 Agent 工具网关
Go / MCP / MQTT / Protobuf / SSE / Streamable HTTP

基于 MCP 协议实现智能充电设备的 Agent 工具接入层，将设备查询、端口控制、充电策略配置和设备诊断能力封装为 LLM 可调用工具。

• 基于 mcp-go 实现 MCP Server，支持 SSE 与 Streamable HTTP 两种传输方式，适配 Claude、Cursor、Codex 等 MCP 客户端。
• 封装设备信息查询、端口状态查询、充电策略切换、温度模式设置、端口开关、功率分配、PD 协议诊断等设备工具能力。
• 通过 MQTT/Protobuf 与设备侧通信，基于 request id 和 pending map 实现异步请求-响应匹配，并处理超时、重试和设备错误返回。
• 基于 PSN/TOKEN 完成设备访问鉴权，并通过 context 注入设备身份，避免 LLM 工具调用时显式传递设备序列号。
• 引入遥测流管理模块，按设备缓存最新端口状态和设备状态，优先复用短时间内的实时数据，降低高频查询延迟。

关键词：MCP、Agent Tool、Tool Calling、MQTT、Protobuf、SSE、Streamable HTTP、Telemetry
```

## 不建议这样写

不要写：

```text
训练大模型
实现大模型智能体
自研 Agent 框架
实现模型推理能力
实现多智能体协作
```

原因：

- 项目没有训练模型。
- 项目重点不是模型推理，而是工具服务端。
- 没有完整多 Agent 协作框架。
- 说得太大，面试官一问细节容易露怯。

更稳的定位是：

```text
MCP Server
Agent 工具层
Tool Calling 服务端
智能设备工具网关
真实设备控制与诊断工具封装
```

## 项目架构总图

```text
Claude / Cursor / Codex
  |
  | MCP: SSE / Streamable HTTP
  v
xdp-mcp
  |
  | main.go
  | - 路由 /{PSN}/{TOKEN}/sse
  | - 路由 /{PSN}/{TOKEN}/mcp
  | - session hook
  |
  | handler.go
  | - 注册 MCP Tools
  | - 参数 schema
  | - Prompt / Resource
  |
  | auth.go / skill.go
  | - PSN/TOKEN 鉴权
  | - SKILL.md 设备说明
  |
  | telemetry.go
  | - 按 PSN 维护遥测流
  | - 缓存最新端口/设备状态
  |
  | mqtt.go
  | - MQTT 发布/订阅
  | - Protobuf 编解码
  | - request id + pending map
  v
IoT 设备 / MQTT Broker
```

## 模块复习

### 1. `main.go`

你要能说：

- 服务启动时加载配置。
- 初始化数据库鉴权、MQTT、TelemetryManager。
- 创建 MCP Server。
- 注册工具和 prompt/resource。
- 暴露 SSE 和 Streamable HTTP 两种 transport。
- URL 里带 PSN/TOKEN，鉴权通过后把 PSN 注入 context。

面试关键词：

```text
服务组合层、MCP transport、路由鉴权、context 注入、session hook、优雅退出
```

### 2. `handler.go`

你要能说：

- 这里注册模型能看到的 MCP 工具。
- 工具分为查询、诊断、控制、显示配置几类。
- 每个 tool 有 description、参数 schema 和 handler。
- handler 从 context 取 PSN，再调用 MQTT 或遥测缓存。
- Prompt/Resource 用来指导客户端执行常见任务流程。

面试关键词：

```text
Tool schema、Tool handler、Prompt、Resource、参数校验、工具抽象
```

### 3. `mqtt.go`

你要能说：

- MCP tool call 对上层像同步函数调用。
- 设备通信底层是 MQTT 异步消息。
- `SendCommand` 用 request id、pending map、channel 把两者接起来。
- 设备响应回来后，通过 `resp.Id` 找到等待的请求。
- 支持 8 秒超时、最多 2 次尝试、QoS1 发布。

面试关键词：

```text
MQTT、Protobuf、request id、pending map、channel、超时、重试、Correlation ID
```

### 4. `telemetry.go`

你要能说：

- 每个 PSN 对应一个遥测流。
- 服务端订阅端口状态和设备状态 topic。
- 收到 stream 数据后缓存最新快照和更新时间。
- 查询类工具优先读 30 秒内缓存。
- 缓存过期时 Kick 设备重新上报。

面试关键词：

```text
遥测流、实时快照、缓存新鲜度、按设备隔离、降低查询延迟
```

### 5. `auth.go` / `skill.go`

你要能说：

- token 根据 PSN + salt 做 SHA-256 后取中间 8 位。
- 使用常量时间比较校验 token。
- 查询数据库确认 PSN 存在。
- 鉴权通过后把设备身份注入 context。
- `SKILL.md` 按设备型号返回 LLM 使用说明。

面试关键词：

```text
PSN/TOKEN、常量时间比较、设备存在性校验、context 设备隔离、SKILL.md
```

## 高频面试问答

### Q1：这个项目和普通大模型聊天项目有什么区别？

普通聊天项目通常是：

```text
用户输入 -> 调模型 API -> 返回文本
```

这个项目是：

```text
用户自然语言 -> LLM 选择 MCP Tool -> xdp-mcp 调真实设备协议 -> 返回设备状态/执行控制
```

它更接近 Agent 应用里的工具调用层，重点是把外部系统能力封装成模型可调用、可约束、可追踪的工具。

### Q2：MCP 在这个项目里解决什么问题？

MCP 提供了一个标准方式，让 Claude、Cursor、Codex 等客户端发现并调用外部工具。这个项目通过 MCP 把智能充电设备能力暴露出去，客户端不用了解 MQTT、Protobuf 或设备协议，只需要调用标准 tool。

### Q3：为什么同时支持 SSE 和 Streamable HTTP？

因为不同 MCP 客户端支持的 transport 不一样。SSE 兼容 Claude Desktop、Cursor 等客户端；Streamable HTTP 适合 Codex 等新式客户端。服务端支持两种 transport，可以让同一套工具被更多客户端使用。

### Q4：工具调用如何变成设备控制？

以关闭端口为例：

```text
LLM 调用 turn_off_port
  -> handler 校验 ports 参数
  -> BuildTurnOffPort 构造 Protobuf 请求
  -> SendCommand 发布到 MQTT request topic
  -> 设备处理后返回 CommandResponse
  -> pending map 根据 request id 匹配响应
  -> handler 返回结果给 MCP 客户端
```

### Q5：为什么需要 pending map？

因为 MQTT 是异步消息模型，响应不是函数返回值。同一个 response topic 上也可能有多个请求的响应，所以需要 request id 做关联。pending map 保存正在等待响应的请求，响应回来后用 `resp.Id` 找到对应 channel。

### Q6：遥测缓存有什么价值？

Agent 可能连续多次查询设备状态。如果每次都发命令到设备，延迟高、通信压力大。遥测缓存保存最新端口/设备状态，查询类工具优先复用 30 秒内快照，使状态查询更快。

### Q7：这个项目怎么保证不会操作错设备？

设备 PSN 来自 URL，不从工具参数里传。请求先通过 PSN/TOKEN 鉴权，鉴权通过后服务端把 PSN 注入 context，handler 只能从 context 取当前设备身份。这样 LLM 不需要也不能在工具参数里随意切换设备。

### Q8：你觉得这个项目还可以怎么改进？

可以从四个方向说：

- 安全：高风险控制操作增加用户确认、审计日志、短期 token。
- 稳定性：更细的错误分类、断线重连后的状态恢复。
- 可观测性：记录 tool call、MQTT 延迟、设备响应耗时。
- Agent 体验：增加更高层的诊断工具，例如“一键分析充电慢原因”。

## 一分钟项目介绍

> 我做的是一个面向智能充电设备的 MCP Server，可以理解为 Agent 工具网关。上层支持 Claude、Cursor、Codex 这类 MCP 客户端，通过 SSE 或 Streamable HTTP 连接进来；服务端把设备查询、端口控制、充电策略、USB-C/PD 诊断等能力注册成 MCP Tools。工具执行时，handler 从 context 获取当前设备 PSN，再通过 MQTT/Protobuf 向设备侧发送命令。因为设备通信是异步的，所以我用 request id、pending map 和 channel 做请求-响应匹配，并加了超时和重试。对于高频状态查询，则通过 telemetry 模块按设备缓存最新端口和设备状态，减少重复查询延迟。

## 三分钟项目介绍

> 这个项目的背景是我们有一类智能多端口充电设备，底层通过 MQTT 和 Protobuf 与云端/设备侧通信。为了让大模型客户端能查询和控制设备，我们做了一个 MCP Server，把设备能力抽象成 LLM 可调用工具。
>
> 服务启动时会初始化数据库鉴权、MQTT 连接和遥测管理器，然后创建 MCP Server，并同时提供 SSE 和 Streamable HTTP 两种 transport。设备访问路径里包含 PSN 和 TOKEN，鉴权通过后会把 PSN 注入 context，后续工具不需要也不能显式传设备 ID。
>
> 工具层主要在 handler.go，分为设备查询、端口状态、充电策略、端口开关、功率分配、USB-C/PD 诊断、显示配置等几类。每个工具都有明确的 schema 和 handler。控制类工具会构造 Protobuf command，通过 MQTT 发到设备 request topic；设备响应回来后，mqtt.go 用 request id 在 pending map 里找到对应请求，再把响应交回 handler。
>
> 另外，为了提升查询体验，telemetry.go 会按 PSN 维护设备遥测流，订阅端口状态和设备状态 topic，缓存最新快照。像 get_port_details 这种查询会优先使用 30 秒内的缓存，过期才触发设备重新上报。
>
> 所以这个项目的核心不是训练模型，而是把真实设备系统封装成 Agent 可用的工具层，解决了 MCP 工具 schema、设备鉴权、异步通信匹配、遥测缓存和真实设备控制这些工程问题。

## 你现在必须能画出来的链路

### 查询端口状态

```text
get_port_details
  -> psnFromContext
  -> telemetry.Latest(psn)
  -> 如果 30 秒内有效，直接返回
  -> 否则 telemetry.Kick(psn)
  -> 等待新遥测
  -> 格式化 JSON 返回
```

### 关闭端口

```text
turn_off_port
  -> psnFromContext
  -> parsePortsArg
  -> BuildTurnOffPort
  -> mqtt.SendCommand
  -> publish MQTT request
  -> pending map 等 response
  -> 检查 resp.Status
  -> 返回结果
```

### 深度诊断

```text
device_diagnostic prompt
  -> get_machine_facts
  -> get_device_info
  -> get_port_details
  -> get_port_pd_status
  -> 分析端口、协议、线缆、电池、温度、能力不匹配
  -> 给出建议
```

## 学习优先级

如果时间很紧，按这个顺序背：

1. 项目一句话介绍
2. MCP 是什么
3. `handler.go` 工具注册
4. `mqtt.go` 的 request id + pending map
5. `telemetry.go` 的 30 秒缓存
6. PSN/TOKEN/context 设备隔离

如果只能讲一个亮点，就讲：

> MCP 工具调用和 MQTT 异步设备协议之间的桥接：用 request id、pending map、channel、timeout/retry 把异步响应包装成 tool handler 可等待的结果。

## 最终自测清单

- 我能不能用一分钟讲清楚项目？
- 我能不能画出 MCP Client 到设备的完整链路？
- 我能不能解释 MCP Tool、Prompt、Resource、SKILL.md 的区别？
- 我能不能讲清楚 `SendCommand` 怎么匹配响应？
- 我能不能讲清楚遥测缓存为什么存在？
- 我能不能说明 PSN/TOKEN/context 如何避免操作错设备？
- 我能不能说出项目不足和改进方向？
- 我能不能把这个项目和“普通大模型聊天 SDK”区分开？

