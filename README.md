# rayneo-cli

**中文** | [English](./README_EN.md) · [完整 CLI 与协议手册](./docs/CLI.md) · [安全策略](./SECURITY.md) · [贡献指南](./CONTRIBUTING.md)

[![Go Version](https://img.shields.io/badge/Go-1.21%2B-00ADD8?logo=go)](./go.mod)
[![CI](https://github.com/VanceHud/rayneo-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/VanceHud/rayneo-cli/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/VanceHud/rayneo-cli?include_prereleases)](https://github.com/VanceHud/rayneo-cli/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)
[![Agent Skill](https://img.shields.io/badge/Agent_Skill-rayneo--cli-6E56CF)](./skills/rayneo-cli/SKILL.md)

`rayneo-cli` 是一个零外部依赖的纯 Go 命令行工具与 AI Agent Skill，用于查询、检索与离线归档**雷鸟 AI 眼镜** 在「**全天智记**」（Lifelog）中积累的个人数据：日历概览、每日事件、GPS 轨迹点（`trailPoints`）、AI 每日故事（Daily Story）、区分说话人的逐句转录（对话与录音）、待办任务、AI 建议及 AI 对话历史。

```text
$ rayneo events -date 2026-11-01 | jq -r '.data.events[] | "\(.startTimeFormatted)  \(.audioLengthFormatted)  \(.eventType)  \(.title)"'
09:30   42:15  audio  上午产品架构评审会议
12:15   28:40  audio  午间团队交流与排期对齐
15:00   55:10  audio  客户端性能优化方案讨论
```

---

## 核心特性

- **纯 Go 标准库单二进制**：零第三方依赖，编译产物仅单个 `rayneo` 可执行文件，原生支持 macOS、Linux、Windows（`amd64` / `arm64`）。
- **免刷新不顶号双鉴权（Broker 自动同步 + HAR 抓包）**：
  - **方案 A · 被动 Token Broker 自动同步（推荐）**：搭配 iOS Loon 插件与个人 Token Broker（支持 Cloudflare Workers 或自托管 Go 服务），手机端 RayNeo App 始终是唯一续期方，Loon 仅被动镜像活跃 `access_token`（绝不触碰 `refreshToken`）；CLI 通过 `rayneo sync` 自动拉取并在过期/401 时无感重试，**永不把手机端 App 顶下线**。
  - **`.har` 抓包导入（零部署备选）**：直接从抓包文件提取当前活跃 `xtcl-access-token` 与安全设备头，自动清空 `refreshToken` 并开启 `no_refresh` 保护。
- **双存储引擎转录自动合并**：一条 `rayneo full` 命令自动打通全天智记对话存储（`/lifelog/api/v1/events/{id}/full`）与独立录音任务存储（`/clsapi/aiTask/getDataAllByTaskId`），支持输出带说话人标签、毫秒级时间戳与译文的 `md` / `txt` / `srt` / `json` 四种格式。
- **智能本地磁盘缓存**：历史日期响应默认落盘缓存 30 天（`0600` 权限），重复查询与重跑批量导出实现 **0 网络请求**；支持 `-refresh` 随时强制拉取最新同步数据。
- **温和限速批量导出**：`rayneo export` 内置 `400ms` 请求间隔，按日期区间一键导出原始 JSON、带 UTF-8 BOM 的 CSV（Excel 直接打开不乱码）及结构化 Markdown，并可通过 `rayneo report` 纯离线合并为月度/阶段汇总报告。
- **AI Agent 原生友好**：仓库内置 [`skills/rayneo-cli/SKILL.md`](./skills/rayneo-cli/SKILL.md) 技能包与 `rayneo help --json` 机器可读规格，可一键安装至 Claude Code、Codex、Antigravity、Cursor 等智能体。

---

## 安装

### 1. 使用 `go install`（需要 Go 1.21+）

```bash
go install github.com/VanceHud/rayneo-cli/cmd/rayneo@latest
rayneo -version
```

### 2. 下载预编译二进制

前往 [GitHub Releases](https://github.com/VanceHud/rayneo-cli/releases) 下载对应操作系统（`darwin` / `linux` / `windows`）与架构（`amd64` / `arm64`）的压缩包，解压后将 `rayneo` 放入 `$PATH` 即可。

### 3. 从源码构建

```bash
git clone https://github.com/VanceHud/rayneo-cli.git
cd rayneo-cli
make build          # 编译至 ./bin/rayneo
./bin/rayneo -version

# 可选：安装到 $(go env GOPATH)/bin
make install
```

### 4. 安装 AI Agent Skill（供智能体自动调用）

```bash
# 方式 A：通过 skills CLI 一键安装（推荐）
npx skills add VanceHud/rayneo-cli

# 方式 B：使用仓库自带安装脚本（支持 --target agents|claude|gemini|all）
./scripts/install-skill.sh --target all
```

---

## 认证指南（重要：为什么推荐 Broker 自动同步 / `.har` 模式）

雷鸟云端网关存在严格的单会话令牌轮换机制：

1. 调用短信验证码登录（`POST /identify/quickLogin`）会使手机端 App 的 `refreshToken` 失效；
2. 调用令牌续期（`POST /authapi/token/refresh`）会**立即作废**上一枚 `access_token`，导致手机端 App 瞬间掉线重登。

因此，日常与手机 App 并行使用时，**强烈推荐采用「方案 A：被动 Token Broker 自动同步」或「`.har` 抓包导入」**，两者均强制设置 `no_refresh=true` 且清空 `refresh_token`，彻底杜绝误触续期导致手机掉线：

### 方式一：方案 A · Token Broker + Loon 自动同步（最省心，手机端不掉线）

手机端 RayNeo App 照常负责自动续期，iOS Loon 插件（[`loon/`](./loon/README.md)）在后台被动镜像最新 `access_token` 与安全客户端头至你的私有 Token Broker（[`broker/`](./broker/README.md)，支持免费版 Cloudflare Workers + SQLite Durable Object 或单二进制 Go 服务），全程**绝不采集、存储或传输 `refreshToken`**：

```bash
# 1) 一次性绑定私有 Broker 并拉取最新 Token（保存至 0600 权限的 config.json，支持 -verify 实时校验）
rayneo sync -url https://your-broker.workers.dev -secret <your-broker-secret> -verify

# 2) 后续所有查询命令完全无感：
#    - 若本地 Token 缺失或已过 token_expires_at，请求前自动向 Broker 拉取；
#    - 若接口返回 401，CLI 自动向 Broker 强制拉取新 Token，如有更新则透明重试一次！
rayneo count
rayneo events -date 2026-11-01

# 3) 常用 Broker 管理命令
rayneo sync             # 按 ETag 条件拉取（未变化返回 304 Not Modified）
rayneo sync -force      # 跳过 ETag 强制全量拉取
rayneo sync -status     # 查看 Broker 配置（密钥脱敏）、Token 指纹与剩余有效期
rayneo sync -clear      # 清除本地保存的 Broker 配置
```

> **提示**：当手机 App 长时间未唤醒且 Token 已过期时，若 Broker 仍为旧 Token，CLI 会在 `stderr` 提示打开/唤醒一次手机上的雷鸟 App（保持 Loon 开启），即可自动同步新 Token。也支持通过环境变量 `RAYNEO_BROKER_URL` / `RAYNEO_BROKER_SECRET` 或全局开关 `-broker-url` / `-broker-secret` 临时覆盖。

### 方式二：从 `.har` 抓包导入（零部署推荐，手机端不掉线）

无需部署任何服务端，在手机上使用 Loon / Reqable / Proxyman / Charles 打开雷鸟 App 刷新一次全天智记页面，导出包含 `xr-api-p.rayneo.cn` 请求的 `capture.har`（详细步骤见 [`skills/rayneo-cli/references/har-capture-guide.md`](./skills/rayneo-cli/references/har-capture-guide.md)）：

```bash
# 1) 持久导入（自动清空 refreshToken 并设置 no_refresh=true，保存至 0600 权限的 config.json）
rayneo har ./capture.har -verify

# 或：单次临时运行（完全不写入 config.json，跳过 Broker 自动同步，绝不触发续期）
rayneo -har ./capture.har events -date 2026-11-01

# 2) 查看当前会话与保护状态
rayneo whoami
```

> **提示**：`access_token` 有效期通常约 2 小时（或随手机 App 自身续期而轮换）。当收到 `401 Authentication failed` 时，重新抓包执行 `rayneo har ./capture.har -verify` 即可；导入成功后建议及时删除本地 `.har` 文件。

### 方式三：短信验证码独立登录（备选）

仅在不介意手机端会话失效时使用：

```bash
rayneo sendcode -account 13800000000 -area +86
rayneo login    -account 13800000000 -code 123456
```

---

## 常用工作流与命令速查

### 常用工作流

```bash
# 1. 查看哪些日期有全天智记记录及每日一句话摘要
rayneo calendar -from 2026-10 -to 2026-11
rayneo count

# 2. 查询指定日期的事件列表、GPS 轨迹点与单事件提纲
rayneo events -date 2026-11-01
rayneo event  -id 100000000000000001

# 3. 获取指定日期的 AI「每日故事」（注意：按日期查询请用 jd，journal 仅返回最新一篇）
rayneo jd -date 2026-11-01

# 4. 导出区分说话人的完整逐句转录（自动覆盖全天对话 + 录音任务，支持 md|txt|srt|json）
rayneo full -date 2026-11-01 -format md
rayneo full -id 100000000000000001 -format srt
rayneo full -date 2026-11-01 -out ./rayneo-export/transcripts

# 5. 查询助手待办任务、AI 建议与 AI 对话记录
rayneo tasks
rayneo suggests
rayneo chat

# 6. 按日期区间限速批量归档，并离线合并为汇总 Markdown 报告
rayneo export -from 2026-11-01 -to 2026-11-07 -out ./rayneo-export -format all
rayneo report -dir ./rayneo-export -out ./rayneo-export/weekly-report.md -title "11 月第一周全天智记"

# 7. 当天眼镜刚同步新数据、需跳过 5 分钟本地缓存强制拉取最新结果
rayneo events -date 2026-11-01 -refresh
```

### 命令速查表（共 25 个子命令）

完整参数、返回结构、缓存策略与排障指南请查阅 **[`docs/CLI.md`](./docs/CLI.md)**（或运行 `rayneo help --json`）。

| 分类 | 子命令 | 说明 |
|---|---|---|
| **认证与配置** | `sync` | **【推荐·自动同步】** 绑定或从私有 Token Broker 拉取最新 `access_token`（方案 A，支持 `-verify` / `-force` / `-status` / `-clear`，强制 `no_refresh`） |
| | `har` | **【推荐·零部署】** 从 `.har` 提取 Token 与设备头（自动开启 `no_refresh`） |
| | `whoami` / `config` / `logout` | 查看当前会话与 Broker 状态 / 修改网关、区域与 Broker 配置 / 清除本地凭证 |
| | `sendcode` / `login` / `refresh` | 备选短信验证码登录与手动续期（注意会影响其他在线设备） |
| **只读查询** | `count` / `calendar` | 全局统计计数 / 月度日历打卡与每日一句话摘要 |
| | `events` / `event` | 单日事件列表（含 GPS `trailPoints`） / 单事件摘要、提纲与子事件 |
| | `jd` / `journal` | **指定日期**的 AI 每日故事详情（`jd -date`） / 最新一篇每日故事 |
| | `tasks` / `suggests` / `chat` | AI 助手提取的待办事项 / AI 建议列表 / AI 对话历史分页 |
| **逐句转录** | `full` | **全量对话+录音逐句转录**（自动回退双存储，支持 `md`/`txt`/`srt`/`json`） |
| | `recordings` / `transcript` | 列出录音与实时字幕任务（`taskId`） / 单独拉取 `aiTask` 录音转录 |
| **归档与工具** | `export` / `report` / `cache` | 区间限速批量导出 / 离线合并日记报告 / 查看与清理本地磁盘缓存 |
| | `raw` / `help` | 发送任意已签名的网关请求（调试用） / 查看帮助与 `--json` 机器规格 |

---

## 协议与签名概览（`TCL1-HMAC-SHA256`）

网关（`https://xr-api-p.rayneo.cn`）对所有 HTTP 请求执行 `TCL1-HMAC-SHA256` 签名校验：

```text
canonicalHeaders = "xtcl-nonce:" + nonce + ";xtcl-timestamp:" + ts + ";"
canonicalRequest = METHOD + "\n" + PATH + "\n" + QUERY + "\n" + canonicalHeaders + "\n" + base64(sha256(body))
stringToSign     = "TCL1-HMAC-SHA256\n" + ts + "\n" + base64(sha256(canonicalRequest))
signature        = base64(hmac_sha256(appKey, stringToSign))

Authorization: TCL1-HMAC-SHA256 Credential=<appId>,SignedHeaders=xtcl-nonce;xtcl-timestamp;,Signature=<signature>
```

- 运行任意命令时附加 `-debug` 可在 `stderr` 查看完整的 `canonicalRequest`、`stringToSign` 与 `Authorization` 头。
- 如后续客户端版本轮换签名常量，无需重新编译即可通过环境变量 `RAYNEO_APP_ID` 与 `RAYNEO_APP_KEY` 覆盖默认值。完整协议细节详见 [`docs/CLI.md`](./docs/CLI.md#认证与签名协议tcl1-hmac-sha256)。

---

## 免责声明

- **用途限定**：本项目仅用于**用户本人读取、备份自己账号下的个人数据**以及客户端协议互操作性研究。
- **严格只读**：所有业务查询与导出命令均为只读（Read-Only）操作，未实现且不会提供任何修改、上传或删除云端数据的写接口。
- **请求频率**：请自觉控制请求频率，避免对服务端造成压力；批量拉取请使用内置 `400ms` 间隔与本地缓存的 `rayneo export`。
- **隐私安全**：`.har` 抓包文件与 `~/.config/rayneo-cli/config.json` 含有个人登录凭证与设备信息，切勿上传至公开仓库或粘贴在 Issue 中（详见 [`SECURITY.md`](./SECURITY.md)）。
- **非官方声明**：本项目为独立社区开源项目，与雷鸟创新（RayNeo）及 TCL 无任何隶属、合作、授权或背书关系。官方接口变更可能导致工具失效。

---

## 开源协议

基于 [MIT License](./LICENSE) 开源 · Copyright (c) 2026 VanceHud
