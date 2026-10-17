# v2.5.3：思维链传回与中止反馈角色

修复一个真实长期运行场景：DeepSeek thinking 模式 + 工具调用的会话中，自纠错重试被 API 以 400 `The reasoning_content in the thinking mode must be passed back to the API` 拒绝，程序按致命错误退出。用户实测形态：自主任务运行 64 轮（`last_request_bytes` 212KB）后模型输出畸形工具调用，自纠错记录中止标记并重试——重试请求被 400 拒绝。

## 根因

DeepSeek 官方规则（thinking 模式指南）：**请求携带 `tools` 参数时，之前所有轮次的 `reasoning_content` 必须完整传回 API**，否则 400。两个叠加缺陷：

1. agent-go 只在 `-verbose` 下**显示**思维链增量，从不收集、不保存、不传回；
2. 中止反馈 `[Turn aborted: …]` 以**合成的 assistant 消息**写入并传回——该消息没有 `reasoning_content`。它追加在会话尾部并立即重试，使请求以一条无思维链的 assistant 消息**结尾**，正是服务端校验拒绝的形态。

此前 63 轮正常的原因：正常轮次的请求尾部始终是 `user` 或 `tool` 消息（等待模型响应），避开了校验点；只有中止反馈的合成 assistant 恰好落在尾部。

## 修复

- **收集与传回**：`EventReasoning` 增量无条件累积到 `reasoning` builder；正常回答轮与工具调用轮的 assistant 消息携带 `reasoning_content` 保存（`Message` 新增 `reasoning_content,omitempty` 字段），随会话文档持久化、`-continue` 恢复并在后续请求中传回——官方要求的正确姿势。流中从不出现思维链的 provider（非 thinking 模型）看不到该字段，请求字节零变化。
- **中止反馈改为 user 角色**：`recordTurnAbort` 写 `user` 消息（标记文本不变，流中已收到的部分输出仍拼接在前）。thinking 模式 API 对 user 消息无思维链要求，重试请求不再以裸 assistant 结尾。中止轮已收到的半截思维链随该轮输出一并丢弃（不构成"完整传回"）。
- 中断标记 `[Interrupted by user]` 走同一路径，一并修正。

## 验证

- 单元回归：`TestMalformedToolCallRetriesInsteadOfStopping` 升级——断言重试请求的 messages **尾部不是 assistant**（直接验证 400 触发形态的消除）、中止标记在 user 反馈中、assistant 消息带回流式 reasoning；`TestReasoningContentPersistsAndReturns` 新增——流式收集 → assistant 消息 → 会话文档 → 重新加载 → 请求序列化的全链路 `reasoning_content` 断言；非 assistant 消息永不携带该字段。
- mock e2e（真实二进制 + 逐请求体记录）：完整复现用户场景链——正常工具轮（reasoning + tool_calls）→ 工具结果 → 畸形工具调用（用户报告的截断形态）→ 中止反馈 → 自愈回答。验证请求 2 的 assistant(tool_calls) 带 `reasoning_content`；请求 3（用户报 400 的形态）尾部为 user 反馈、所有 assistant 带思维链；会话文档 `m2`/`m5` 持久化 reasoning、`m4` 为 user 反馈；exit 0。
- 全量测试含 `-race` 通过；`gofmt`、`go vet` 清洁。
- 注：用户提供的临时 DeepSeek 凭据已失效，真实 API 回归无法执行；协议形态由官方文档规则 + 上述请求体级断言覆盖。

## 兼容性

- 旧会话文档（无 `reasoning_content` 字段）照常加载；继续会话时新轮次开始携带。
- 非 thinking provider：流无 reasoning 增量 → 字段从不出现 → 请求与 v2.5.2 逐字节一致。
- 中止反馈角色变化对模型语义无影响：系统提示词对 `[Turn aborted: …]` 的解释不变（被中止的轮次未执行任何工具调用）。
