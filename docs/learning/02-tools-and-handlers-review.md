# 第 02 课复盘：工具注册、Prompt/Resource 与业务 handler

## 这份复盘的定位

这份文档不是逐行解释 `handler.go`，而是帮你抓住这一层最重要的系统价值。

目标是做到：

- 知道 `handler.go` 在整个 `xdp-mcp` 里负责什么
- 理解 Tool、Prompt、Resource 三类能力分别是什么
- 能说清查询类工具和控制类工具的执行差异
- 能用后端语言和 AI Agent 语言分别描述这层设计

## 一句话总结

`handler.go` 是 `xdp-mcp` 的能力暴露层。它把真实设备的查询、诊断、控制、显示配置等能力注册成 MCP Tool，同时把预设 prompt 文本注册成 MCP Prompt 和 Resource，让 LLM 客户端能发现并调用这些设备能力。

## 这一层在系统里的位置

可以把前两课的链路接起来：

```text
MCP Client
-> 路由 / 鉴权 / PSN 注入 context
-> MCP Server
-> handler.go 里的 Tool / Prompt / Resource
-> MQTT / 遥测缓存 / 数据库 / 静态资源
-> 真实设备或本地数据源
```

如果说：

- `01` 解决的是“请求怎么进来”
- 那 `02` 解决的就是“模型进来以后到底能调用什么”

## 这节的关键文件

- `handler.go`
- `telemetry.go`
- `mqtt.go`
- `proto/xdp/v1/*.proto`
- `data/prompts/*.txt`
- `data/machine_facts/*.json`

主战场还是 `handler.go`，其他文件在这节里只当被调用方。

## 这一层到底在做什么

`handler.go` 主要做三件事：

1. 注册 Prompt
2. 注册 Resource
3. 注册 Tool 并为每个 Tool 绑定具体 handler

它的作用不是“直接做设备通信”，而是：

> 把下层设备协议、遥测缓存、数据库和静态资源，包装成 LLM 能理解和调用的统一工具接口。

这层是 MCP 项目里最接近“Agent 工具层”的地方。

## 所有工具的共同前提：先从 context 里取 PSN

`psnFromContext()` 是整个文件里最重要的基础函数之一。

它只干一件事：

- 从 `ctx` 中读取当前会话对应的 `psn`

这说明所有 tool handler 的默认前提是：

- 工具不显式接收设备 ID
- 设备身份由上游路由层和会话上下文提供

这和 `01` 课完全接上：

- `01` 负责把 `psn` 放进 context
- `02` 负责在业务 handler 里真正消费这个 `psn`

面试里可以这样讲：

> 我们把设备身份从工具参数里剥离出来，工具只暴露业务参数，当前设备由会话上下文提供，这样既简化了工具 schema，也减少了模型误传设备 ID 的风险。

## Prompt 和 Resource 是怎么注册的

`registerPrompts()` 会扫描 `data/prompts/*.txt`，然后把每个 prompt 文本做两次注册：

1. 注册成 MCP Prompt
2. 注册成 MCP Resource

这意味着：

- **Prompt** 面向模型，给它一套任务模板
- **Resource** 面向客户端发现机制，让客户端能通过资源列表找到这些内容

当前的 prompt 包括：

- `charging_brief`
- `charging_status`
- `charging_control`
- `charging_profile`
- `charging_comparison`
- `device_diagnostic`

这类设计的工程价值在于：

> 一套 prompt 内容既能作为模型任务模板，又能作为可发现资源复用，不需要在客户端和服务端重复维护两份定义。

## Tool 注册长什么样

所有 tool 注册都遵循一个固定结构：

```go
s.AddTool(
    mcp.NewTool(...schema...),
    makeXXXHandler(...deps...),
)
```

一定要把它拆成两层理解：

### 第一层：`mcp.NewTool(...)`

这是对外的工具接口定义，告诉模型：

- 工具名是什么
- 描述是什么
- 需要哪些参数
- 参数类型和约束是什么

这层是“面向 LLM 的 schema”。

### 第二层：`makeXXXHandler(...)`

这是对内的业务执行逻辑，负责：

- 读取 `psn`
- 校验参数
- 决定读缓存、查数据库还是发设备命令
- 把结果格式化成 MCP ToolResult

这层是“面向系统执行的 handler”。

## 这层注册了哪些工具

不要死记工具总数，最优记法是按场景分类。

### 1. 设备与端口查询

- `get_device_info`
- `get_machine_facts`
- `get_port_details`
- `get_charging_status`
- `get_temperature_mode`

### 2. USB-C / PD 诊断与历史数据

- `get_port_pd_status`
- `get_port_stats`

### 3. 充电控制

- `set_charging_strategy`
- `set_usba_charging_mode`
- `set_temperature_mode`
- `set_port_power_allocation`
- `turn_on_port`
- `turn_off_port`

### 4. 屏幕与显示配置

- `get_display_config`
- `set_display_intensity`
- `set_status_display_mode`
- `set_idle_display`
- `set_hourly_chime`

所以面试里更稳的说法是：

> 当前工具主要分成查询、诊断、控制和显示配置四类，而不是死背具体多少个工具。

## 查询类工具：不是都走同一条路径

这是这一层最值得讲的点之一。

查询类工具看起来都像“读数据”，但实际上实现路径并不一样。

### 1. 直接打设备

例如 `get_device_info`：

- 取 `psn`
- 调 `mqtt.SendCommand(...)`
- 等设备返回
- 组装 JSON 返回

这类工具适合查询明确且设备实时返回的内容。

### 2. 查数据库 + 静态资源

例如 `get_machine_facts`：

- 先查数据库里的 `product_family`
- 再从本地嵌入的 JSON 文件里读该产品族的静态事实

这类工具不是“设备实时状态”，而是“设备静态知识”。

### 3. 优先走遥测缓存，必要时触发刷新

例如 `get_port_details` 和 `get_temperature_mode`：

- 先查 `TelemetryManager` 的最近缓存
- 如果 30 秒内数据新鲜，直接返回
- 如果没有缓存或缓存过期，就 `Kick(...)` 触发设备重新上报
- 再等待一段时间
- 超时就报设备可能离线

这一类是典型的“缓存优先 + 按需刷新”模式。

面试里可以这样说：

> 查询类工具并不是统一打设备，而是根据数据属性选择不同的数据源路径：实时信息走设备命令、静态信息走数据库和本地资源、高频状态走遥测缓存优先。

## 控制类工具：固定模式非常清晰

控制类工具的模式基本都一样。

拿 `set_charging_strategy` 举例：

1. 从 context 取 `psn`
2. 从请求参数里读 `strategy`
3. 做类型转换和语义映射
4. 构造 protobuf command
5. 通过 `mqtt.SendCommand(...)` 下发设备
6. 检查设备响应状态码
7. 返回成功或错误文本

也就是说，控制类工具不是“直接改数据库”或“改本地内存”，而是：

> 把模型发起的结构化工具调用翻译成真实设备协议指令。

这句话非常适合面试时直接讲。

## Tool description 为什么很重要

对 Agent 项目来说，tool description 不只是注释，而是“模型能否选对工具”的关键输入。

例如 `set_usba_charging_mode` 的 description 里直接放了：

- 魔拟充
- 小家电模式
- 模拟 A 口
- 模拟充

这样用户说这些自然语言时，模型更容易把意图映射到正确工具上。

所以这层除了注册业务能力，还有一层隐含职责：

> 用名称、描述和参数 schema，把设备能力翻译成模型能理解的语义接口。

## Prompt、Tool、Resource 的区别

这个问题面试里非常容易被问。

### Tool

可执行能力。  
会真正触发查询、控制、诊断流程。

### Prompt

任务模板。  
告诉模型“面对某类问题时应该如何组织分析或回答”。

### Resource

可发现内容。  
让客户端能通过资源列表拿到静态文本或说明内容。

一句话版：

> Tool 是能执行的，Prompt 是指导模型思考和表达的，Resource 是让客户端可发现和读取的内容载体。

## C++ / 后端类比

如果用你熟悉的方式理解，这一层很像：

```text
CommandRegistry + HandlerFactory + ServiceFacade
```

具体可以类比成：

- `registerAllToolsWithContext`：命令注册表初始化
- `mcp.NewTool`：RPC / 命令元数据定义
- `makeXXXHandler`：业务回调构造器
- `psnFromContext`：从请求上下文取会话身份
- `mqtt.SendCommand`：下游设备网关客户端
- `telemetry.Latest / Kick`：缓存 + 刷新机制

如果按后端语言总结：

> 这层本质上是一个命令分发层，把 LLM 的结构化 tool call 分发到不同的数据源和下游能力上。

## AI Agent / MCP 讲法

如果按 AI Agent 项目来讲，这一层的价值更明显：

- 它不是简单暴露 HTTP API
- 而是在设计“模型能用什么工具、如何理解这些工具”
- 查询类、控制类、诊断类工具各自的 schema 和边界都不一样
- Prompt/Resource 又补了一层任务模板和内容发现机制

一句话总结：

> `handler.go` 是设备能力到 Agent 工具的抽象边界，上层是模型可理解的 tool schema，下层是真实设备协议、遥测缓存和静态数据源。

## 面试时可以怎么讲

### 后端版 30 秒

`handler.go` 负责把设备查询、控制和配置这些能力注册成可调用的 MCP 工具。每个工具都会先从上下文中读取当前设备 PSN，再根据业务类型决定是走数据库、遥测缓存，还是通过 MQTT/Protobuf 下发设备命令，最后把结果格式化成统一的 ToolResult 返回。

### AI Agent 版 30 秒

这一层是整个项目最核心的 Agent 工具抽象层。我们不是把底层设备协议直接暴露给模型，而是通过工具名、描述、参数 schema 和业务 handler，把设备能力包装成模型可发现、可调用、可控的 MCP Tool，并配套 Prompt/Resource 提供任务模板和内容发现能力。

### 1 分钟版

`handler.go` 这一层主要负责能力暴露。它会先批量注册 prompt 和 resource，再按场景注册查询、诊断、控制和显示配置四类工具。每个工具分成两层：上层是 `mcp.NewTool(...)` 定义的 schema，告诉模型工具怎么用；下层是 `makeXXXHandler(...)` 定义的执行逻辑，真正从 context 取设备 PSN，再根据业务类型去读数据库、查遥测缓存或者发送 MQTT/Protobuf 指令。这里最关键的设计点是把设备身份和业务参数解耦，把底层设备链路抽象成模型可理解的结构化工具接口。

## 面试高频问题

### 1. `handler.go` 在这个项目里负责什么？

负责把底层设备能力抽象成 MCP Tool、Prompt 和 Resource，是整个项目的能力暴露层。

### 2. 为什么所有 handler 都先从 context 里取 `psn`？

因为设备身份属于会话上下文，不应该作为用户自然语言层显式传入的业务参数。这样可以简化工具 schema，也能降低模型传错设备 ID 的风险。

### 3. 查询类工具和控制类工具最大的区别是什么？

查询类工具可能走缓存、数据库、静态资源或设备命令，路径不唯一；控制类工具通常都是参数校验后构造 protobuf 命令，再通过 MQTT 下发设备并检查响应。

### 4. 为什么 `get_port_details` 不每次都直接打设备？

因为它属于高频状态查询，优先读遥测缓存能减少设备通信压力和延迟；只有缓存缺失或过期时才触发设备重新上报。

### 5. Tool description 为什么重要？

因为模型选工具时会参考名称、描述和参数 schema。description 写得越贴近用户自然语言，模型越容易选中正确工具。

### 6. Prompt 和 Tool 的区别是什么？

Tool 是会执行的能力；Prompt 是任务模板，不直接执行设备操作。

### 7. Resource 在这里有什么用？

让客户端可以通过资源发现机制拿到 prompt 内容或其他静态内容，不一定非要通过 tool 调用。

### 8. 这一层更像后端服务，还是更像 AI Agent 工具层？

两者都有。实现方式上它很像后端命令分发层；对外能力形态上它是典型的 Agent 工具层。

## 你现在最该记住的 5 个点

1. `handler.go` 是能力暴露层，不是底层通信层。
2. 所有 tool handler 默认都从 context 中取当前设备 PSN。
3. Tool、Prompt、Resource 分别对应执行能力、任务模板和可发现内容。
4. 查询类工具不只一种实现路径，控制类工具的执行模式更固定。
5. 这一层的核心价值是把底层设备协议包装成模型可理解的结构化工具接口。

## 对你当前阶段的学习要求

这一课你不需要做到：

- 背下全部 18 个工具名
- 读懂 `handler.go` 每个分支实现细节
- 完全搞懂所有 protobuf 字段

这一课你需要做到：

- 知道四类工具分别是什么
- 知道 Tool / Prompt / Resource 的区别
- 能说出查询类和控制类工具的差异
- 能用后端版和 AI Agent 版各讲 30 秒

## 下一课衔接

第 03 课再继续往下走，不再看“能力怎么注册”，而是看：

- `mqtt.go` 如何把工具调用变成设备请求
- request/response 怎么匹配
- timeout / pending map / 并发订阅怎么做
- 为什么这层最像真正的“后端异步通信桥接”
