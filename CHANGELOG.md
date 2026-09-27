# Changelog

All notable changes to SuperbTmr are documented in this file.

## v1.0.0

首个正式版本。SuperbTmr 是一个跨平台的共享终端工作台：一个实例同时服务人、AI Agent 与脚本，把本地或经 SSH 的真实终端统一到一层可观测、可编程、可接管的会话之上。

### 核心能力

- **一个端口，四类入口**：Web UI（人）、MCP / SKILLS（Agent）、REST + WebSocket（脚本）共用同一个端口，访问同一批真实会话。
- **真实多轮终端**：会话长驻并被复用，Agent 可以像人一样持续驱动 TUI、REPL、GDB、msfconsole、vim 等需要真正终端的程序，而不是只能下发一次性命令。
- **人机接力**：人和 Agent 可在同一终端上交替操作，输入被串行化；`sudo`、密码、MFA 等特权提示可交由人在 Web UI 中输入；`notify_user` 可直接呼叫操作者。
- **多主机 / 多会话并行**：会话仪表盘、标签页与平铺工作区、端口转发一览、文件管理（浏览 / 上传 / 下载 / 重命名 / 建目录）。
- **本地与远程一套流程**：零配置访问本机（`ssh_config="internal"`），或经 SSH 连接 profile 访问任意远端；命令、SFTP 与可续传 HTTP 文件传输、端口转发（`-L` / `-R` / `-D`）都走同一条连接。
- **凭据留在服务端**：经 `ssh_config` 写入的密码、私钥与 passphrase 只存放在主机上，MCP 读接口只返回 profile 名称；SSH 配置写工具默认关闭，需 `--mcp-manage-ssh-configs` 显式开启。
- **失败可恢复**：会话关闭、退出、崩溃或重启后，仍以只读 DEAD 状态留在会话列表中，输出完整可回放、可分页；从同一 `superbtmr://<entry>` 重连即可开新会话继续。
- **主动通知，无需轮询**：`shell_notify` 在进程退出、静默或出现新输出时唤醒 Agent，只发信号不带载荷；`channel="sampling"` 可直接发送 `sampling/createMessage`。
- **多读者不丢输出**：同一会话的多个并行读者各自持有独立游标，互不干扰。

### 工程特性

- **纯 Go，无 CGO**：单个静态二进制，`CGO_ENABLED=0` 构建，跨平台原生交叉编译；Windows 使用 ConPTY，macOS / Linux 使用 POSIX PTY，行为一致。
- **三平台 CI**：ubuntu / macos / windows 全部跑 `go test ./internal/... -count=1 -timeout 120s`。
- **测试与实现同量级**：单元测试覆盖并发、PTY、审批、认证、存储、WebSocket 等关键路径。
- **实例自描述**：每个运行中的实例对外提供自己的 `/api.md` 与 `/skills.md`（免 token），并注册为 MCP resources 与 `learn-api` prompt。
- **多语言 Web UI**：跟随浏览器语言，可在页头切换，切换不重载页面、不重建已开终端。

### 安全模型

- **HTTP 认证**：单一静态 token，`--auth-token`（明文）或 `--auth-hash`（salted SHA-256，服务端不存明文）；非 loopback 绑定必须配置认证，除非显式 `--disable-auth`。
- **审阅模式**：开启后 Agent 的每一次写操作（终端输入、文件传输、端口转发）都需人在 Web UI 中确认或拒绝。
- **已知边界**：审阅模式需要人工确认，但脚本执行、文件上传仍可能因复核疏忽而被放行；浏览器登录使用 HTTP Basic，暴露到本地网络之外时应在前面终止 TLS。
