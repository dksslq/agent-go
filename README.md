# agent-go

一个小型终端 AI Agent：单二进制，只用 Go 标准库。连接 OpenAI 兼容的 Chat Completions API，通过三个工具执行程序、读取图片和整理记忆。

[![CI](https://github.com/dksslq/agent-go/actions/workflows/ci.yml/badge.svg)](https://github.com/dksslq/agent-go/actions/workflows/ci.yml) · [Releases](https://github.com/dksslq/agent-go/releases) · [MIT](LICENSE)

支持 Linux、macOS、Windows 的 amd64 / arm64。构建需要 Go 1.21+，无第三方依赖，无 cgo。

## 安装与运行

```bash
go install github.com/dksslq/agent-go/v2@latest
```

也可以从 [Release](https://github.com/dksslq/agent-go/releases) 下载对应平台的二进制。Debian / Ubuntu 的包名为 `agentlet`，安装后命令为 `agentlet`；Go 安装后的命令为 `agent-go`。

```bash
# 本地构建
git clone https://github.com/dksslq/agent-go.git
cd agent-go
go build -o agent .

# 设置模型与 API 地址；认证通过环境设置注入 AGENT_API_KEY
export AGENT_MODEL='your-model'
export AGENT_API_BASE='http://localhost:8080/v1'

# 交互使用
./agent

# 执行一项任务后退出
./agent -once -prompt '检查当前项目，运行测试并报告结果'

# stdin 是任务数据；-prompt 是附加指令
./agent -pipe -prompt '分析日志，给出有证据的结论' < error.log > report.md

# 载入已有会话，或新建；每个工具轮次及用户轮次结束后保存
./agent -continue session.json -once -prompt '继续检查剩余问题'
```

模型服务需要支持流式 Chat Completions 和工具调用；图片功能还需要模型支持视觉输入。环境变量 `AGENT_MODEL`、`AGENT_API_BASE`、`AGENT_API_KEY` 提供默认值，同名命令行参数覆盖它们。HTTP 代理使用 Go 的标准 `HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY` 规则；HTTPS 保留证书校验。

当前模型连接使用 OpenAI 兼容的 HTTP / SSE。stdio 是传输方式，MCP stdio 用于工具服务通信，不定义模型推理协议。接入本地推理进程需要先确定具体的消息帧、流式输出、取消与工具回合协议；目前没有额外的后端适配层。

## 自主运行的行为

`-once` 要求提供非空 `-prompt`。`-pipe` 读取 stdin 至 EOF，将它与可选的 `-prompt` 用空行连接，原样保留 stdin 的空白与文本，并隐含 `-once`。这两种模式不切换终端状态，也不打印交互提示符。

stdout 输出流式助手正文，包括工具执行前的说明；工具状态与错误走 stderr。默认日志只展示工具名称，`-verbose` 才展示推理内容、工具参数与结果。这些详细日志可能包含任务数据，请按需要开启。

| 退出码 | 含义 |
|---|---|
| `0` | 成功完成 |
| `1` | 参数、API、输入、输出或会话保存错误 |
| `130` | 被 SIGINT / Ctrl+C 中断 |
| `143` | 收到 SIGTERM 并退出 |

API 错误、断流、未完成的工具参数、模型长度截断均按错误处理。程序不会自动重试请求或重放工具。中断时已经输出的正文可能不完整，消费结果时应同时检查退出码。

同一响应中的独立工具并行执行，结果按调用顺序回传；`memory_edit` 在其他工具结束后按调用顺序执行。存在依赖的工作应拆成多个工具轮次。多个 Agent 可以由 shell、CI 或服务管理器作为独立进程调度；每个进程使用独立会话文件，避免互相覆盖。

## 工具与输出

| 工具 | 行为 |
|---|---|
| `exec` | 直接启动程序，将 `input` 写入 stdin，分别返回 stdout、stderr 与退出码；无 PTY。支持工作目录、环境、超时、后台启动与输出文件 |
| `image_read` | 读取本地常规图片文件，附加为视觉输入 |
| `memory_edit` | 用完整 `memory` 文本替换语义记忆，将已完成的历史移出 RAM 和后续 API 上下文 |

`exec` 直接执行程序，需要 shell 语法时显式启动 shell。前台执行要求正数 `timeout_seconds`，支持小数秒；`detach: true` 不等待退出、不捕获输出，并忽略超时。后台进程的后续管理由调用者负责。

`exec` 的 `result` 是 JSON 对象，例如：

```json
{"stdout":"","stderr":"test failed\n","exit_code":1}
```

`stdout`、`stderr` 始终为字符串。正常退出时 `exit_code` 是程序退出码；信号终止通常为 `-1`，未启动或后台启动时为 `null`。正常非零退出不会重复生成错误说明；超时、取消、信号终止、启动或输出 I/O 失败才附带 `error`，已经捕获的输出仍保留。后台启动成功还返回 `pid`。

输出有两种选择，由模型按任务决定：

- 未设置 `output_file` 时，默认完整回传两个流。正数 `max_output_chars` 分别限制每个流的 Unicode 字符预览；省略或 `0` 表示完整输出。
- 设置 `output_file` 时，两个流的完整原始字节写入一个**新文件**，回传 `output_file`、`output_bytes` 以及可选预览。路径相对于 `cwd`，已有文件会导致工具报错。此时省略或 `0` 的 `max_output_chars` 表示不附带预览。输出文件不能与 `detach` 同用；合并文件不保证两个流之间的先后顺序。

发生截断时才附带 `stdout_truncated: true` / `stderr_truncated: true`，程序输出本身不插入提示文字。预览达到上限后仍继续读取两个流、等待命令完成，不会因为预览截断而杀死程序。需要保留完整日志时使用输出文件，再通过 `exec` 检索相关片段：

```json
{
  "program": "go",
  "args": ["test", "./..."],
  "input": "",
  "timeout_seconds": 120,
  "output_file": "test.log",
  "max_output_chars": 4000
}
```

输出文件保留在磁盘上，需要自行归档或删除。字符数与 JSON 字节数均不是 token 数；程序不猜测模型的 tokenizer 或上下文窗口，也不暗中裁掉工具输出。

## 记忆与会话

上下文由系统提示词、模型维护的记忆和未压缩的历史组成。`memory_edit` 必须提交完整记忆；应保留当前目标、用户约束、完成事项、证据、输出文件路径与未完成工作。

简短的系统提示词明确要求模型在任务里程碑、大量输出之后、历史开始重复之前调用 `memory_edit`。这是模型的主动整理策略；程序不猜测窗口大小，也不按固定阈值触发压缩。

默认压缩所有已完成的历史，**包括同一任务中先前完成的步骤**。尚未收齐结果的工具调用组保留，调用与结果不会被游标拆开。可选 `cursor` 使用完整 transcript 的绝对索引，游标只能向前移动。

压缩后的原文进入进程私有的临时 JSONL 存档，释放内存中的历史引用，也不再随每次 API 请求发送。记忆消息附带存档路径，可以用 `exec` 找回原文。临时存档在正常退出或重载会话时清理；需要跨进程保留历史时使用 `-save` 或 `-continue`。

每个工具轮次的最后一个结果附带 `last_request_bytes`：它是刚完成推理所发送的完整请求 JSON 字节数，包含消息、工具定义与附加字段，不是 token 数或剩余窗口容量。动态观测只追加到新结果中，避免每轮改写系统与记忆前缀。记忆编辑回传游标、下一索引与当前上下文字节数。

保存的会话仍为完整、可迁移的 v3 JSON：

```json
{"version":3,"transcript":[{"role":"system","content":"..."}],"memory":"...","cursor":1}
```

保存时流式合并存档与活跃历史，使用同目录私有临时文件、刷新后替换。加载兼容旧 v3 文件与旧消息数组，并检查工具调用配对；旧数组在下次保存时转为 v3。加载旧会话时保留其中的系统提示词，新的默认规则用于新建会话。

Windows 在启动时占用 `-save` / `-continue` 选定的实际会话文件，拒绝其他读写打开；冲突立即失败，不等待或重试，也不改已有快照。保存通过已持有的句柄替换，要求文件系统支持 `FileRenameInfoEx` 的 POSIX 替换语义。允许 `FILE_SHARE_DELETE`，有删除权限的外部程序仍可重命名或删除路径。Unix 保持同目录替换，各进程应使用独立会话文件。

`-save` / `-continue` 在每个工具轮次和用户轮次结束后检查点保存，保存失败会停止当前推理并报告错误。检查点记录会话，不能把外部程序的副作用与保存操作组成事务：异常退出后应先核实已有文件、进程或服务状态，再决定是否重试。

没有固定的工具轮数、图片大小或默认输出大小上限。完整内联输出和未压缩的活跃上下文仍会消耗内存与模型额度；存档与输出文件会占用磁盘。持续任务应使用文件输出并主动整理记忆，CPU、内存、磁盘和进程权限由运行环境配置。

## 参数与交互

| 参数 | 默认 / 用途 |
|---|---|
| `-model` | `AGENT_MODEL`；必须指定模型 |
| `-api-base` | `AGENT_API_BASE`，否则 `http://localhost:8080/v1` |
| `-api-key` | `AGENT_API_KEY`，否则为空 |
| `-prompt` / `-once` / `-pipe` | 启动提示词、单次执行、读取 stdin |
| `-load` / `-save` / `-continue` | 载入已有文件、保存检查点、载入或新建并保存至同一文件 |
| `-verbose` | 默认关闭；显示推理、工具参数与结果 |
| `-stream-idle-timeout` | `5m`；等待网络数据的空闲超时，`0` 关闭；`-tool-call-idle-timeout` 为兼容别名 |
| `-http-idle-timeout` | `5m`；空闲 HTTP 连接保留时间 |
| `-http-max-idle-conns` | `100`；保留的空闲 HTTP 连接数 |
| `-reasoning-field` / `-content-field` | delta 字段名，默认 `reasoning_content` / `content` |
| `-tool-calls-field` / `-finish-reason-field` | 默认 `tool_calls` / `finish_reason` |
| `-extra` | 可重复的 Base64 JSON 对象；附加厂商参数，不能覆盖 `model`、`messages`、`tools`、`stream` |

`-stream-idle-timeout` 覆盖请求连接、响应头以及后续网络读取等待，不限制一个持续产生数据的任务总时长。Go 标准传输层的连接与 TLS 握手超时仍然适用。

终端按 Enter 发送。支持 bracketed paste 的终端会保留粘贴中的换行、缩进和特殊字符，粘贴结束后仍需按 Enter；Alt+Enter（以及终端支持时的 Shift+Enter）插入换行。旧终端先输入 `/paste`，再输入或粘贴多行，按 Ctrl+D 发送。多行文本不会被解释为交互命令。键盘 Ctrl+C 仍按中断处理。

交互命令：`/paste`、`/help`、`/image <path>`、`/image-url <url>`、`/pending`、`/clear`、`/memory`、`/save <file>`、`/reload <file>`，以及 `exit` / `quit`。`/memory` 显示记忆、游标与上下文 JSON 字节数。Ctrl+C 中断当前推理轮，空闲时退出；Ctrl+D 结束输入。SIGTERM 结束整个进程。

Linux / macOS 的前台命令在独立进程组中运行，取消时终止该进程组。Windows 的取消只保证终止直接子进程；需要管理完整进程树时由外部运行环境负责。任何平台的进程控制都不能保证立即结束不可中断的内核操作。

`exec` 继承 Agent 的操作系统权限。生产部署应使用符合任务需要的用户、目录权限和隔离环境。工具输出与记忆作为数据处理，用户授权的文档可用于完成任务；系统提示词不能代替操作系统的权限控制。

## 开发与验证

```bash
gofmt -l .
go vet ./...
go build .
go test -count=1 -race -shuffle=on -covermode=atomic ./...
```

CI 在 Linux 的 Go 1.21 与 stable、macOS stable、Windows stable 上运行检查与竞态测试，并交叉编译 Linux / macOS / Windows × amd64 / arm64。测试使用标准库 `testing`、`httptest` 和真实子进程；Linux 还使用 PTY 验证行输入。发布工作流由 `v*` 标签触发，构建平台二进制与 Debian 包。

代码按职责拆分：`cli.go` 处理入口与信号，`agent.go` 处理推理与工具，`openai.go` 处理 API / SSE，`memory.go` 处理会话与存档，`exec_output.go` 处理输出捕获，`output.go` 处理可取消的标准输出，`exec_*.go` / `term_*.go` / `session_*.go` 提供平台适配。它们构建为同一个可执行文件。

历史变更见 [CHANGES.md](CHANGES.md)。运行时审计见 [v2.1.0](docs/release-v2.1.0.md)，提示词与会话占用修订见 [v2.1.1](docs/release-v2.1.1.md)，安装修订见 [v2.1.2](docs/release-v2.1.2.md)，终端与资源清理见 [v2.1.3](docs/release-v2.1.3.md)，命令结果格式见 [v2.2.0](docs/release-v2.2.0.md)。

## 许可与署名

[MIT](LICENSE)。开发者：DeepSeek、GLM (Z.ai)、Codex (OpenAI，审计、重构与测试)。
