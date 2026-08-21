# 第 03 课：`mqtt.go` MQTT 请求-响应链路

## 一句话总结

`mqtt.go` 是 MCP 工具调用真正落到设备侧的通信层。它把 handler 构造出的 Protobuf 指令发布到 MQTT request topic，再通过 `request id + pending map + channel` 等待设备从 response topic 返回匹配结果。

## 这部分在系统里的位置

前两课讲的是“客户端如何进来”和“模型能调用什么工具”。这一课讲的是工具执行后如何真正访问设备。

```text
MCP Tool Handler
  -> BuildXXXCommand(...)
  -> MQTTClient.SendCommand(...)
  -> MQTT request topic
  -> 设备 / IoT 网关
  -> MQTT response topic
  -> OnPublishReceived
  -> pending map 按 request id 找到等待方
  -> handler 得到 CommandResponse
```

这部分是简历里最容易被追问、也最能体现工程能力的一段。

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

可以拆开理解：

- `conn`：MQTT 连接管理器，负责连接 broker、发布、订阅。
- `router`：根据 topic 分发消息，遥测模块会用到。
- `idSeq`：原子递增的 request id 生成器。
- `pending`：保存“正在等待响应的请求”。
- `subCount`：订阅引用计数，避免多个模块订阅同一个 topic 时互相提前取消。

### `pendingRequest`

```go
type pendingRequest struct {
    id uint32
    ch chan *pb.CommandResponse
}
```

每发出一个需要响应的命令，就创建一个 `pendingRequest`。设备响应回来后，`OnPublishReceived` 根据响应里的 `resp.Id` 找到对应的 `pendingRequest`，再把响应塞进它的 channel。

## MQTT topic 设计

代码里有两个 topic 模板：

```go
endUserRequestTopicTpl  = "device/%d/enduser/request/%d"
endUserResponseTopicTpl = "device/%d/enduser/response/%d"
```

参数含义：

- 第一个 `%d`：设备 PSN。
- 第二个 `%d`：service id。

举例：

```text
device/123456/enduser/request/76
device/123456/enduser/response/76
```

这里的 `76` 对应 `ServiceTurnOnPort`，也就是打开端口。

## Service ID 是什么

`ServiceSetChargingStrategy`、`ServiceGetChargingStatus`、`ServiceTurnOffPort` 这些常量，本质上是设备协议里的命令编号。

例如：

```go
ServiceTurnOnPort  uint8 = 0x4c // 76
ServiceTurnOffPort uint8 = 0x4d // 77
```

handler 不直接拼二进制协议，而是：

1. 选择对应 service id。
2. 调用 `BuildXXX(...)` 构造 Protobuf request。
3. 交给 `SendCommand(...)` 发布。

这让业务 handler 不需要关心 MQTT topic 和 pending map 细节。

## `SendCommand` 完整流程

以 `turn_off_port` 为例，handler 最终会调用：

```go
mqtt.SendCommand(ctx, psn, ServiceTurnOffPort, BuildTurnOffPort(ports))
```

`SendCommand` 内部流程如下：

1. 生成 request id。
   - `id := c.nextID()`
   - 写入 `req.Id = id`

2. 拼出 request topic 和 response topic。
   - `device/{psn}/enduser/request/{serviceID}`
   - `device/{psn}/enduser/response/{serviceID}`

3. 创建 `pendingRequest`，放入 `pending` map。
   - key 是 request id
   - value 里有一个 channel，用来接收响应

4. 订阅 response topic。
   - 使用 `addSubscription(...)`
   - 内部通过 `subCount` 做引用计数

5. Protobuf 序列化 request。
   - `proto.Marshal(req)`

6. 发布 MQTT 消息。
   - QoS 是 1
   - 表示至少投递一次

7. 等待响应或超时。
   - `commandTimeout = 8 * time.Second`
   - 最多尝试 2 次

8. 收到响应后返回。
   - handler 再检查 `resp.Status`

9. 函数退出时清理。
   - 从 `pending` map 删除 request id
   - 取消 response topic 订阅

## 响应是怎么回到对应请求的

关键在 `OnPublishReceived`：

```go
if strings.Contains(topic, "/enduser/response/") {
    resp := &pb.CommandResponse{}
    if err := proto.Unmarshal(p.Packet.Payload, resp); err == nil {
        c.mu.Lock()
        pending, ok := c.pending[resp.Id]
        c.mu.Unlock()
        if ok {
            pending.ch <- resp
        }
    }
}
```

注意：它不是只靠 topic 匹配，因为同一个 response topic 上可能有多个请求的响应。真正区分请求的是 Protobuf 里的 `resp.Id`。

所以核心机制是：

```text
request id 写入 CommandRequest
  -> 设备处理后把同一个 id 写入 CommandResponse
  -> 服务端用 resp.Id 找 pending map
  -> 找到后把响应发给对应 channel
```

这和你实习里的“复合键映射表 / 类 Correlation ID”是同一个思想，只是这里用的是 MQTT + Protobuf + Go channel。

## 为什么要有 `subCount`

`addSubscription` 和 `removeSubscription` 不是简单地订阅/取消订阅，而是维护引用计数：

```text
第一次订阅某 topic -> 真正 Subscribe
第二次订阅同一 topic -> 只把计数 +1
某个调用结束 -> 计数 -1
计数归零 -> 真正 Unsubscribe
```

原因是：同一个 response topic 可能被多个请求或遥测模块使用。如果一个调用结束就直接 unsubscribe，可能会影响别的正在等待消息的模块。

## 超时和重试

`commandTimeout` 是 8 秒，`maxAttempts` 是 2。

流程是：

1. 发布一次 command。
2. 等待 response。
3. 8 秒没等到，就重试一次。
4. 第二次仍然超时，就返回错误。

这不是“无限重试”，因为控制设备的指令不能无节制重复。尤其是端口开关、功率分配这类操作，重试必须克制。

## `SendCommandNoResponse` 是什么

有些指令不需要等待设备返回完整业务响应，例如遥测启动/停止：

```go
tm.mqtt.SendCommandNoResponse(ctx, psn, ServiceStartTelemetryStream, BuildStartTelemetryStream())
```

这种场景只需要把命令发出去，不需要占用 pending map 等响应。

它仍然会：

- 生成 request id
- Protobuf 序列化
- 发布到 request topic

但不会：

- 订阅 response topic
- 写 pending map
- 等待 response

## Command Builder 的作用

文件后半部分有很多 `BuildXXX` 函数，例如：

- `BuildGetDeviceInfo`
- `BuildSetChargingStrategy`
- `BuildTurnOnPort`
- `BuildGetPortPDStatus`
- `BuildGetPowerHistoricalStats`
- `BuildSetDisplayConfig`

它们负责把业务参数转换成 Protobuf 的 `CommandRequest`。

这是一层很重要的隔离：

```text
handler 只关心业务参数
BuildXXX 负责 Protobuf payload
SendCommand 负责 MQTT 发布、订阅、匹配、超时
```

如果没有这层隔离，handler 里会混进大量协议细节，代码会很难读。

## C++ 类比

可以把 `SendCommand` 类比成 C++ 里的异步请求管理器：

```cpp
uint32_t id = nextId();
request.set_id(id);

auto promise = std::make_shared<std::promise<Response>>();
pending[id] = promise;

mqtt.subscribe(responseTopic);
mqtt.publish(requestTopic, serialize(request));

auto future = promise->get_future();
if (future.wait_for(8s) == timeout) {
    retry_or_return_error();
}
```

Go 的 channel 相当于这里的 `promise/future`。`pending map` 就是根据 request id 找到对应等待者的表。

## 简历候选写法

> 通过 MQTT/Protobuf 对接设备侧命令协议，基于 service id、request id 和 pending map 实现异步请求-响应匹配，支持 QoS1 发布、8s 超时、失败重试和错误返回。

如果更口语化：

> 我负责梳理并实现 MCP 工具到设备命令的通信链路：工具调用进入 handler 后构造 Protobuf 请求，通过 MQTT 发布到设备 topic，再用 request id 在 pending map 中匹配异步响应。

## 面试表达

短版本：

> `mqtt.go` 负责把 MCP 工具调用转成设备命令。每个命令会生成 request id，写入 Protobuf 请求并发布到 MQTT request topic。设备返回 CommandResponse 后，服务端根据响应里的 id 从 pending map 找到对应 channel，把响应交回正在等待的 handler。

更强一点的版本：

> 这个设计解决的是 MQTT 异步通信和 MCP 同步工具调用之间的桥接问题。对 LLM 来说，tool call 像一次普通函数调用；但底层设备通信是异步消息。`SendCommand` 用 request id、pending map、channel、timeout/retry 把这两种模型接起来。

## 可能被追问

1. 为什么不能只靠 MQTT topic 匹配响应？
2. `pending map` 的 key 是什么？
3. 如果设备不返回响应会怎样？
4. 为什么最大重试次数只有 2 次？
5. QoS1 代表什么？会不会导致重复消息？
6. `SendCommand` 和 `SendCommandNoResponse` 区别是什么？
7. 为什么订阅需要引用计数？
8. 如果响应晚到了，pending 已经删了，会怎样？

## 练习答案

**Q：为什么要 request id？**

因为 MQTT response topic 只能表示“哪个设备、哪个 service”的响应，不能唯一表示哪一次请求。多个请求可能共享同一个 response topic，所以需要 request id 做更细粒度的匹配。

**Q：pending map 的作用是什么？**

它保存正在等待响应的请求。设备响应回来后，服务端用 `resp.Id` 找到对应的 `pendingRequest`，再通过 channel 唤醒等待中的 `SendCommand`。

**Q：这个模块和 Agent 有什么关系？**

MCP tool 对模型来说像函数调用，但真实设备通信是异步 MQTT 消息。这个模块把异步设备协议封装成工具 handler 可等待的调用结果，是 Agent 能操作真实设备的关键桥接层。

## 复习检查

- 能不能画出 `SendCommand` 的完整流程？
- 能不能解释 `request id + pending map + channel` 怎么配合？
- 能不能解释为什么有超时和重试？
- 能不能说明 `subCount` 为什么存在？
- 能不能把这部分和你简历里的“异步请求-响应匹配”对应起来？

