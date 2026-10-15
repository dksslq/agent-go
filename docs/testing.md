# 测试与验证

## 测试布局

测试使用 Go 标准库，和生产代码同属 `package main`，因此保留在仓库根目录以验证未导出的内部状态。测试辅助函数只放在 `*_test.go` 中，不编入发布二进制。

| 文件或分组 | 重点 |
|---|---|
| `cli_regression_test.go`、`cli_test_helpers_test.go` | 真实 CLI 子进程、退出码、参数、输入输出、可选覆盖率构建 |
| `terminal_*_test.go`、`pty_readline_test.go` | 行输入、粘贴、终端恢复；Linux PTY 测试有平台限定 |
| `runtime_boundaries_test.go` | 10 MiB 文本、超过 4 MiB 的有效 PNG、会话状态拒绝、旧字段跳过、空轮次重试、失败时保留记忆、取消不产生输出 |
| `session_*_test.go`、`memory_regression_test.go`、`format_migration_regression_test.go`、`persistence_command_test.go`、`no_temp_regression_test.go` | 保存、恢复、压缩、旧格式兼容、平台文件语义 |
| `exec*_test.go`、`output_test.go` | 命令输出、预览、取消、进程回收及平台行为 |
| `sse_test.go`、`stream_regression_test.go`、`round_retry_regression_test.go`、`request_feedback_test.go`、`context_protocol_test.go`、`history_test.go`、`agent_test.go` | 流协议、工具回合、请求、历史与通用辅助函数 |

## 本地与 CI

需要 Go 1.21+；`-race` 还需要当前平台支持竞态检测及可用的 C 编译器。测试使用本地 HTTP 模拟服务，不需要模型密钥，不会主动请求真实模型服务。

```bash
gofmt -l .
go vet ./...
go build .
go test -count=1 -race -shuffle=on -gcflags=all=-d=checkptr=2 -timeout=6m ./...
```

`gofmt -l .` 应无输出。CI 使用 Go 1.21 与 stable，并在 Linux、macOS、Windows 上验证；另外交叉编译六个平台组合并检查 Debian 包。详见 [CI 配置](../.github/workflows/ci.yml)。交叉编译成功不等同于在该平台运行测试。

## 包含 CLI 子进程的覆盖率

普通 `go test -coverprofile` 只统计测试进程；真实 CLI 是另外构建的子进程，默认不会进入该报告。`AGENT_TEST_COVERDIR` 仅供测试辅助函数使用，开启 CLI 覆盖率构建并向子进程传递 `GOCOVERDIR`，不改变生产程序。

下面命令在仓库根目录执行，适用于 Bash 环境。每次创建全新目录，避免混入旧运行数据；需要 Go 工具链、`awk` 和 `sort`。

```bash
coverage_dir=$(mktemp -d)
mkdir "$coverage_dir/cli"
AGENT_TEST_COVERDIR="$coverage_dir/cli" go test -count=1 -race -shuffle=on \
  -covermode=atomic -coverprofile="$coverage_dir/unit.out" -timeout=6m ./...
go tool covdata textfmt -i="$coverage_dir/cli" -o="$coverage_dir/cli.out"
# 同一源码块合并命中次数；不能直接拼接，否则会重复统计分母。
{
  echo 'mode: atomic'
  awk '$1 != "mode:" { key = $1 " " $2; hits[key] += $3 }
       END { for (key in hits) print key, hits[key] }' \
    "$coverage_dir/unit.out" "$coverage_dir/cli.out" | sort
} > "$coverage_dir/combined.out"
go tool cover -func="$coverage_dir/combined.out"
# 可选：查看逐行覆盖情况
go tool cover -html="$coverage_dir/combined.out" -o="$coverage_dir/coverage.html"
```

前一步失败时应先修复失败，不使用残缺报告。CI 当前上传的是普通测试进程的覆盖率；上述合并报告是额外的本地统计方式。

在 v2.5.2 生产代码上，本次新增测试在 Linux / Go 1.27.2 下的语句覆盖率为：测试进程 74.9%，合并 CLI 子进程后 89.1%（1755 / 1969 条语句）。Go 1.21.13 与 Go 1.27.2 全量竞态测试通过。数字受平台、Go 版本、执行路径影响，不作为固定门槛。

剩余未覆盖部分包括文件 I/O 错误、旧格式的部分无效结构、异常恢复和关闭时序等路径；平台专属源文件只在对应平台参与统计。覆盖率既不表示功能完成比例，也不能证明没有内存泄漏、竞态或无限运行风险。大输入测试验证实际样本完整性，不证明任意大小都能成功；程序不设统一内容大小上限，仍受可用内存、磁盘和模型服务约束。

## 真实模型对话测试

真实服务测试与离线回归分开执行。认证通过运行环境注入，不提交密钥、原始私人会话或临时输出。DeepSeek 验证按一个 Agent 会话一个会话顺序运行，服务请求并发保持为 1；同一响应内的工具并行是另一个独立行为。

检查基础流式对话、工具参数与实际副作用、超时/取消/后台运行、完整文件输出与 Unicode 预览、图片附件、记忆整理与回溯、另存/只读加载/持续保存/跨进程恢复、管道输入、交互命令及退出码。应同时核对会话 JSON 和文件/进程结果，不能只看模型说“成功”。

2026-10-15 对 v2.5.2 的串行复测中，`deepseek-flash` 与 `deepseek-v4-pro` 的运行检查通过，未出现 503；共检查 26 个完整 JSON 会话文件。图片理解有例外：flash 正确识别测试图；pro 在 `image_read` 和直接图片附件两条路径均收到完整附件，但回复 `[Unsupported Image]`，因此 pro 的视觉理解未通过。请求被接受不等于模型看懂图片。这份记录不是永久可复现的服务能力保证，也不计入上面的离线代码覆盖率。
