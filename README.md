<div align="center">

# ⚡ agent-go

**一个单二进制、零依赖的终端 AI Agent —— ~1600 行纯 Go（标准库）**

流式输出 · 工具调用 · 多模态 · 会话持久化 · 跨平台

[![Go](https://img.shields.io/badge/Go-1.21%2B-00ADD8?logo=go&logoColor=white)](#-快速开始)
[![Platform](https://img.shields.io/badge/platform-linux%20%7C%20macOS%20%7C%20Windows-lightgrey)](#-快速开始)
[![Deps](https://img.shields.io/badge/third--party%20deps-zero-3fb950)](#-特性)
[![Tests](https://img.shields.io/badge/PTY%20tests-3%2F3%20pass-3fb950)](#-测试与质量)
[![License](https://img.shields.io/badge/license-MIT-green)](#-许可)
[![Designed by](https://img.shields.io/badge/designed%20by-DeepSeek%20%C3%97%20GLM-ff7b72)](#-designed-by-deepseek--glm)

<img src="docs/demo.svg" alt="agent-go 终端演示：流式回复、工具调用、会话持久化" width="820"/>

</div>

---

## ✨ 特性

| | |
|---|---|
| 🪶 **轻到极致** | 核心逻辑单文件 `agent.go`（~1600 行），**只用 Go 标准库、零第三方依赖、无 cgo**，交叉编译产出单文件二进制 |
| 🌊 **真流式** | SSE 逐 token 输出；`-reasoning-field` 等 4 个 delta 字段名可映射，**原生兼容 DeepSeek-R1 等推理模型**，思考流与正文分流呈现 |
| 🔧 **工具调用** | `exec`（stdin 注入 / 超时 / 后台 detach / cwd / 环境注入 / 输出限长）、`image_read`、`video_read`；**同轮多工具并行执行** |
| 🖼 **多模态** | `/image` `/image-url` `/video` `/video-url` 附件随下一条消息发送——让模型"看得见图、看得了视频" |
| 💾 **会话持久化** | `-save` / `-load` / `-continue` 一键续聊，`/save` `/reload` 在线切换；每轮结束自动存档，对话历史永不丢 |
| ⌨️ **终端原生** | raw mode 行内编辑（退格即见即所得）；**Ctrl+C 中断当前推理轮、再按退出**；转义序列安全吞除——方向键 / Home / Delete 不会污染输入（有 PTY 实测背书） |
| 🛡 **防御性默认** | `-max-tool-rounds` 工具轮次护栏（默认 25，对齐 OpenAI Agents SDK / LangChain 惯例）、工具流空闲超时、媒体体积上限、panic 恢复并自动还原终端状态 |
| 🔌 **即插即用** | 直连任何 **OpenAI 兼容 API**（GLM / DeepSeek / OpenAI / vLLM / Ollama…）；`-extra` 以 Base64 注入任意 JSON 请求字段，厂商私有参数全兼容 |

## 🚀 快速开始

```bash
git clone https://github.com/your-username/agent-go.git
cd agent-go
go build -o agent .          # 零依赖，秒级构建
```

```bash
# 交互模式（-continue 自动续档 + 存档）
./agent -model glm-4.7 \
        -api-base https://api.z.ai/api/paas/v4 \
        -api-key  "$ZAI_API_KEY" \
        -continue dev.session

# 一次性提问（脚本友好）
./agent -model glm-4.7 -api-base https://api.z.ai/api/paas/v4 \
        -api-key "$ZAI_API_KEY" \
        -prompt "用一句话介绍你自己" -once
```

## ⌨️ 交互命令与按键

| 命令 | 作用 |
|---|---|
| `/image <path>` / `/image-url <url>` | 追加图片到下一条消息 |
| `/video <path>` / `/video-url <url>` | 追加视频到下一条消息 |
| `/pending` · `/clear` | 查看 / 清空待发媒体 |
| `/save <file>` · `/reload <file>` | 保存 / 重载会话 |
| `exit` · `quit` · `Ctrl+D` | 结束会话 |

| 按键 | 行为 |
|---|---|
| `Ctrl+C`（推理中） | 中断当前轮，已生成内容与 tool_calls 成对入档 |
| `Ctrl+C`（空闲时） | 退出（退出码 130） |
| `Backspace` | 删除上一个字符（raw mode 实时回显） |
| 方向键 / Home / End / Delete | 安全吞除，不污染输入 ✅ |

## 🧰 内置工具

| 工具 | 能力 |
|---|---|
| `exec` | 启动任意程序并向 **stdin 写入字符串**；必填 `timeout_seconds`；支持 `cwd`、`environ`、`max_output_chars` 截断、`detach` 后台运行、`unquote_arguments` / `quote_result` 转义控制 |
| `image_read` | 读取本地图片 → base64 data URI，模型直接"看图" |
| `video_read` | 读取本地视频 → base64 data URI，模型直接"看片" |

## ⚙️ 参数一览

| 参数 | 默认 | 说明 |
|---|---|---|
| `-model` | （必填） | 模型名 |
| `-api-base` | `http://localhost:8080/v1` | OpenAI 兼容 API 地址 |
| `-api-key` | 空 | API Key |
| `-prompt` / `-once` | — | 非交互单问；`-once` 处理完即退 |
| `-load` / `-save` / `-continue` | — | 启动载入 / 逐轮自动存档 / 续聊（同一文件自动双向） |
| `-reasoning-field` `-content-field` `-tool-calls-field` `-finish-reason-field` | `reasoning_content` 等 | 流式 delta 字段名映射，适配不同厂商 |
| `-max-tool-rounds` | `25` | 每轮用户输入的工具循环上限（`0` 不限；对齐主流 Agent 框架惯例） |
| `-max-file-bytes` | `0` | 媒体文件大小上限 |
| `-max-exec-output-chars` | `0` | exec 输出默认截断 |
| `-tool-call-idle-timeout` | `5m` | 工具参数流空闲超时（防模型断流挂死） |
| `-extra` | 可重复 | Base64(JSON) 合并进请求体，私有参数全兼容 |

## 🔒 安全与审计

两条硬约束**可 grep 复核**：

- **网络**：程序自身仅 `chatStream` 访问模型 API（`apiBase + "/chat/completions"`），无任何其他网络请求；
- **文件**：仅 `readMedia`（工具用）与 `saveSession` / `loadSession`（会话存档）读写文件，无其他 IO。

```bash
rg -n 'http\.' agent.go      # 网络面：只有 chatStream
rg -n 'os\.(Open|Create)' agent.go   # 文件面：只有媒体与会话
```

## 🧪 测试与质量

- **编译矩阵**：linux / darwin / windows × amd64 / arm64 全通过，`go vet` 三平台零告警，`gofmt` 干净；
- **PTY 实测**（`pty_readline_test.go`，真实 `/dev/ptmx` 伪终端 + raw mode 驱动真实 `readLine`）：
  - `↑` + `Home` + `Delete` + `ok` → 读到 `"ok"`（修复前为 `"[A[H[3~ok"`，转义残片污染输入的 bug 实锤复现）
  - 纯文本、退格编辑回归通过（`go clean -testcache && go test -count=1` 非缓存复跑）
- **补丁可信**：`agent-fix.patch`（5 hunk、20 行纯新增）经过 **原始文件 + patch == 修复版** 逐字节往返校验。

```bash
go test -v .        # 仅 Linux，需要 /dev/ptmx；不需要可直接删除该测试文件
```

历史改动详见 [CHANGES.md](CHANGES.md)，最小修复补丁见 [agent-fix.patch](agent-fix.patch)。

## 📦 目录结构

```
agent-go
├── agent.go               # 核心逻辑（~1600 行）：流式推理、工具循环、会话、raw mode 输入
├── term_linux.go          # 平台适配：raw mode（Linux/macOS/Windows 各一）
├── term_darwin.go
├── term_windows.go
├── exec_unix.go           # 平台适配：进程组（Unix）/ Windows 空实现
├── exec_windows.go
├── pty_readline_test.go   # PTY 输入回归实测（linux only）
├── go.mod
├── docs/demo.svg          # 效果演示
├── CHANGES.md             # 修复说明
└── .github/workflows/ci.yml   # CI：vet + build + PTY test + 6 平台交叉编译
```

## 🚢 发布到 GitHub

```bash
cd agent-go
git init -b main
git add .
git commit -m "feat: agent-go — 单二进制终端 AI Agent（designed by DeepSeek × GLM）"
git remote add origin git@github.com:<your-username>/agent-go.git
git push -u origin main
```

push 后把下面这行加回 README 顶部徽章区，CI 状态徽章即自动点亮（`.github/workflows/ci.yml` 已内置 vet + build + PTY 实测 + 6 平台交叉编译）：

```markdown
[![CI](https://github.com/<your-username>/agent-go/actions/workflows/ci.yml/badge.svg)](https://github.com/<your-username>/agent-go/actions/workflows/ci.yml)
```

## 🧬 Designed by DeepSeek × GLM

<div align="center">

**本程序由 [DeepSeek](https://www.deepseek.com/) 与 [GLM (Z.ai)](https://z.ai/) 联合设计。**

一个由 AI 为 AI 写的终端 Agent —— 架构与代码出自 **ds × glm** 双模型协作，
评审、实测修复（编译矩阵 / PTY 复现回归）与本主页亦然。

*Less code, more agent.*

</div>

## 📄 许可

[MIT](LICENSE) © DeepSeek × GLM (Z.ai)
