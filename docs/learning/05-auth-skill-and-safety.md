# 第 05 课：`auth.go`、`skill.go` 与安全边界

## 一句话总结

`auth.go` 负责设备访问鉴权，`skill.go` 负责按设备型号返回客户端使用说明。它们共同决定了“谁能控制哪台设备”和“LLM 应该如何正确使用这些工具”。

## 这部分在系统里的位置

到目前为止，我们已经看到：

- `main.go`：启动服务和路由。
- `handler.go`：注册 MCP 工具。
- `mqtt.go`：把工具调用转成设备命令。
- `telemetry.go`：缓存实时设备状态。

这一课关注边界：

```text
客户端请求 /{psn}/{token}/mcp
  -> auth.Validate(psn, token)
  -> 验证通过后进入 MCP transport
  -> context 注入 psn
  -> handler 只能操作该 psn 对应设备
```

以及：

```text
客户端请求 /{psn}/SKILL.md
  -> 查询设备型号
  -> 返回对应 data/skills/{MODEL}.md
  -> LLM 根据说明选择 prompt 和 tool
```

## `auth.go` 做了什么

### 连接数据库

`NewAuthenticator(ctx, databaseURL)` 使用 `pgxpool.New` 创建 PostgreSQL 连接池，并调用 `Ping` 验证数据库可用。

这说明设备是否存在不是写死在配置里的，而是查数据库：

```sql
SELECT EXISTS(SELECT 1 FROM device_information WHERE psn = $1)
```

### 校验 token

`Validate(ctx, psn, token)` 分两步：

1. 根据 PSN 派生 expected token。
2. 查询数据库确认 PSN 存在。

token 派生逻辑在 `DeriveToken`：

```go
sum := sha256.Sum256([]byte(psn + tokenSalt))
digest := hex.EncodeToString(sum[:])
return digest[28:36]
```

也就是：

```text
psn + salt
  -> SHA-256
  -> hex 编码
  -> 取中间 8 位作为 token
```

### 常量时间比较

代码用的是：

```go
subtle.ConstantTimeCompare(...)
```

这比普通字符串比较更安全，因为它尽量避免通过比较耗时泄露 token 前缀匹配情况。面试不需要展开太深，但可以说这是“避免 timing attack 的常量时间比较”。

## 为什么还要查数据库

只校验 token 不够，因为 token 是根据 PSN 派生的。如果只要 PSN/token 格式对就放行，可能会允许访问不存在或未登记的设备。

数据库校验保证：

- token 算法正确
- PSN 确实存在于 `device_information`

也就是同时验证“凭证”和“设备存在性”。

## context 设备注入

鉴权通过后，`main.go` 会把 PSN 放进请求 context。后续 handler 用 `psnFromContext(ctx)` 获取设备身份。

好处：

- 工具参数里不暴露 PSN。
- LLM 不需要自己选择设备编号。
- 一个 MCP 会话天然绑定到 URL 里的设备。

这个设计的安全收益是：模型不能通过工具参数临时切换到另一台设备。

## `SKILL.md` 是什么

`skill.go` 里实现了：

```text
/{psn}/SKILL.md
```

它不需要 token，原因是它只返回设备使用说明，不返回实时数据，也不能控制设备。

流程是：

1. 从 URL 解析 PSN。
2. 查数据库拿 `device_model`。
3. 根据型号读取 `data/skills/{MODEL}.md`。
4. 返回 markdown 给客户端。

例如型号是 `PRO`，就返回：

```text
data/skills/PRO.md
```

当前支持的 skill 文件包括：

- `BIRD.md`
- `DEVB.md`
- `PRO.md`
- `ULTRA.md`

## Skill 文件给 LLM 什么信息

以 `PRO.md` 为例，它会告诉客户端：

- 设备型号和端口信息
- 启动时应该先读哪些 prompt resource
- 常用 tool 的用途
- 控制策略的参数映射
- 端口号都是 1-indexed
- 屏幕亮度、待机显示、整点报时等用户语言如何映射到工具参数
- 失败时如何处理

这类文档的作用不是给人看的 README，而是给 LLM 客户端看的“操作手册”。

可以理解成：

```text
Tool schema 告诉模型：有哪些函数可调用
Prompt/Resource 告诉模型：某类任务的推荐流程
SKILL.md 告诉模型：这台设备的背景、规则和安全约束
```

## Prompt 和 Skill 的区别

`data/prompts/*.txt` 是任务模板，例如：

- `charging_control`：控制类任务怎么做
- `device_diagnostic`：深度诊断时应该按什么顺序调用工具
- `charging_status`：展示完整充电状态

`data/skills/*.md` 是设备级说明，例如：

- 设备有几个端口
- 支持哪些控制能力
- 哪些型号支持屏显模式
- 端口名如何展示

所以：

```text
Skill 更像设备说明书
Prompt 更像任务操作流程
Tool 更像可执行函数
```

## 安全边界总结

这个项目的安全边界主要有四层：

1. URL 层
   - 设备访问路径里必须有 `/{psn}/{token}/...`

2. 鉴权层
   - token 必须和 PSN 派生结果一致
   - PSN 必须存在于数据库

3. context 层
   - 设备身份由服务端注入，不让 LLM 在工具参数里自由传 PSN

4. tool handler 层
   - 参数 schema 限制输入类型
   - handler 检查端口号、策略值、设备返回状态

注意：这不等于“完全安全”。例如控制类工具仍然可能被模型调用，所以实际产品里还可以继续加强：

- 对高风险控制操作增加用户确认
- 增加操作审计日志
- 对公网部署增加 HTTPS、账号体系或短期 token
- 对端口开关/功率分配增加更严格的业务规则

## C++ 类比

可以类比成一个设备控制服务：

```cpp
bool validate(psn, token) {
    if (deriveToken(psn) != token) return false;
    return database.deviceExists(psn);
}

RequestContext ctx;
ctx.psn = psn;

handler(ctx, args); // handler 不再从 args 里拿设备 ID
```

`SKILL.md` 则像给上层自动化系统的一份设备能力描述文件：

```text
这个型号有 5 个端口
端口编号从 1 开始
支持这些策略
控制后需要再次查询状态确认
```

## 简历候选写法

> 基于 PSN/TOKEN 完成设备访问鉴权，并通过 context 注入设备身份，使 MCP 工具调用天然绑定单台设备；同时按设备型号返回 SKILL.md，指导 LLM 客户端选择 Prompt 和 Tool。

如果空间更紧：

> 实现 PSN/TOKEN 鉴权与 context 设备注入，避免工具参数暴露设备 ID；按设备型号提供 SKILL.md，约束 LLM 的工具调用流程和参数映射。

## 面试表达

短版本：

> 鉴权层会根据 PSN 派生 token，并查询数据库确认设备存在。鉴权通过后，服务端把 PSN 注入 context，后续 handler 从 context 读取设备身份，而不是让模型在工具参数里传设备 ID。

更强一点的版本：

> 我们把设备身份放在服务端路由和 context 层处理，而不是暴露给 LLM。这样工具 schema 更简单，也减少模型误操作其他设备的风险。`SKILL.md` 和 prompt/resource 则负责告诉模型某个设备型号应该如何调用这些工具。

## 可能被追问

1. token 是怎么生成的？
2. 为什么使用常量时间比较？
3. 为什么除了 token，还要查数据库确认 PSN 存在？
4. 为什么 `SKILL.md` 不需要 token？
5. `SKILL.md`、Prompt、Tool 的区别是什么？
6. context 注入设备身份有什么好处？
7. 这个安全方案还有哪些不足？
8. 如果要上生产，你还会补哪些安全措施？

## 练习答案

**Q：为什么不让工具参数里带 PSN？**

因为 PSN 是访问身份，不是用户任务参数。把它放在 URL 和 context 里，可以让会话绑定到具体设备，减少模型误传或切换设备的风险。

**Q：`SKILL.md` 不鉴权是否危险？**

当前 `SKILL.md` 只返回设备型号对应的使用说明，不包含实时状态，也不能控制设备，所以风险较低。但如果里面未来加入敏感信息，就应该加鉴权。

**Q：Prompt 和 Tool 有什么区别？**

Tool 是可执行能力，Prompt 是任务流程说明。比如 `get_port_pd_status` 是工具，`device_diagnostic` 是告诉模型诊断时应该按什么顺序调用工具的 prompt。

## 复习检查

- 能不能讲清楚 token 派生算法？
- 能不能解释数据库校验的作用？
- 能不能说清楚 PSN 从 URL 到 context 再到 handler 的路径？
- 能不能区分 SKILL、Prompt、Resource、Tool？
- 能不能主动说出这个安全方案还可以怎么加强？

