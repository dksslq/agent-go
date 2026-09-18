# agent-go 修复包

基于您上传的 6 个源文件制作。**仅 `agent.go` 有改动**，其余 5 个文件与原始上传逐字节一致。

## 改动 1：修复方向键/编辑键污染输入（实测复现的 bug）

**现象**（已用 `/dev/ptmx` 真实伪终端 + raw 模式运行真实 `readLine` 复现）：
按 `↑` / `Home` / `Delete` 后输入 `ok` 回车，程序读到的输入行是 `"[A[H[3~ok"` —— ESC 转义序列残片全部混入输入并发送给模型。

**修复**：`readLine` 新增 `r == 27` 分支，吞掉以 `ESC [` 或 `ESC O` 开头、至字母或 `~` 结束的整个转义序列（10 行，含注释）。孤立按 Esc 会吞掉紧随的一个按键，与 kilo 等极简实现的行为一致。

**修复后实测**：`↑` + `Home` + `Delete` + `ok` → 读到 `"ok"`；纯文本 `"hello"` → `"hello"`；`ab` + 退格 + `c` → `"ac"`（后两项为无回归证明）。

## 改动 2：新增 `-max-tool-rounds`（工具循环轮次上限）

**标准依据**：主流 agent 运行时均内置每轮工具循环上限 —— OpenAI Agents SDK 默认 `max_turns=10`，LangChain 默认 `max_iterations=15`。

**实现**（最小化：1 个变量 + 1 个 flag + 6 行守卫）：
- 默认 `25`，`0` = 不限制（与本文件其他 limit 约定一致）；
- 在每轮工具**执行完毕后**检查，保证 assistant(tool_calls) 与 tool 消息永远成对，历史不会残缺；
- 达到上限时向会话注入 `[Tool round limit reached: N]` 并正常结束本轮（非报错路径）。

## 补丁文件

`agent-fix.patch` 为相对原始文件的 unified diff：**5 个 hunk、全部为新增行（共 20 行）**，无任何删改。
应用方式：`patch -p0 < agent-fix.patch` 或 `git apply agent-fix.patch`。
（该补丁已经过往返校验：原始文件 + 补丁 == 修复版，逐字节一致。）

## 验证矩阵（Go 1.27.1）

- 编译：linux / darwin(amd64, arm64) / windows(amd64, arm64) **全部通过**
- `go vet`：3 个 GOOS 零告警；`gofmt` 干净
- PTY 实测：3/3 通过（`go clean -testcache && go test` 非缓存复跑）

## 约束核查（未改动，原代码已满足）

- 网络请求：仅 `chatStream` 访问模型 API（`apiBase+"/chat/completions"`），无其他网络调用；
- 文件读写：仅工具用 `readMedia`、会话存档 `saveSession`/`loadSession`。

## 文件清单

| 文件 | 说明 |
|---|---|
| agent.go | 修复版（含上述 2 处改动） |
| exec_unix.go | 原样（unix 进程组设置） |
| exec_windows.go | 原样（Windows 空实现） |
| term_darwin.go | 原样（macOS raw mode） |
| term_linux.go | 原样（Linux raw mode） |
| term_windows.go | 原样（Windows console） |
| pty_readline_test.go | 输入回归实测用例（`//go:build linux`，不影响 `go build`，不需要可删除） |
| go.mod | 仅便于就地 `go build` / `go test`，并入您自己的模块时可忽略 |
| agent-fix.patch | 修复补丁 |
| CHANGES.md | 本说明 |

## 快速使用

```bash
# 直接构建
go build -o agent .

# 复跑输入实测（仅 Linux，需要 /dev/ptmx）
go test -v .

# 查看全部改动
less agent-fix.patch
```

---

# v1.1：会话重载环境值刷新 + 全面单元测试

## 会话重载的过期环境值（设计决策：载入即刷新）

- **问题**：`-load` / `-continue` / `/reload` 载入的 system 消息中 ENV 段（Timestamp / PID / Exe / Vars）描述的是旧进程，重载后含义已失效。
- **决策**：既不直接舍弃、也不标注过期 —— `loadSession` 在载入时以当前进程**原位刷新 ENV 段**（单一真相源：system 的环境值永远描述当前进程）；规则段原样保留；无 ENV 锚点的自定义 system 消息一字不动。零数据丢失、零新增噪音。
- **实现**：`buildSystemPrompt` 拆为 `systemRules()` + `systemEnv()`（共用锚点 `systemEnvMarker`）；`loadSession` 尾部 7 行刷新循环，`-load` / `-continue` / `/reload` 三路自动统一。

## 测试套件（32 函数 / 45+ 用例，零测试依赖）

- `agent_test.go`（全平台）：会话存档 round-trip、ENV 刷新（含自定义 system 保留、非字符串 content 跳过）、提示词结构、管道模式输入解析（转义序列 / 控制字符 / CRLF / EOF）、readMedia（类型 / 大小 / 目录 / 缺失）、wrapResult / 参数解析、sanitize、ANSI 剥离。
- `sse_test.go`（全平台）：httptest 假端点 SSE 全链路 —— reasoning/content 分流、tool_calls 乱序分片累积、finish 触发与 EOF 兜底、[DONE] 停读、自定义字段映射、坏行跳过、API 错误、断连、ctx 取消、请求形状（含 `-extra` 合并）、工具轮次护栏端到端（assistant 与 tool 严格成对）。
- `pty_readline_test.go`（linux）：PTY 行编辑回归（保留）。

---

# v1.2：Debian 打包 + `go install` 直装

## go.mod 模块路径规范化

- `module agent` → `module github.com/dksslq/agent-go`：解锁 `go install github.com/dksslq/agent-go@latest` 一行安装，同时是 dh-golang 打包的前提。
- 纯单包、无内部导入，改名零代码影响。

## debian/ 打包（通往官方仓库的完整材料）

- `debian/control|rules|changelog|copyright|docs|source/format`：dh-golang 规范、quilt 格式、DEP-5 版权、`Rules-Requires-Root: no`。
- CI `package` job 持续验证：dpkg-buildpackage 构建 `.deb` + lintian `--fail-on error` + 产物挂 artifact。
- tag `v*` 触发 `release.yml`：6 平台二进制 + amd64 `.deb` 自动挂 GitHub Release —— `apt install ./agent-go_*_amd64.deb` 即装即用。
- 官方 Debian/Ubuntu 仓库需真人维护者走 ITP + 赞助（mentors.debian.net）→ NEW 队列；打包材料已齐备。

---

# v1.3：Debian 包定名 agentlet + 特色描述

## 命名（撞名检索后定案）

- "nano agent" 方向经检索已被同域占用：GitHub `disler/nano-agent`（MCP Server）、PyPI `nano-agent`、HuggingFace NanoAgent-135M —— 对 apt 可见性与 Debian NEW 队列审核均不利。
- 定名 **agentlet**：agent + -let（piglet / droplet 同构），即 "nano 尺度的 agent"；保留 `agent` 关键词，`apt search agent` 可命中；未发现同域撞名。
- "nano" 降级为描述词：synopsis `nano-sized terminal AI agent (streaming, tool-calling, zero deps)`。

## debian/ 同步

- Source/Package: `agentlet`，`Upstream-Name: agent-go`；rules 增加 agent-go → agentlet 二进制改名（dh-golang 以模块基名产出）。
- changelog 重写为 agentlet 0.1.0-1 首包；CI / release 工作流改由 `dpkg-parsechangelog` 派生源包名与版本（后续改名零 CI 改动）。
- deb 内二进制 `/usr/bin/agentlet`；`go install github.com/dksslq/agent-go@latest` 产物仍为 `agent-go`（模块路径 = 仓库名），README 安装表分列注明。
