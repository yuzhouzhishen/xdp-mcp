# 第 04 课：`telemetry.go` 遥测流与实时状态缓存

## 一句话总结

`telemetry.go` 负责按设备 PSN 维护实时遥测流，订阅设备端口状态和设备状态的 MQTT stream topic，并缓存最新快照，让查询类 MCP 工具优先读缓存，而不是每次都发设备命令。

## 这部分在系统里的位置

上一课的 `mqtt.go` 解决的是“发一条命令，等一次响应”。但很多状态查询更适合走持续上报：

```text
设备持续上报遥测
  -> MQTT stream response topic
  -> TelemetryManager.run(...)
  -> 缓存 latestPort / latestDevice
  -> get_port_details / get_temperature_mode 等工具读取缓存
  -> LLM 更快得到状态
```

简单说：

- `mqtt.go`：主动问设备一次。
- `telemetry.go`：让设备持续上报，服务端保存最新状态。

## 核心数据结构

### `TelemetryManager`

```go
type TelemetryManager struct {
    mqtt    *MQTTClient
    mu      sync.Mutex
    streams map[uint64]*telemetryStream
}
```

含义：

- `mqtt`：复用上一课的 MQTTClient。
- `streams`：按 PSN 保存每台设备的遥测流。
- `mu`：保护 `streams` 并发访问。

### `telemetryStream`

```go
type telemetryStream struct {
    cancel        context.CancelFunc
    latestPort    *pb.StreamPortStatus
    updatedPort   time.Time
    latestDevice  *pb.StreamDeviceStatus
    updatedDevice time.Time
    latestMu      sync.RWMutex
}
```

它保存某一台设备的：

- 取消函数：用于停止该设备的遥测 goroutine。
- 最新端口状态：`latestPort`
- 最新设备状态：`latestDevice`
- 两份数据各自的更新时间。
- `latestMu`：读写快照时的锁。

## 为什么按 PSN 管理遥测流

这个 MCP Server 是按设备 URL 访问的：

```text
/{psn}/{token}/mcp
```

不同用户或不同客户端可能访问不同设备。因此遥测不能是一个全局状态，而必须按 PSN 隔离：

```text
streams[10001] -> 设备 A 的遥测流
streams[10002] -> 设备 B 的遥测流
```

这和前面 `context` 注入 PSN 的设计是一致的。

## Start：启动遥测流

`Start(ctx, psn)` 做三件事：

1. 加锁检查这个 PSN 是否已经有流。
2. 如果没有，就创建 `telemetryStream`。
3. 启动一个 goroutine 执行 `tm.run(...)`。

它是幂等的：

```go
if _, exists := tm.streams[psn]; exists {
    return
}
```

也就是说，同一台设备重复调用 `Start` 不会启动多个遥测流。

## Stop：停止遥测流

`Stop(ctx, psn)` 做的是：

1. 从 `streams` 找到对应流。
2. 删除 map 里的记录。
3. 调用 `ts.cancel()` 取消 goroutine。

真正的订阅清理和停止遥测命令是在 `run` 函数退出时做的。

## Latest / LatestDevice：读取缓存

`Latest(psn)` 返回端口遥测：

```go
return ts.latestPort, ts.updatedPort
```

`LatestDevice(psn)` 返回设备级遥测：

```go
return ts.latestDevice, ts.updatedDevice
```

handler 会根据更新时间判断是否还新鲜。例如 `get_port_details` 只复用 30 秒内的缓存。

## Kick：缓存过期时重新触发上报

`telemetryStaleness = 30 * time.Second`

`Kick(ctx, psn)` 的逻辑是：

1. 如果这个 PSN 还没有遥测流，就调用 `Start`。
2. 如果已有遥测流，但最新端口状态为空或超过 30 秒，就发送 `StartTelemetryStream` 指令。

`KickDevice` 类似，只是看的是设备级状态 `latestDevice`。

这里的 “Kick” 可以理解为：

> 设备可能还在线，但暂时没上报新数据。服务端补发一次开始遥测指令，把数据流重新踢起来。

## run：遥测流 goroutine

`run(ctx, psn, ts)` 是核心函数。

它先拼出两个 topic：

```go
portTopic   = device/{psn}/enduser/response/128
deviceTopic = device/{psn}/enduser/response/129
```

对应：

- `ServiceStreamPortStatus = 0x80`
- `ServiceStreamDeviceStatus = 0x81`

然后注册两个 topic handler：

- 端口状态 handler：解析 `StreamPortStatus`，更新 `latestPort` 和 `updatedPort`。
- 设备状态 handler：解析 `StreamDeviceStatus`，更新 `latestDevice` 和 `updatedDevice`。

注意顺序：代码先注册 handler 和订阅 topic，再发送 start command。原因是设备可能很快开始上报，如果先发 start 再订阅，可能丢掉第一批数据。

## 启动遥测的重试

`run` 里会最多发送 3 次开始遥测命令：

```go
for attempt := 1; attempt <= 3; attempt++ {
    tm.kick(ctx, psn)
    wait 8 seconds
    if latestPort != nil || latestDevice != nil {
        break
    }
}
```

它不是等待 ACK，而是等待是否真的收到流数据。这个判断更接近业务目标：只要收到了端口状态或设备状态，就说明遥测流已经跑起来。

## 退出时做了什么

`run` 收到 `ctx.Done()` 后会进入清理：

1. 注销 port topic handler。
2. 注销 device topic handler。
3. 取消两个 topic 的订阅。
4. 发送 `StopTelemetryStream` 指令给设备。

这对应简历里的“按设备维护遥测流生命周期”。

## 哪些工具用到了遥测缓存

典型例子：

- `get_port_details`
  - 先读 `telemetry.Latest(psn)`
  - 30 秒内数据直接返回
  - 过期则 `telemetry.Kick(...)`

- `get_temperature_mode`
  - 读 `telemetry.LatestDevice(psn)`
  - 过期则 `telemetry.KickDevice(...)`

- 显示配置相关工具
  - 会读取设备级 stream 里的 display config
  - 部分设置后会等待新遥测确认配置更新

## 为什么这对 Agent 很重要

Agent 经常会连续调用多个工具，例如：

```text
用户：为什么 C1 口充电慢？
Agent:
1. get_machine_facts
2. get_port_details
3. get_port_pd_status
4. get_port_stats
```

如果每次状态查询都打设备命令，延迟会高，也会增加设备通信压力。遥测缓存可以让高频状态查询更快，Agent 的交互体验更接近实时。

## C++ 类比

可以把 `TelemetryManager` 类比成一个按设备维护的后台订阅服务：

```cpp
class TelemetryManager {
    std::unordered_map<uint64_t, TelemetryStream> streams;

    void start(uint64_t psn) {
        if (streams.contains(psn)) return;
        spawn_thread([=] { run(psn); });
    }

    Snapshot latest(uint64_t psn) {
        std::shared_lock lock(stream.latest_mutex);
        return stream.latest_port;
    }
};
```

`latestMu` 相当于读写锁：写入遥测快照时加写锁，handler 读取快照时加读锁。

## 简历候选写法

> 引入按 PSN 隔离的遥测流管理模块，订阅端口状态和设备状态 MQTT stream topic，缓存 30 秒内最新快照并在缓存过期时主动 Kick 设备上报，降低高频状态查询延迟。

如果空间更紧：

> 设计遥测流缓存，按设备维护最新端口/设备状态，查询类工具优先复用 30 秒内实时快照，减少重复设备命令和交互延迟。

## 面试表达

短版本：

> `telemetry.go` 负责维护设备的实时状态流。每个 PSN 对应一个 telemetry stream，服务端订阅端口状态和设备状态 topic，收到 Protobuf stream 数据后更新缓存。查询类工具会优先读 30 秒内的缓存，过期再触发设备重新上报。

更强一点的版本：

> 这个模块解决的是 Agent 高频查询时的实时性和通信压力问题。工具调用看起来是即时查询，但底层设备状态来自持续遥测流。服务端用最新快照把流式设备数据转换成工具可以快速读取的状态。

## 可能被追问

1. 为什么遥测流要按 PSN 隔离？
2. `Start` 为什么要做幂等？
3. 为什么先订阅 topic，再发送开始遥测命令？
4. 30 秒新鲜度是在哪里判断的？
5. `Kick` 和 `Start` 有什么区别？
6. 为什么启动遥测时最多尝试 3 次？
7. 退出时为什么要发送 `StopTelemetryStream`？
8. 如果设备离线，工具会如何表现？

## 练习答案

**Q：`Kick` 是干什么的？**

当缓存不存在或超过 30 秒时，`Kick` 会重新发送开始遥测命令，让设备重新上报状态。如果当前还没有流，它会先启动流。

**Q：为什么先订阅再发 start？**

因为设备可能很快开始上报。如果先发 start 再订阅，服务端可能错过第一批遥测数据。

**Q：这个模块和 `mqtt.go` 有什么区别？**

`mqtt.go` 是一次请求对应一次响应；`telemetry.go` 是持续订阅设备状态，并缓存最新快照。前者适合控制命令，后者适合实时状态查询。

## 复习检查

- 能不能解释 `streams map` 的 key 和 value？
- 能不能画出 `Start -> run -> Subscribe -> Kick -> cache update` 流程？
- 能不能说明 30 秒缓存的意义？
- 能不能说清楚哪些工具会读遥测缓存？
- 能不能把这部分讲成“提升 Agent 实时查询体验”的工程点？

