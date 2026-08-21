# 第 02 课：`handler.go` 工具注册与业务 handler

## 一句话总结

`handler.go` 是 `xdp-mcp` 的 MCP 能力层。它把智能充电设备的查询、诊断、控制、显示配置等能力注册成 MCP Tool，同时把预设 Prompt 注册成 MCP Prompt 和 Resource，供 Claude、Cursor、Codex 等客户端发现和调用。

## 这部分在系统里的位置

如果 `main.go` 负责“服务怎么起来”，那么 `handler.go` 负责“模型能调用什么”。

```text
LLM 客户端
  -> 调用 MCP tool
  -> handler.go 参数校验 / 读取 PSN
  -> MQTTClient.SendCommand 或 TelemetryManager.Latest
  -> MQTT + Protobuf
  -> 设备返回结果
  -> handler.go 格式化为 JSON 文本
  -> LLM 根据结果继续推理或回复用户
```

这就是简历里说的“Agent 工具层”的核心。

## 代码里实际注册了哪些工具

当前 `handler.go` 里注册了 18 个 MCP Tool，按场景可以分成四类：

**1. 设备与端口查询**

- `get_device_info`
- `get_machine_facts`
- `get_port_details`
- `get_charging_status`
- `get_temperature_mode`

**2. USB-C / PD 诊断与历史数据**

- `get_port_pd_status`
- `get_port_stats`

**3. 充电控制**

- `set_charging_strategy`
- `set_usba_charging_mode`
- `set_temperature_mode`
- `set_port_power_allocation`
- `turn_on_port`
- `turn_off_port`

**4. 屏幕与显示配置**

- `get_display_config`
- `set_display_intensity`
- `set_status_display_mode`
- `set_idle_display`
- `set_hourly_chime`

简历里不建议写死“12 个工具”，因为 README 已经落后于代码。更稳的写法是“围绕查询、诊断、控制、显示配置四类场景封装 MCP 工具”。

## Tool 是怎么注册的

典型结构如下：

```go
s.AddTool(
    mcp.NewTool("turn_off_port",
        mcp.WithDescription("Turn off one or more charging ports."),
        mcp.WithArray("ports",
            mcp.Required(),
            mcp.Description("Array of port numbers to turn off (1-indexed)"),
        ),
    ),
    makeTurnOffPortHandler(mqtt),
)
```

可以拆成两部分理解：

- `mcp.NewTool(...)`：告诉 LLM 这个工具叫什么、能做什么、需要哪些参数。
- `makeTurnOffPortHandler(...)`：真正执行工具逻辑，把 MCP 调用转成设备指令。

对于 Agent 来说，工具描述和参数 schema 很重要。模型能不能选对工具，很多时候取决于工具命名、description 和参数约束。

## 查询类工具：优先读缓存，必要时触发遥测

`get_port_details` 和 `get_temperature_mode` 会优先从 `TelemetryManager` 读取最近的设备状态。

核心逻辑：

1. 从 `context` 里读取 PSN。
2. 查找该 PSN 对应的最新遥测缓存。
3. 如果缓存存在且 30 秒内更新，直接返回。
4. 如果缓存缺失或过期，调用 `telemetry.Kick(...)` 或 `KickDevice(...)` 触发设备重新上报。
5. 等待一小段时间，如果仍然没有数据，返回“设备可能离线”的错误。

这部分的工程价值是：高频查询不用每次都打设备指令，既减少延迟，也降低设备侧通信压力。

## 控制类工具：参数校验后发 MQTT/Protobuf 指令

比如 `turn_off_port` 的链路是：

```text
LLM 调用 turn_off_port({"ports":[1,2]})
  -> handler 校验 ports 参数
  -> 用户端口号从 1-indexed 转成设备内部端口号
  -> BuildTurnOffPort(...)
  -> MQTTClient.SendCommand(...)
  -> 等待设备 CommandResponse
  -> 返回成功或错误文本
```

`set_charging_strategy`、`set_temperature_mode`、`set_port_power_allocation` 也是同样模式：参数校验、构造 Protobuf command、发送 MQTT、等待响应、检查设备状态码。

面试时要强调：这不是简单 HTTP API 转发，而是把 LLM 的结构化工具调用转成真实设备协议。

## PD 诊断工具为什么更像 Agent 场景

`get_port_pd_status` 不是只查一个字段，它会先调用充电状态，找出正在工作的端口，再并发查询每个活跃端口的 PD 信息。

它返回的信息包括：

- 电池 VID/PID 和容量信息
- 线缆是否有 E-marker
- 线缆最大电压、电流、USB 速率
- PD 协议版本
- PPS/EPR 能力
- Sink capability
- 温度范围
- 设备/线缆数据库匹配结果

这类工具很适合被 Agent 用来回答：

- “为什么这个口充电慢？”
- “这根线是否支持高功率？”
- “当前瓶颈是设备、线材还是充电策略？”

所以简历里可以写“USB-C/PD 诊断”，这比泛泛写“设备诊断”更具体。

## Prompt 和 Resource 的作用

`handler.go` 里通过 `embed.FS` 读取 `data/prompts/*.txt`，并把它们同时注册成：

- MCP Prompt
- MCP Resource

当前包括：

- `charging_brief`
- `charging_status`
- `charging_control`
- `charging_profile`
- `charging_comparison`
- `device_diagnostic`

Prompt 的作用是给 LLM 一套固定任务模板，例如快速查看充电状态、做深度诊断、对比前后功率变化等。

Resource 的作用是让部分客户端可以通过资源列表发现这些 prompt 内容。

## C++ 类比

可以把 `handler.go` 类比成一个“命令路由表 + 业务回调集合”：

```text
server.registerTool("get_port_details", schema, GetPortDetailsHandler);
server.registerTool("turn_off_port", schema, TurnOffPortHandler);
server.registerTool("get_port_pd_status", schema, GetPortPDStatusHandler);
```

每个 handler 像 C++ 里的一个 command object 或 callback：

```text
class TurnOffPortCommand {
public:
    Result operator()(Context ctx, Json args) {
        auto psn = ctx.psn();
        auto ports = parsePorts(args);
        auto req = BuildTurnOffPort(ports);
        return mqtt.sendCommand(psn, req);
    }
};
```

## 简历候选写法

推荐简历里写成：

> 围绕设备查询、USB-C/PD 诊断、端口控制和显示配置四类场景封装 MCP Tools，并注册充电状态、设备诊断、功率对比等 Prompt/Resource，使 Claude/Cursor/Codex 等客户端可通过自然语言调用真实设备能力。

如果空间更紧，可以压缩成：

> 封装设备查询、USB-C/PD 诊断、端口控制、显示配置等 MCP Tools，并配套注册诊断类 Prompt/Resource，支撑 LLM 客户端通过自然语言访问真实设备能力。

## 面试表达

短版本：

> `handler.go` 负责把设备能力注册成 MCP Tool。工具分为查询、诊断、控制和显示配置几类。每个工具都有明确的参数 schema 和 handler，handler 会从 context 读取 PSN，再通过遥测缓存或 MQTT/Protobuf 指令访问真实设备。

更强一点的版本：

> 我理解 Agent 项目里最关键的不是“能调模型”，而是能把外部系统能力抽象成模型可发现、可调用、可控的工具。这个项目的 handler 层就是这个抽象边界：上层是 MCP Tool schema，下层是真实设备协议和遥测数据。

## 可能被追问

1. MCP Tool 的 description 为什么重要？
2. 查询类工具和控制类工具有什么区别？
3. 为什么 `get_port_details` 不每次都直接发设备命令？
4. `get_port_pd_status` 为什么能支持“充电慢”这类诊断问题？
5. Prompt 和 Tool 有什么区别？
6. Resource 在这里有什么用？
7. 控制类工具如何避免参数错误？
8. 为什么端口号对用户是 1-indexed，对设备内部可能要转换？

## 复习检查

- 能不能说出四类工具分别是什么？
- 能不能解释一个工具从注册到执行的完整流程？
- 能不能拿 `turn_off_port` 画出调用链路？
- 能不能说明 `get_port_pd_status` 为什么比普通查询更有诊断价值？
- 能不能解释 Prompt、Resource、Tool 的区别？

