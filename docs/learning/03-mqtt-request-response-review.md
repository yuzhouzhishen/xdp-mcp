# 第 03 课复盘：MQTT 请求-响应链路与命令下发

## 这份复盘的定位

这份文档不是让你去硬啃 `autopaho` 或 `paho` 的库实现，而是帮你把 `mqtt.go` 这一层讲清楚。

目标是做到：

- 知道 `mqtt.go` 在系统里解决什么问题
- 能画出一次工具调用如何真正到达设备
- 理解 `request id + pending map + channel` 为什么成立
- 能用后端语言和 AI Agent 语言分别解释这层设计

## 一句话总结

`mqtt.go` 是 `xdp-mcp` 的设备通信层。它把上层 handler 构造出的 Protobuf 命令发布到 MQTT request topic，再通过 `request id + pending map + channel` 等机制，把设备异步返回的响应重新交还给对应的工具调用。

## 这一层在系统里的位置

把前面几课接起来看：

```text
MCP Client
-> Tool handler
-> BuildXXXCommand(...)
-> mqtt.go / SendCommand(...)
-> MQTT request topic
-> 真实设备
-> MQTT response topic
-> OnPublishReceived
-> pending map 按 id 找到等待方
-> handler 拿到 CommandResponse
```

如果说：

- `01` 解决“请求怎么进来”
- `02` 解决“模型能调用什么工具”

那 `03` 解决的就是：

> 工具执行后，命令如何真正发到设备侧，以及异步响应如何再回到当前这次调用。

## 这节的关键文件

- `mqtt.go`
- `handler.go`
- `telemetry.go`
- `proto/xdp/v1/*.proto`

主战场是 `mqtt.go`，`handler.go` 和 `telemetry.go` 只是它的调用方。

## 为什么这层一定要存在

上层 MCP Tool 的调用方式，看起来像同步函数：

```text
调用工具
-> 等结果
-> 返回给模型
```

但设备通信底层不是函数调用，而是 MQTT 异步消息：

```text
发布 request 消息
-> 设备稍后处理
-> 再从 response topic 回消息
```

所以 `mqtt.go` 的本质工作就是：

> 把“异步消息通信”包装成“上层 handler 看起来像同步 RPC 调用”的形式。

这就是 `SendCommand(...)` 的价值。

## 核心数据结构

### `MQTTClient`

```go
type MQTTClient struct {
    conn   *autopaho.ConnectionManager
    router *paho.StandardRouter
    idSeq  atomic.Uint32

    mu       sync.Mutex
    pending  map[uint32]*pendingRequest
    subCount map[string]int
}
```

不要逐字段背代码，记住它们的职责：

- `conn`：MQTT 连接管理器，负责连接、发布、订阅
- `router`：把收到的 MQTT 消息再路由给别的模块，遥测会用到
- `idSeq`：生成递增的请求 id
- `pending`：保存“当前还在等响应的请求”
- `subCount`：记录某个 topic 当前被订阅了几次，避免重复退订

### `pendingRequest`

```go
type pendingRequest struct {
    id uint32
    ch chan *pb.CommandResponse
}
```

可以把它理解成：

- 这次请求的唯一编号
- 这次请求专属的“收结果通道”

设备响应回来后，系统会按 `resp.Id` 找到对应 `pendingRequest`，再把结果塞进这个 channel。

## Topic 设计怎么记

`mqtt.go` 里最重要的两个模板是：

```go
endUserRequestTopicTpl  = "device/%d/enduser/request/%d"
endUserResponseTopicTpl = "device/%d/enduser/response/%d"
```

两个 `%d` 分别表示：

- 第一个：设备 `psn`
- 第二个：`serviceID`

例如：

```text
device/123456/enduser/request/76
device/123456/enduser/response/76
```

这里的 `76` 对应某个具体设备服务，比如端口打开命令。

所以这个项目里，topic 不是泛泛地按“设备”分，而是按：

```text
设备 + 指令类别(serviceID)
```

来组织的。

## Service ID 是什么

像这些常量：

```go
ServiceTurnOnPort              uint8 = 0x4c
ServiceTurnOffPort             uint8 = 0x4d
ServiceSetChargingStrategy     uint8 = 0x43
ServiceGetDeviceInfo           uint8 = 0x92
```

本质上就是设备协议里的命令编号。

所以 handler 并不是直接拼裸字节协议，而是：

1. 选定要调用的 `serviceID`
2. 调 `BuildXXX(...)` 构造 Protobuf 请求体
3. 交给 `SendCommand(...)` 发出去

这让业务层不需要知道 topic 怎么拼，也不需要自己处理响应匹配。

## `SendCommand` 的完整链路

你面试里最该讲清楚的就是这段。

以 `turn_off_port` 为例，上层最终会调用：

```go
mqtt.SendCommand(ctx, psn, ServiceTurnOffPort, BuildTurnOffPort(devicePorts))
```

`SendCommand(...)` 内部做了这些事：

### 1. 生成请求 id

```go
id := c.nextID()
req.Id = id
```

这一步的意义是：

- 每次命令都有唯一编号
- 后续响应回来时，能知道它属于哪次调用

### 2. 计算 request / response topic

```go
requestTopic := fmt.Sprintf(endUserRequestTopicTpl, psn, serviceID)
responseTopic := fmt.Sprintf(endUserResponseTopicTpl, psn, serviceID)
```

### 3. 注册 pending request

```go
c.pending[id] = &pendingRequest{
    id: id,
    ch: make(chan *pb.CommandResponse, 1),
}
```

这相当于先在本地挂一个“等结果的号”。

### 4. 订阅 response topic

```go
c.addSubscription(ctx, responseTopic)
```

这样设备回消息时，这个服务才能收到。

### 5. Protobuf 序列化

```go
payload, err := proto.Marshal(req)
```

### 6. 发布 MQTT 消息

```go
c.conn.Publish(ctx, &paho.Publish{
    Topic:   requestTopic,
    QoS:     1,
    Payload: payload,
})
```

这里用了 `QoS 1`，意思是至少投递一次。

### 7. 等待响应或超时

```go
select {
case resp := <-pr.ch:
    return resp, nil
case <-timeoutCtx.Done():
    ...
}
```

也就是说，对上层 handler 来说它像是一次阻塞等待：

```text
发命令
-> 等设备结果
-> 成功就返回
-> 超时就报错
```

### 8. 清理现场

函数退出前会：

- 从 `pending` map 删除当前请求
- 取消 response topic 订阅

避免请求对象和订阅关系越积越多。

## 响应是怎么匹配回来的

这要看 `OnPublishReceived`。

核心逻辑是：

1. 收到 MQTT 消息
2. 如果 topic 含有 `/enduser/response/`
3. 就把 payload 反序列化成 `CommandResponse`
4. 用 `resp.Id` 去 `pending` map 查找等待中的请求
5. 找到后把响应写进它的 channel

关键思想不是“按 topic 找请求”，而是：

> 按响应里的 `resp.Id` 找请求。

因为同一个 response topic 上，可能连续回来多次响应；真正的请求关联键是 `request id`。

所以你可以把它记成：

```text
请求发送时写入 req.Id
-> 设备返回时带回 resp.Id
-> 服务端用 resp.Id 命中 pending map
-> 把结果交给对应那次调用
```

这就是典型的 **Correlation ID** 思想。

## 为什么这里要用 channel

因为上层 `SendCommand(...)` 需要“发完后等待结果”，而 MQTT 回包是异步到达的。

channel 刚好把这两部分接起来：

- 发送线程先把 `pendingRequest` 放进 map
- 接收线程以后某个时刻收到 MQTT 响应
- 找到这个 `pendingRequest`
- 把 `resp` 写入它的 channel
- 等待中的 `SendCommand(...)` 被唤醒

如果用 C++ 来类比，它有点像：

- `unordered_map<request_id, promise<Response>>`
- 收到响应后 `promise.set_value(...)`
- 发送侧拿 `future.get()` 等结果

只是 Go 这里用的是 `map + channel`。

## 为什么要有 `subCount`

`addSubscription(...)` 和 `removeSubscription(...)` 不是简单的订阅和退订，而是带引用计数的。

行为可以记成：

```text
第一次订阅某 topic -> 真正 Subscribe
后续再次订阅同一 topic -> 只增加计数
某次请求结束 -> 计数减一
计数减到 0 -> 真正 Unsubscribe
```

这样做的原因是：

> 同一个 response topic 可能被多个请求共用，如果一个请求结束就直接退订，可能会把别的还在等待的请求一起影响掉。

这是很典型的共享资源管理思路。

## 超时和重试怎么理解

代码里有两个关键参数：

```go
commandTimeout = 8 * time.Second
const maxAttempts = 2
```

含义是：

- 每次命令最长等 8 秒
- 最多尝试 2 次

流程是：

1. 发一次
2. 等响应
3. 超时则重试一次
4. 第二次还超时就返回失败

这说明它不是“盲目无限重试”，而是比较克制的设备控制策略。  
对于端口开关、功率分配这类操作，无限重试反而可能带来副作用。

## `SendCommandNoResponse` 是什么

不是所有命令都要同步等设备完整响应。

例如遥测模块会调用：

- `BuildStartTelemetryStream()`
- `BuildStopTelemetryStream()`

对应发送入口是：

```go
SendCommandNoResponse(...)
```

它会做：

- 生成 request id
- 序列化 Protobuf
- 发布到 request topic

但不会做：

- 注册 pending map
- 订阅 response topic
- 阻塞等待结果

所以它适合“发出去就行”的命令。

## `BuildXXX(...)` 这层不要忽略

`mqtt.go` 后半部分有很多：

- `BuildGetDeviceInfo()`
- `BuildTurnOnPort(...)`
- `BuildTurnOffPort(...)`
- `BuildSetChargingStrategy(...)`
- `BuildGetPortPDStatus(...)`
- `BuildSetDisplayConfig(...)`

这些函数的价值不是“少写几行代码”，而是：

> 把协议载荷构造从业务 handler 里拆出来，形成清晰的命令构造层。

这样 `handler.go` 更像业务语义层：

```text
我要关端口
-> 校验参数
-> BuildTurnOffPort(...)
-> SendCommand(...)
```

而不是在 handler 里直接堆 Protobuf 细节。

## 这层和 02 是怎么接起来的

可以直接记几个典型调用点：

- `get_device_info` -> `SendCommand(..., ServiceGetDeviceInfo, BuildGetDeviceInfo())`
- `turn_on_port` -> `SendCommand(..., ServiceTurnOnPort, BuildTurnOnPort(...))`
- `turn_off_port` -> `SendCommand(..., ServiceTurnOffPort, BuildTurnOffPort(...))`
- `set_charging_strategy` -> `SendCommand(..., ServiceSetChargingStrategy, BuildSetChargingStrategy(...))`
- `telemetry` 启停 -> `SendCommandNoResponse(...)`

所以 02 和 03 之间的关系可以总结成：

> `handler.go` 负责“决定要做什么”，`mqtt.go` 负责“把这个动作可靠地发到设备并等结果回来”。

## 用后端语言怎么讲

面试里你可以这样描述：

> 这层本质上是一个面向设备协议的异步 RPC 适配层。上层 tool handler 以同步调用方式发命令，但底层实际是 MQTT 异步消息模型，所以我用 request id 作为 correlation id，再配合 pending map 和 channel 做请求-响应匹配，同时处理 topic 订阅、超时重试和资源清理。

这段话比泛泛说“用了 MQTT”有含金量得多。

## 用 AI Agent / MCP 语言怎么讲

也可以换成更贴近岗位的说法：

> MCP Tool 只是模型的能力接口，真正把工具调用落到真实设备执行，靠的是下层命令分发链路。`mqtt.go` 这一层把 Agent 的 tool call 转成设备命令，再把设备异步响应重新包装成 tool result，因此它是模型工具层和真实设备系统之间的关键桥接层。

## 这节你最该背的三句话

### 第一句

> `mqtt.go` 把上层同步的工具调用，转换成底层异步的 MQTT 设备通信。

### 第二句

> 为了把响应精确还给原始请求，项目使用 request id 作为关联键，并通过 pending map 和 channel 完成请求-响应匹配。

### 第三句

> 这层除了消息收发，还处理订阅引用计数、超时重试和命令构造，是项目里最核心的设备通信封装层之一。

## 高频面试问题

### Q1：为什么这里不能直接“发完 MQTT 就返回”？

因为很多工具是控制类或查询类操作，上层需要明确知道设备是否执行成功、返回了什么状态。如果直接发布后返回，MCP 客户端拿不到这次操作的确定结果。

### Q2：为什么需要 request id？

因为 MQTT 是异步通信，同一个 response topic 上可能回来多个响应。只有给每次请求分配唯一 id，响应回来时才能知道它属于哪次调用。

### Q3：为什么不是只按 topic 匹配？

因为 topic 只能告诉你“这是哪个设备、哪类服务的响应”，不能唯一标识“这是哪一次调用的响应”。真正唯一的是 `resp.Id`。

### Q4：pending map 的价值是什么？

它保存所有还在等待结果的请求，相当于一个“未完成调用表”。响应回来后通过 `resp.Id` 命中它，就能把结果交给正确的等待方。

### Q5：为什么这里用 channel？

因为命令发送和响应接收是异步发生的，channel 提供了一种很自然的线程间结果传递方式，让 `SendCommand(...)` 能阻塞等待异步回包。

### Q6：为什么需要超时和重试？

设备可能离线、链路可能抖动、消息可能丢失。没有超时，请求可能一直挂住；没有有限重试，临时波动会直接暴露成失败。这里选择了“8 秒超时 + 最多 2 次尝试”的折中策略。

### Q7：`SendCommand` 和 `SendCommandNoResponse` 的区别是什么？

`SendCommand` 适合需要确定结果的命令，会等待 `CommandResponse`；`SendCommandNoResponse` 只负责下发，不建立等待链路，适合遥测启停这类“发出去即可”的命令。

## 你现在至少要能画出的链路

### 关闭端口

```text
LLM 调用 turn_off_port
-> handler 校验端口参数
-> BuildTurnOffPort(...)
-> SendCommand(...)
-> 分配 request id
-> 注册 pending request
-> 订阅 response topic
-> 发布 MQTT request
-> 设备处理并返回 response
-> OnPublishReceived 按 resp.Id 找到请求
-> channel 返回给 SendCommand
-> handler 检查状态并返回 ToolResult
```

### 开启遥测流

```text
session 建立
-> telemetry manager 触发 start stream
-> BuildStartTelemetryStream()
-> SendCommandNoResponse(...)
-> 发布 MQTT request
-> 后续设备持续上报 stream topic
-> telemetry.go 负责消费并缓存
```

## 这一课的最优学习方式

你不用去背 `autopaho` 的连接细节，也不用陷在 Go 语法里。  
最优做法是盯住这 5 个点：

1. `SendCommand(...)` 做了什么
2. `OnPublishReceived` 怎么把响应送回去
3. `pending map` 为什么需要
4. `subCount` 为什么需要
5. `SendCommand` 和 `SendCommandNoResponse` 分别用在什么场景

只要这 5 个点讲稳，03 这一课在面试里就够用了。
