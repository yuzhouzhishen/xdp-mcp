# xdp-mcp 学习笔记

这个目录用来记录 `xdp-mcp` 的分阶段学习过程。目标不是先成为 Go 专家，而是站在你已有的 C++/IoT 背景上，把这个项目理解到“能写进简历、能被面试追问、能画出链路”的程度。

你需要重点搞清楚：

- 这个 MCP Server 解决了什么问题
- MCP Client、MCP Server、Tool、Prompt、Resource 分别是什么
- LLM 客户端如何通过 MCP 调用真实设备能力
- 工具调用如何变成 MQTT/Protobuf 指令
- 鉴权、会话上下文、遥测缓存、超时重试、请求响应匹配分别在哪里做
- 简历里哪些点可以写，哪些点现在还不适合写得太满

## 学习顺序

1. `01-main-startup-and-routing.md`
   - 对应文件：`main.go`
   - 重点：服务启动、配置加载、鉴权初始化、MQTT 初始化、MCP Server 创建、SSE/Streamable HTTP 路由、优雅退出

2. `02-tools-and-handlers.md`
   - 对应文件：`handler.go`
   - 重点：工具注册、参数 schema、查询类工具、控制类工具、显示配置工具、Prompt/Resource、结果格式化

3. `03-mqtt-request-response.md`
   - 对应文件：`mqtt.go`
   - 重点：MQTT topic、Protobuf 指令构造、request id、pending map、超时、重试、订阅引用计数

4. `04-telemetry-cache.md`
   - 对应文件：`telemetry.go`
   - 重点：按 PSN 维护遥测流、最新状态缓存、30 秒新鲜度判断、Kick 机制、订阅清理

5. `05-auth-skill-and-safety.md`
   - 对应文件：`auth.go`、`skill.go`、`data/skills/*.md`
   - 重点：PSN/TOKEN 鉴权、设备会话隔离、设备说明文档、安全边界

6. `06-resume-and-interview-review.md`
   - 重点：项目总结、简历 bullet、面试问答、哪些说法需要降级为“了解/参与”

## 每篇笔记固定记录

- 一句话总结
- 这部分在系统里的位置
- 关键代码路径
- C++ 类比
- 面试表达
- 可能被追问的问题
- 简历候选写法

## 当前简历定位

这个项目不要包装成“训练大模型”或“做了一个完整 Agent 框架”。更准确的定位是：

> 面向智能充电设备的 MCP Server / Agent 工具层，把设备查询、USB-C/PD 诊断、端口控制、显示配置等能力封装成 LLM 可调用工具，让 Claude、Cursor、Codex 等客户端可以安全地访问真实设备。

你的差异化不是“会调一个模型 API”，而是：

- 有 C++/FreeRTOS/设备通信背景
- 理解真实设备的状态、协议、控制链路
- 能把设备能力通过 MCP 工程化暴露给 AI Agent
- 能讲清楚 Tool Calling 背后的服务端实现

