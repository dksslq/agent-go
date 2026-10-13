# v2.1.2：自主运行审计与安装修正

修正 Go 模块路径的主版本错误。旧路径不能通过 `v2` 标签安装；现在使用：

```sh
go install github.com/dksslq/agent-go/v2@v2.1.2
```

命令仍为 `agent-go`；Debian 包的命令仍为 `agentlet`。运行参数和完整 v3 会话兼容。

包含 [运行时审计](https://github.com/dksslq/agent-go/blob/v2.1.2/docs/release-v2.1.0.md) 和 [提示词与会话修订](https://github.com/dksslq/agent-go/blob/v2.1.2/docs/release-v2.1.1.md) 的全部修复：

- 严格 SSE 完成校验、错误退出、信号清理、进程回收及每工具轮检查点。
- 大结果可写入完整原始输出文件；`memory_edit` 将已完成历史移到磁盘，释放 RAM 和后续请求中的原文。
- 工具定义与系统说明减少 42.7% 的 JSON / UTF-8 字节，明确记忆整理时机；反馈实际请求字节数，避免猜测 token 或窗口。
- Windows 启动时占用所选会话文件的读写权限，基于句柄替换快照；冲突立即失败，无全局进程互斥或自动重试。
- 仅 Go 标准库、三个工具，无固定工具轮数或文件大小上限。模型连接保持 OpenAI 兼容 HTTP / SSE。

CI 验证 Linux Go 1.21 / stable、macOS、Windows 的竞态测试、六目标构建与 Debian 包。发布附六个平台二进制、amd64 Debian 包和 SHA256SUMS。
