<div align="center">

# ⚡ agent-go

**一个单二进制、零依赖的终端 AI Agent —— ~1600 行纯 Go（标准库）**

流式输出 · 工具调用 · 多模态 · 会话持久化 · 跨平台

[![Go](https://img.shields.io/badge/Go-1.21%2B-00ADD8?logo=go&logoColor=white)](#-快速开始)
[![Platform](https://img.shields.io/badge/platform-linux%20%7C%20macOS%20%7C%20Windows-lightgrey)](#-快速开始)
[![Deps](https://img.shields.io/badge/third--party%20deps-zero-3fb950)](#-特性)
[![Tests](https://img.shields.io/badge/tests-39%20pass%20%C2%B7%200%20test%20deps-3fb950)](#-测试与质量)
[![License](https://img.shields.io/badge/license-MIT-green)](#-许可)
[![Designed by](https://img.shields.io/badge/designed%20by-DeepSeek%20%C3%97%20GLM-ff7b72)](#-designed-by-deepseek--glm)
[![CI](https://github.com/dksslq/agent-go/actions/workflows/ci.yml/badge.svg)](https://github.com/dksslq/agent-go/actions/workflows/ci.yml)

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
| 🤖 **全自主可编排** | 人机模式与全自主模式同一二进制：`-once` 内联一句话，`-pipe` 让 stdin 成为数据流——stdin 进、stdout 出、处理完即退、退出码可编排；**并发就是 N 个 OS 进程**，xargs / cron / systemd 任意编排 |
| 🛡 **防御性默认** | `-max-tool-rounds` 工具轮次护栏（默认 25，对齐 OpenAI Agents SDK / LangChain 惯例）、工具流空闲超时、媒体体积上限、panic 恢复并自动还原终端状态 |
| 🔌 **即插即用** | 直连任何 **OpenAI 兼容 API**（GLM / DeepSeek / OpenAI / vLLM / Ollama…）；`-extra` 以 Base64 注入任意 JSON 请求字段，厂商私有参数全兼容 |

## 🎯 适用环境与安全模型

**为受信任的本地环境设计**：个人开发机、工业生产内网主机、封闭运维终端——你合法拥有、且有必要让模型直接操作这台机器的场景。

**agent 本身零权限限制**：`exec` 运行任意程序、继承进程的完整 OS 权限——无沙箱、无白名单、无确认弹窗。这是刻意的最小设计：权限决策交给使用者与外部机制，而不是内置一套可被提示词绕过的伪防线（提示词层只做一件事：声明工具输出不可信，防注入指令）。

**限制必须来自外部**——与对待 shell 完全同理：

| 层 | 手段 |
|---|---|
| OS | 专用低权用户运行；按需收敛文件系统权限 |
| 隔离 | 容器 / namespace / chroot / 独立虚拟机 |
| 网络 | 防火墙收敛出站（程序自身只需访问模型 API 一条） |

> 把 agentlet 当作 shell 的等价物：它不是玩具，也不要让它接触不可信来源的输入。

## 🤖 自主模式与并发生产

同一二进制，两种形态：

| 形态 | 入口 | 特征 |
|---|---|---|
| **人机协作** | 直接运行 | raw mode 交互、Ctrl+C 中断、`/` 命令、会话续聊 |
| **全自主** | `-once` / `-pipe` | 无终端假设：stdin 进、stdout 出、处理完即退、退出码可编排 |

`-pipe` 让 agentlet 成为标准 Unix 过滤器——`-prompt` 是指令，stdin 是数据：

```bash
cat error.log | agentlet -model glm-4.7 -api-base https://api.z.ai/api/paas/v4 -api-key "$ZAI_API_KEY" \
        -pipe -prompt "定位根因，给出修复命令"

agentlet -model glm-4.7 -api-base https://api.z.ai/api/paas/v4 -api-key "$ZAI_API_KEY" \
        -pipe < task.md > result.md
```

**输出纪律**：stdin 非终端（管道 / 重定向）时，stdout 只有模型输出，启动信息与错误全部走 stderr；退出码 `0` 正常 / `1` 出错 / `130` 被中断——三种身份都能被脚本直接消费。

**并发就是进程**，刻意不内置线程池与队列：N 个 agentlet = N 个 OS 进程，交给久经考验的 Unix 编排器，无共享状态、无锁、崩溃互不传染——

```bash
# 100 个任务、8 路并行，每路一个自治 agent
ls tasks/*.md | xargs -P 8 -I{} sh -c 'agentlet -model glm-4.7 -api-base https://api.z.ai/api/paas/v4 \
        -api-key "$ZAI_API_KEY" -pipe < {} > {}.out'
```

适合放进去的场景：**cron / CI 步骤 / systemd timer 里的自治工人**，**工业内网数据管道的加工工序**，**xargs · make -j 式批量并行分片**。配合 `-continue` 断点续跑；配合逐字节恒定的系统提示词吃满模型端前缀缓存——大规模并发运行的计费最优解。

> 人机模式让人掌舵，自主模式让机器干活——并发与容错交给操作系统，这是 Unix 的做法。

## 🔭 场景推演：当模型足够可靠

下面只做推演，不跑分：**用到的原语今天全部存在**，唯一变量是模型的可靠性。每项"未来能力"都对应一个现有机制——

| 愿景里的说法 | 今天就有的原语 |
|---|---|
| 主 agent 自主控制并发 | `exec` 本来就能启动任意程序——包括再启动 N 个 `agentlet -pipe` 子代理 |
| 主 agent 记忆 | `-continue` 的会话文件就是磁盘上的纯 JSON，主 agent 可用 `exec` 读写自己的记忆 |
| 侧 agent 压缩对话 | 会话 JSON 管道给侧 agentlet 摘要，校验后原子替换回主会话 |
| 重生 | systemd `Restart=on-failure` / cron 重拉，`-continue` 从最近一轮自动存档续跑 |
| 自治 | 退出码 + 每轮自动存档 + 纯净 stdout（自主模式 stdout 只有模型输出，其余走 stderr） |

### 例子：工厂班次交付与报表（流程描述，非跑分）

一台内网主机、一个低权用户、一个 systemd timer：

```ini
# /etc/systemd/system/shift-report.timer
[Timer]
OnCalendar=*-*-* 06:00

[Install]
WantedBy=timers.target
```

```ini
# /etc/systemd/system/shift-report.service
[Service]
Type=oneshot
User=agentlet
EnvironmentFile=/etc/agentlet/shift.env
ExecStart=/usr/bin/agentlet -model ${MODEL} -api-base ${API_BASE} -api-key ${API_KEY} \
          -continue /var/lib/agentlet/shift.session \
          -pipe -prompt "新班次开始。读取队列任务，全部完成后生成交付报表。"
Restart=on-failure
RestartSec=30
```

发生了什么（每一步都是上表中的原语）：

1. **06:00 systemd 拉起主 agent**：`-pipe` 把当日任务队列表作为数据注入，`-continue` 载入昨天的会话——这就是主 agent 的长期记忆；
2. 主 agent 用 `exec` 读到产线系统导出的本地 CSV 后，**自己决定分片与并发**（例如 `ls /data/line*.csv | xargs -P 3 -I{} … agentlet -pipe < {} > {}.summary`）——分几路、怎么分，是模型在会话里定的，不是框架定的；
3. 收齐侧 agent 摘要后生成交付报表写入 `/srv/reports/`，投递走主机上已有通道（邮件 / 内部网关）——agentlet 自身只有模型 API 一条网络连接，grep 可审计；
4. **进程夜间被杀也没关系**：`Restart=on-failure` 重拉，`-continue` 从最近一轮自动存档继续，已完成的轮次不重做——这就是重生；
5. 会话越长越贵：主 agent 定期把 `shift.session` 管道给一个侧 agentlet 压缩成最小状态（"保留目标、未完成事项、关键事实，只输出 JSON 数组"），`python3 -m json.tool` 校验通过后原子替换——这就是侧 agent 压缩对话；上下文不无限膨胀，前缀缓存依然命中。

护栏全在进程外（见「适用环境与安全模型」）：低权用户、报表目录最小写权限、出站防火墙只放行模型 API。这套流程今天就能部署——**它能不能跑得稳，取决于模型的可靠性；而这正是 agentlet 被设计成"无魔法、可 grep、可重生"的原因：把不可靠的部件，放进一个可观察、可重生、权限受控的骨架里。**

## 🚀 快速开始

**安装**

| 方式 | 说明 |
|---|---|
| Go | `go install github.com/dksslq/agent-go@latest`（Go 1.21+，秒级构建） |
| Debian / Ubuntu | Release 页下载 `.deb` → `apt install ./agentlet_*_amd64.deb`（**Debian 包名 agentlet**——nano-sized 的终端 agent，二进制 `/usr/bin/agentlet`） |
| 源码 | `git clone` → `go build -o agent .` |

> 命名：同域检索发现 "nano-agent" 已被占用（GitHub MCP Server / PyPI 同名包），故取 **agentlet**（agent + -let，piglet / droplet 同构）；"nano" 保留为描述词。

**运行**

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

# 全自主管道：-prompt 为指令，stdin 为数据，处理完即退
cat error.log | ./agent -model glm-4.7 -api-base https://api.z.ai/api/paas/v4 \
        -api-key "$ZAI_API_KEY" \
        -pipe -prompt "定位根因，给出修复命令"
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
| `-pipe` | — | 读取 stdin 至 EOF 作为提示词内容；与 `-prompt` 组合（指令 + 空行 + 数据）；隐含 `-once` |
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

全部测试仅用标准库 `testing` + `httptest`，**零测试依赖**，`go test .` 一条命令全跑：

| 套件 | 用例 | 覆盖 | 平台 |
|---|---|---|---|
| `agent_test.go` | 23 函数 / 50 用例 | 会话存档 round-trip（多模态 / tool_calls / tool 成对）；空、坏、缺、坏路径文件；**系统提示词完全静态——ENV / TZ / OS / 时间戳 / PID / 变量一律不进提示词（跨运行逐字节恒定，前缀缓存天然全命中），环境按需 `exec` 实时探测**；`-pipe` 语义（指令 × stdin 组合）；**execCmd 真实进程端到端**（stdout+stderr 合流 / stdin 注入 / 超时击杀 / 输出截断 / cwd / environ / 非零退出 / detach，unix）；runTool 分发与参数错误路径；unquote_arguments / quote_result 端到端；参数解析（timeout / environ / buildCmdEnv / unquote 递归 / rune 边界截断）；用户消息组装（媒体块顺序）；转义序列吞除（管道模式）；控制字符；媒体读取；sanitize；ANSI 剥离 | 全平台* |
| `sse_test.go` | 13 函数 | SSE 全链路（httptest 假端点）：reasoning / content 分流；tool_calls 乱序分片累积、finish 触发与 EOF 兜底、`[DONE]` 停读；自定义字段映射；坏行跳过；API 错误；断连；ctx 取消；请求形状（model / tools / auth / `-extra` 合并）；**工具轮次护栏端到端**（assistant 与 tool 消息严格成对） | 全平台 |
| `pty_readline_test.go` | 3 用例 | 真实 `/dev/ptmx` 伪终端 + raw mode 驱动真实 `readLine`：方向键 / Home / Delete 不污染输入、纯文本、退格编辑 | linux |

平台差异几乎只有终端特性：输入解析、流解析、会话、工具辅助均在纯管道层验证（全平台），仅终端行编辑需 PTY 实测（linux）。*exec 真实进程用例（`sh`）在 windows 上自动跳过，其余全平台。

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
├── agent_test.go          # 全平台单测：会话 / 提示词静态守卫 / 输入 / 媒体 / 工具辅助
├── sse_test.go            # 全平台单测：SSE 全链路（httptest）/ 轮次护栏端到端
├── pty_readline_test.go   # PTY 输入回归实测（linux only）
├── go.mod
├── debian/                # Debian 打包（dh-golang）：control / rules / changelog / copyright
├── docs/demo.svg          # 效果演示
├── CHANGES.md             # 修复说明
├── .github/workflows/ci.yml       # CI：vet + build + PTY test + 6 平台交叉编译 + .deb 打包（lintian）
└── .github/workflows/release.yml  # tag v* → 6 平台二进制 + .deb 自动挂 Release
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

CI 徽章已指向 `dksslq/agent-go`，fork 后把 README 顶部的仓库名改为你自己的即可。

## 🧬 Designed by DeepSeek × GLM

<div align="center">

**本程序由 [DeepSeek](https://www.deepseek.com/) 与 [GLM (Z.ai)](https://z.ai/) 联合设计。**

一个由 AI 为 AI 写的终端 Agent —— 架构与代码出自 **ds × glm** 双模型协作，
评审、实测修复（编译矩阵 / PTY 复现回归）与本主页亦然。

*Less code, more agent.*

</div>

## 📄 许可

[MIT](LICENSE) © DeepSeek × GLM (Z.ai)
