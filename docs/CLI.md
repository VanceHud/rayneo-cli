# `rayneo` 完整命令行与协议参考手册

[返回中文主页](../README.md) · [English README](../README_EN.md) · [安全策略](../SECURITY.md)

本文档提供 `rayneo` 全部 **31 个子命令**、全局开关、环境变量、方案 A（被动 Token Broker）自动同步机制、任务增删改查（CRUD）、GPS 轨迹与 Obsidian 每日笔记导出、双存储引擎转录架构、本地磁盘缓存策略以及 `TCL1-HMAC-SHA256` 网关签名协议的完整说明。

---

## 目录

- [快速开始](#快速开始)
- [安装与构建](#安装与构建)
- [命令一览（31 个子命令）](#命令一览31-个子命令)
- [全局开关与环境变量](#全局开关与环境变量)
- [输出与管道处理](#输出与管道处理)
- [批量导出与报告合成](#批量导出与报告合成)
- [逐句转录架构（对话 + 录音）](#逐句转录架构对话--录音)
- [本地磁盘缓存](#本地磁盘缓存)
- [认证与签名协议（`TCL1-HMAC-SHA256`）](#认证与签名协议tcl1-hmac-sha256)
- [AI Agent 集成](#ai-agent-集成)
- [配置文件结构](#配置文件结构)
- [仓库目录结构](#仓库目录结构)
- [故障排查](#故障排查)
- [已知限制](#已知限制)

---

## 快速开始

### 方式一：方案 A · Token Broker + Loon 自动同步（强烈推荐，长期免手工抓包且不顶号）

服务端在短信验证码重登（`/identify/quickLogin`）时会作废其他设备的 `refreshToken`，且调用 `/authapi/token/refresh` 续期会**立即作废**手机端正在使用的旧 `access_token`。方案 A 将手机端 RayNeo App 保留为唯一的续期权威，由 iOS Loon 插件（[`loon/`](../loon/README.md)）将被动观测到的最新 `access_token` 与安全设备头镜像至私有 Token Broker（[`broker/`](../broker/README.md)，支持 Cloudflare Workers 或自托管 Go 服务），全链路**零 `refreshToken` 接触**：

```bash
# 1) 一次性绑定私有 Broker 并立即拉取（凭证写入 0600 权限的 config.json，强制 no_refresh=true 并清空 refresh_token）
rayneo sync -url https://your-broker.workers.dev -secret <your-broker-secret> -verify

# 2) 开始查询（若本地 Token 缺失或已过期，请求前自动静默拉取；若遇 401，自动强制拉取新 Token 并透明重试一次）
rayneo count
rayneo events -date 2026-11-01

# 3) 查看状态 / 手动同步 / 清除 Broker 配置
rayneo sync -status     # 查看 Broker URL、脱敏密钥、Token 指纹、来源与过期状态
rayneo sync             # 携带 If-None-Match ETag 条件同步（未变化返回 304）
rayneo sync -force      # 跳过 ETag 强制全量拉取
rayneo sync -clear      # 清除 config.json 中的 broker_url / broker_secret / broker_etag
```

### 方式二：从 `.har` 抓包提取 Token（零部署推荐，不顶掉手机登录）

无需部署 Broker 时，可直接从抓包工具（Loon / Reqable / Proxyman / Charles 等导出的 `.har`，详见 [抓包指南](../skills/rayneo-cli/references/har-capture-guide.md)）提取当前活跃的 `xtcl-access-token`：

```bash
# 1) 持久导入 HAR 中的 token（自动清空 refreshToken 并开启 no_refresh，绝不触发续期）
rayneo har ./capture.har -verify

# 或者单次临时使用（不改写 config.json，跳过 Broker 自动同步，同样绝不刷新 token）
rayneo -har ./capture.har events -date 2026-11-01

# 2) 开始查询
rayneo count
rayneo events -date 2026-11-01
```

### 方式三：短信验证码独立登录（备选）

```bash
# 1) 发送验证码（area 需带 + 号）
rayneo sendcode -account 13800000000 -area +86

# 2) 使用收到的 6 位验证码登录（凭证保存在 ~/.config/rayneo-cli/config.json，权限 0600）
rayneo login -account 13800000000 -code 123456

# 3) 开始查询
rayneo count
rayneo events -date 2026-11-01
rayneo journal | jq -r '.data[0].context' | head -20
```

> **续期行为差异**：短信登录模式下 `access_token` 有效期约 2 小时，默认过期自动续期（传 `-no-refresh` 可关闭续期）。Broker 模式与 HAR 模式下**永久禁用 `/authapi/token/refresh` 续期**——Broker 模式下会在 Token 过期或 401 时自动从私有 Broker 拉取手机端最新同步的 Token；HAR 模式下待 Token 过期后重新抓包执行 `rayneo har ./capture.har -verify` 即可。

---

## 安装与构建

需要 Go 1.21+（零第三方依赖）。

```bash
# 直接安装最新正式版至 $(go env GOPATH)/bin
go install github.com/VanceHud/rayneo-cli/cmd/rayneo@latest
rayneo -version

# 或在克隆仓库后使用 Makefile 构建与验证
make build          # 产出 ./bin/rayneo（含版本信息注入）
make vet            # 运行 go vet ./...
make test           # 运行 go test -race ./...
make install        # 运行 go install ./cmd/rayneo
```

也可以直接使用 `go build`：

```bash
go build -trimpath -o ./bin/rayneo ./cmd/rayneo
./bin/rayneo -version
```

---

## 命令一览（31 个子命令）

运行 `rayneo help` 查看终端帮助，`rayneo help --json` 输出完整机器可读规格。所有 `-date` / `-from` / `-to` 参数除 `YYYY-MM-DD` 外，也支持 `today`、`yesterday` 与 `-Nd`（如 `-7d`）。

### 1. 认证与配置（8 个）

| 子命令 | 接口 / 动作 | 说明 |
|---|---|---|
| `sync [-url url] [-secret sec] [-force] [-verify] \| -status \| -clear` | `GET <broker>/v1/token`（`-verify` 额外调用 `GET /profileapi/v1/profile/query`） | **【优先推荐·自动同步】** 配置或从方案 A Token Broker 拉取最新 `access_token` 与安全设备头（支持 `If-None-Match` ETag `304`、强制 `no_refresh=true`、清空 `refresh_token`）；`-status` 查看脱敏配置与有效期；`-clear` 清除 Broker 配置 |
| `har <file.har> [-verify] [-dry-run]` | 本地解析（可选 `GET /lifelog/api/v1/count` 验证） | **【优先推荐·零部署】** 从 `.har` 抓包提取最新有效 `xtcl-access-token` 与安全设备头并保存；自动清空 `refreshToken` 且设置 `no_refresh=true`（别名：`import-har`、`from-har`） |
| `sendcode -account <phone> [-area +86]` | `POST /identify/common/sendVerifyCode` | 请求发送短信登录验证码 |
| `login -account <phone> -code <code> [-area +86] [-country cn]` | `POST /identify/quickLogin` | 短信验证码登录并保存会话（也支持 `login -har <file.har>` 免刷新导入） |
| `refresh` | `POST /authapi/token/refresh` | 手动续期 `access_token`（Broker / HAR / `no_refresh` 模式下会被拦截，防止踢掉手机端） |
| `whoami` | 本地只读 | 查看当前网关地址、区域、脱敏账号、Token 掩码、来源、过期时间、Broker 脱敏状态、续期策略与 `deviceId` |
| `logout` | 本地修改 | 清空本地保存的 Token、过期时间、ETag、账号与客户端标识头（不清理磁盘缓存与 Broker 地址） |
| `config [-base url] [-region cn\|us\|sg\|eu] [-token tok] [-har file] [-broker-url url] [-broker-secret sec] [-no-refresh]` | 本地修改 | 更新网关 Base URL、区域、手动写入 Token、Broker 配置（输出自动脱敏密钥）或切换 `no_refresh` 开关 |

### 2. 业务查询（14 个，全部严格只读）

| 子命令 | 接口 | 说明 |
|---|---|---|
| `count` | `GET /lifelog/api/v1/count` | 任务、日程、活动等汇总计数 |
| `calendar [-from YYYY-MM -to YYYY-MM]` | `POST /lifelog/api/v1/calendar` | 指定月份区间内每天是否有记录（`hasLog`）及每日一句话摘要 |
| `events -date YYYY-MM-DD [-tz Asia/Shanghai]` | `GET /lifelog/api/v1/events` | 单日事件列表及原始 GPS 轨迹点 `trailPoints` |
| `event -id <eventId> [-raw]` | `GET /lifelog/api/v1/events/{id}` | 单条日内事件详情：摘要、带时间戳要点（`outline`）、原始子事件与转录来源（默认渲染 Markdown，`-raw` 输出原始 JSON） |
| `journal` | `GET /lifelog/api/v1/journal/latest` | 获取**最新一篇** AI 每日故事（服务端忽略 `-date`，别名：`story`） |
| `jd [-date YYYY-MM-DD]` | `POST /lifelog/api/v1/journal/detail` | 获取**指定日期**的 AI 每日故事详情（默认当天，别名：`journal-detail`） |
| `trail [-date YYYY-MM-DD] [-format stays\|gpx\|geojson\|kml\|json] [-geocode] [-wgs84] [-out file]` | `GET /lifelog/api/v1/events`（`-geocode` 额外调 `GET /profileapi/geo/reverseGeocode`） | 导出每日 GPS 轨迹与停留点（估算里程、关联事件标题、可选逆地理编码省市区、可选 China GCJ-02→WGS-84 纠偏） |
| `digest [-date today\|yesterday\|YYYY-MM-DD] [-with-transcripts] [-geocode] [-wgs84] [-out file\|dir]` | 聚合 `events` + `jd` + `tasks`（+ 可选 `full`） | 生成带 YAML Frontmatter 的 **Obsidian 一站式每日笔记**（含每日故事、事件时间线、停留点、关联待办与可选折叠逐句转录；别名：`today`） |
| `suggests` | `POST /summary-service/suggest/list` | AI 建议列表 |
| `chat [-id <chatId>] [-n 30] [-deadline ms] [-all] [-format json\|md\|table] [-out file]` | `GET /clsapi/aiChat/selectPage` / `GET /clsapi/aiChatMsg/selectPage` | 列出 AI 对话会话（默认 JSON，支持 `-table`/`-md`），或传入 `-id <chatId>` 拉取完整问答消息正文（自动折叠 `<thought>`） |
| `facts [-status 2] [-format json\|md]` | `GET /factsapi/api/v1/facts/list` | AI 记忆事实卡（已确认与待审核列表，支持 `-md`） |
| `devices [-format json\|table\|md]` | `GET /identify/device/list` | 已绑定雷鸟眼镜设备列表（名称、型号、SN、MAC、`glassesDeviceId`） |
| `profile [-onboarding] [-format json\|md]` | `GET /profileapi/v1/profile/query`（`-onboarding` 额外调 `/lifelog/api/v1/settings/onboarding`） | 账号智记设置档案；附加 `-onboarding` 可同时查看自我介绍、关注目标、常用语言与权限状态 |
| `quota [-usages] [-page 1] [-size 20] [-resource-type ...] [-format json\|table\|md]` | `GET /balance/members` / `GET /balance/usages` | 会员等级、有效期、资源包额度（AI 高级模型、专题数、10GB 云空间自动格式化为 MB/GB）与可选消费流水（别名：`balance`） |

### 3. 待办任务管理（1 个，支持完整读/写 CRUD）

| 子命令 | 接口 | 说明 |
|---|---|---|
| `tasks [list] [-status all\|pending\|completed] [-format json\|table\|md] [-source rayneoTasks]` | `GET /action/api/v2/tasks` | 列出待办（默认只读输出 JSON；支持 `-table`、`-md` 与状态过滤；别名：`task`） |
| `tasks add <content> [-desc text] [-priority normal\|high] [-star] [-due YYYY-MM-DD\|today\|tomorrow\|+Nd] [-tz Asia/Shanghai] [-json]` | `POST /action/api/v2/tasks/create` | 新建待办（`priority`: `high=0`, `normal=1`；`-due` 自动转换为服务端毫秒时间戳；成功后自动清理本地待办缓存；别名：`create`、`new`） |
| `tasks done <id...> [-source rayneoTasks] [-list-id id] [-json]` | `POST /action/api/v2/tasks/{id}/complete` | 将一个或多个待办标记为已完成并刷新本地缓存（别名：`complete`） |
| `tasks reopen <id...> [-source rayneoTasks] [-list-id id] [-json]` | `POST /action/api/v2/tasks/{id}/reopen` | 取消完成、重新打开一个或多个待办（别名：`undone`、`uncomplete`） |
| `tasks update <id> [-content ...] [-desc ...] [-priority ...] [-star=true\|false] [-completed=true\|false] [-due ...] [-json]` | `POST /action/api/v2/tasks/{id}` | 更新待办字段（自动先拉取原任务保留未修改字段，避免覆盖丢失；别名：`edit`） |
| `tasks delete <id...> [-source rayneoTasks] [-list-id id] [-json]` | `DELETE /action/api/v2/tasks/{id}?source=...` | 删除一个或多个待办并刷新本地缓存（别名：`rm`、`del`） |

### 4. 逐句转录（3 个，全部严格只读）

| 子命令 | 接口 | 说明 |
|---|---|---|
| `full (-id <eventId> \| -date YYYY-MM-DD) [-format md\|txt\|srt\|json] [-out dir] [-tz Asia/Shanghai]` | `GET /lifelog/api/v1/events/{id}/full`（录音事件自动回退 `getDataAllByTaskId`） | **统一逐句转录入口**：导出按话题分组的对话及独立录音逐句内容、说话人与时间戳（stdout 与 `-out` 均支持录音自动回退） |
| `recordings [-type recording\|cc\|''] [-device RayNeoS3] [-n 50] [-deadline ms] [-all] [-json]` | `POST /clsapi/aiTask/v2/selectPage` | 列出录音（`recording`）或实时字幕（`cc`）任务及其 `taskId`（支持 `-device ''` 查全部机型、`-all` 自动翻页与 `-json`） |
| `transcript (-id <taskId> \| -date YYYY-MM-DD) [-format md\|txt\|srt\|json] [-out dir] [-tz Asia/Shanghai]` | `POST /clsapi/aiTask/getDataAllByTaskId` | 拉取录音任务的完整逐句转录，Markdown 自动渲染**重点打点（marks）、带时间戳提纲（outline）、AI 摘要与 Mermaid 思维导图（mindMap）**（别名：`tr`） |

### 5. 批量归档与缓存（3 个，全部本地/只读）

| 子命令 | 说明 |
|---|---|
| `export -from YYYY-MM-DD -to YYYY-MM-DD [-out ./rayneo-export] [-format all\|json\|csv\|md] [-tz Asia/Shanghai]` | 按日期区间批量拉取每日事件与每日故事，内置 `400ms` 请求间隔 |
| `report [-dir ./rayneo-export] [-out report.md] [-title "..."]` | 将导出目录中的 `journal-*.md` 离线聚合为带每日标题目录锚点的单文件 Markdown 报告（**纯离线 0 网络请求**） |
| `cache [stat\|ls\|prune\|clear]` | 查看缓存统计、最近 20 条记录、`prune` 仅清理已过期缓存或 `clear` 清空全部缓存 |

### 6. 调试与帮助工具（2 个）

| 子命令 | 说明 |
|---|---|
| `raw [-X GET] -p /path [-q k=v] [-d json] [-H 'K: v'] [-f]` | 发送任意 `TCL1-HMAC-SHA256` 签名请求，用于探索或调试接口（`-f` 将 `-q` 键值对编码为 `application/x-www-form-urlencoded` 请求体） |
| `help [--json]` | 打印人类可读帮助，或通过 `--json` 输出完整机器可读命令规格 |

---

## 全局开关与环境变量

### 全局开关

全局开关可放在子命令之前或之后：

```text
-version             打印当前二进制版本号（如 rayneo 0.1.0 或 rayneo dev）并退出
-broker-url <url>    本次运行临时覆盖 Token Broker 地址（优先于 RAYNEO_BROKER_URL 与 config.json）
-broker-secret <sec> 本次运行临时覆盖 Token Broker Bearer 密钥（优先于 RAYNEO_BROKER_SECRET 与 config.json）
-har <file.har>      本次运行直接从 .har 提取 token 使用（不修改 config.json，跳过 Broker 自动同步且绝不刷新 token）
-no-refresh          禁止调用 /authapi/token/refresh 续期（避免旧 access_token 作废影响其他设备）
-debug               在 stderr 打印 canonicalRequest / stringToSign / Authorization，并标注缓存命中
-refresh             忽略本地磁盘缓存，强制走网络拉取最新数据（注意：仅刷新缓存，与 token 续期无关！）
-no-cache            本次运行完全不使用本地磁盘缓存（既不读也不写）
-cache-ttl 72h       自定义本次请求的缓存 TTL（Go duration 格式，如 30m、72h）
-H 'K: v'            追加或覆盖请求头（可重复指定，用于查询命令与 raw）
-q k=v               追加 URL 查询参数（可重复指定，同名键可重复以传递数组参数）
```

> **常见易混点**：全局开关 `-refresh` 仅表示「跳过本地磁盘缓存重新查询服务器」（完全安全）；而子命令 `rayneo refresh` 表示「调用网关刷新登录令牌」（在 Broker / HAR 模式下被禁用）。

### 环境变量

| 环境变量 | 作用 |
|---|---|
| `RAYNEO_BROKER_URL=<url>` | 方案 A Token Broker 地址（如 `https://your-broker.workers.dev`，优先于 `config.json`） |
| `RAYNEO_BROKER_SECRET=<sec>` | 方案 A Token Broker Bearer 鉴权密钥（优先于 `config.json`，不回显明文） |
| `RAYNEO_HAR=<file.har>` | 等同于全局传入 `-har <file.har>`，单次免落盘使用抓包中的 Token |
| `RAYNEO_TOKEN=<token>` | 直接使用指定 `xtcl-access-token`（自动启用 `no_refresh`，不修改 `config.json`） |
| `RAYNEO_NO_REFRESH=1` | 强制关闭自动续期（等同于 `-no-refresh`） |
| `RAYNEO_APP_ID=<id>` | 可选：覆盖内置 `TCL1-HMAC-SHA256` 签名的 `Credential` App ID |
| `RAYNEO_APP_KEY=<key>` | 可选：覆盖内置 `TCL1-HMAC-SHA256` 签名的 HMAC 密钥 |
| `RAYNEO_VERBOSE=1` | 非 `-debug` 模式下命中本地缓存时在 `stderr` 输出 `[cache hit]` |
| `XDG_CONFIG_HOME=<dir>` | 自定义配置与缓存根目录（默认 `~/.config`，可用于多账号隔离） |

---

## 输出与管道处理

- **标准流分离**：结构化业务数据（美化后的 JSON 或 Markdown/SRT）始终输出到 **stdout**；HTTP 状态诊断、签名调试与鉴权告警输出到 **stderr**。
- **退出码约定**：
  - `0`：执行成功
  - `1`：网络请求失败、文件读写错误或 `401` 鉴权失效
  - `2`：命令行参数缺失或格式错误

配合 `jq` 的常用示例：

```bash
# 提取单日事件时间与标题
rayneo events -date 2026-11-01 | jq -r '.data.events[] | "\(.startTimeFormatted) \(.title)"'

# 提取最新每日故事正文
rayneo journal | jq -r '.data[0].context'

# 提取各类型计数
rayneo count | jq -r '.data.countData[] | "\(.type): \(.count)"'
```

使用 `-debug` 对照签名输入（输出至 `stderr`）：

```text
--- canonical
GET
/lifelog/api/v1/events
date=2026-11-01&timezone=Asia%2FShanghai
xtcl-nonce:0123456789abcdef0123456789abcdef;xtcl-timestamp:1762000000;
47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=
--- sts
TCL1-HMAC-SHA256
1762000000
<base64(sha256(canonicalRequest))>
```

---

## 批量导出与报告合成

```bash
# 导出日期区间内的全部事件与每日故事
rayneo export -from 2026-11-01 -to 2026-11-07 -out ./rayneo-export -format all
```

导出目录结构：

```text
rayneo-export/
├── events-2026-11-01.json     # 每日事件与 trailPoints 原始响应
├── events-2026-11-01.csv      # 事件表格（含 UTF-8 BOM，Excel 直接双击打开无乱码）
├── journal-2026-11-01.json    # AI 每日故事原始响应
└── journal-2026-11-01.md      # 结构化 Markdown 每日故事
```

- **CSV 列定义**：`date,start,end,duration,type,flow,title,participants,summary,id`
- **Markdown 每日故事示例**：

```markdown
# 2026-11-01 每日故事

**日期** 2026-11-01 · **模式** panorama · **生成于** 2026-11-02T03:15:00

## 架构方案收敛，性能瓶颈定位清晰
上午的评审把核心链路拆解得非常清楚，下午针对客户端渲染耗时完成了方案对齐……

## 高光时刻
09:30-10:15：完成网关鉴权与缓存策略评审……

## 洞察
……
```

将多篇导出的 `journal-*.md` 离线合成为一份完整汇总文档（不发起任何网络请求）：

```bash
rayneo report -dir ./rayneo-export -out ./rayneo-export/2026-11-summary.md -title "2026 年 11 月全天智记汇总"
```

---

## 逐句转录架构（对话 + 录音）

雷鸟云端将逐句语音转录存放在**两套不同的后端服务**中，`rayneo` 对两套存储均已打通，并通过 `rayneo full` 自动融合：

### 1. 全天智记对话存储（`/lifelog/api/v1/events/{id}/full`）

App 中点开全天智记事件卡片看到的按话题分段的逐句对话，数据结构如下：

```text
GET /lifelog/api/v1/events/{id}/full
  → data.transcriptionContents[]      # 按“话题”分组的多段对话
      ├── title                       # 话题标题（如“项目里程碑与排期对齐”）
      ├── sourceType                  # conversation
      ├── startTime / endTime
      └── clips[]
            ├── speakerName           # 说话人（如“我”或其他声纹参与者）
            ├── startTime / endTime   # 毫秒级 epoch 时间戳字符串
            ├── content               # 原文句子
            └── translateContent / languageCode
```

### 2. 独立录音任务存储（`/clsapi/aiTask/getDataAllByTaskId`）

手动开启的眼镜录音存储在 `aiTask` 服务中，返回 `recording.clips[]`（包含说话人标签、起止相对毫秒、语言代码与译文）。

> **为什么推荐直接用 `rayneo full`？**
> 当对某个事件 ID 或某一天运行 `rayneo full` 时，若遇到录音类事件在 `/events/{id}/full` 返回「事件不存在」，`full` 会**自动回退**调用 `/clsapi/aiTask/getDataAllByTaskId`，因此一条命令即可完整覆盖当天的对话与录音。

```bash
# 查看单个事件转录（默认输出 Markdown 到 stdout）
rayneo full -id 100000000000000001

# 直接生成 SRT 字幕格式
rayneo full -id 100000000000000001 -format srt

# 批量导出某一天全部事件的转录（每个事件自动生成 .md / .txt / .srt / .json 四种文件）
rayneo full -date 2026-11-01 -out ./rayneo-export/transcripts

# 如需单独枚举或拉取录音任务列表：
rayneo recordings -type recording -n 50
rayneo transcript -id a1b2c3d4e5f60718293a4b5c6d7e8f90 -format md
```

Markdown 转录输出示例：

```markdown
# 上午产品架构评审会议

**时间** 2026-11-01 09:30 · **时长** 42:15 · **语言** zh-CN · **片段** 18 段
**说话人** me×8, speaker-1×6, speaker-2×4

## 转录

- `00:00` **me**：我们先对齐一下今天评审的三个核心模块……
- `00:18` **speaker-1**：缓存层的过期策略我建议按历史日期和当天分开处理……
```

SRT 字幕输出示例（`-format srt`）：

```srt
1
00:00:00,000 --> 00:00:17,450
[me] 我们先对齐一下今天评审的三个核心模块……
```

> **服务端限制说明**：对于 `cc`（实时字幕）类型的 `aiTask`，云端仅保留任务元数据，未存储逐句 `clips` 数组；仅全天对话（`conversation`）与手动录音（`recording`）包含完整逐句切片。

---

## 本地磁盘缓存

由于过去日期的全天智记数据归档后基本不再变化，`rayneo` 默认启用本地磁盘缓存，使重复查询与断点续跑 `export` 达到 **0 网络请求**。

| 请求类型 | 默认缓存 TTL |
|---|---|
| 历史日期（早于今天）的 `events` / `journal/detail` / `calendar` 与 `reverseGeocode` | **30 天** |
| 事件详情与逐句转录（`/events/{id}`、`/events/{id}/full`、`getDataAllByTaskId`） | **6 小时** |
| 当天及未来日期的数据 | **5 分钟** |
| 动态列表（`count` / `tasks` / `chat` / `suggests` / `recordings` / `facts` / `quota`） | **10 分钟** |
| 认证接口与所有写操作（`sendcode` / `login` / `refresh` / `tasks add\|done\|reopen\|update\|delete`） | **永不缓存**（待办写操作成功后自动清除本地 `/action/api/v2/tasks` 缓存） |

缓存管理命令：

```bash
rayneo cache stat                                      # 查看缓存目录、文件数、新鲜与过期条目数
rayneo cache ls                                        # 列出最近写入的 20 条缓存记录
rayneo cache prune                                     # 仅清理已过期缓存条目，保留新鲜历史缓存
rayneo cache clear                                     # 清空全部缓存文件
rayneo events -date 2026-11-01 -refresh                # 忽略本地缓存，强制从服务端拉取并更新缓存
rayneo events -date 2026-11-01 -no-cache               # 本次请求完全跳过缓存读写
```

- **缓存路径**：`~/.config/rayneo-cli/cache/<sha256前2位>/<sha256>.json`（文件权限 `0600`，目录权限 `0700`）
- **缓存键计算**：`sha256(method + path + query + body)`
- `rayneo logout` 仅清除登录凭证，不会删除缓存；如需彻底清理缓存数据请执行 `rayneo cache clear`。

---

## 认证与签名协议（`TCL1-HMAC-SHA256`）

雷鸟网关（Kong，`https://xr-api-p.rayneo.cn`）要求每个请求携带 `TCL1-HMAC-SHA256` 签名头。签名计算流程如下：

```text
canonicalHeaders = "xtcl-nonce:" + nonce + ";xtcl-timestamp:" + ts + ";"
canonicalRequest = METHOD + "\n"
                 + PATH + "\n"
                 + QUERY + "\n"
                 + canonicalHeaders + "\n"
                 + base64(sha256(body))

stringToSign     = "TCL1-HMAC-SHA256\n"
                 + ts + "\n"
                 + base64(sha256(canonicalRequest))

signature        = base64(hmac_sha256(appKey, stringToSign))

Authorization: TCL1-HMAC-SHA256 Credential=<appId>,SignedHeaders=xtcl-nonce;xtcl-timestamp;,Signature=<signature>
```

### 协议实现要点

1. **`QUERY` 独立成行**：当 URL 无查询参数时，`QUERY` 所在行必须保留为空行（即 `PATH` 与 `canonicalHeaders` 之间有两个连续换行 `\n\n`）。
2. **摘要编码格式**：请求体 SHA-256 与 `canonicalRequest` 的 SHA-256 均采用标准 **Base64** 编码（而非 Hex 十六进制）；空请求体的摘要固定为 `47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=`。
3. **时间戳精度**：`xtcl-timestamp` 为 **Unix 秒级**十进制字符串。
4. **参与签名的请求头**：固定仅有 `xtcl-nonce` 与 `xtcl-timestamp`（末尾均带分号 `;`）；`xtcl-access-token`、`device-id`、`platform` 等业务头不参与 `canonicalHeaders` 计算，但正常随请求发送。
5. **续期接口特殊约定**：`POST /authapi/token/refresh` 请求体为空，需将 `refreshToken` 放入 `xtcl-access-token` 请求头发送；一旦调用成功，上一枚 `access_token` 立即失效。
6. **自定义密钥覆盖**：默认内置客户端签名常量，同时支持通过环境变量 `RAYNEO_APP_ID` 与 `RAYNEO_APP_KEY` 在运行时动态覆盖（详见 `internal/sign` 单元测试）。

---

## AI Agent 集成

1. **Agent Skill**：仓库内置 [`skills/rayneo-cli/SKILL.md`](../skills/rayneo-cli/SKILL.md)，可通过 `npx skills add VanceHud/rayneo-cli` 或 `./scripts/install-skill.sh` 安装。技能内置了严格的「优先 HAR 免刷新鉴权」与「只读查询工作流」护栏。
2. **机器可读规格（JSON Schema）**：任何 Agent 或自动化脚本均可直接运行：

```bash
rayneo help --json
```

输出包含全部 31 个子命令的名称、功能说明、参数列表与默认值、对应 HTTP 方法与路径、示例、`read_only` 以及 `requires_confirmation` 布尔标记：
- `read_only: false` 且 `requires_confirmation: true`（**非只读，执行前必须获得用户明确许可**）：`sync`（`-status` 除外）、`har`（`-dry-run` 除外）、`sendcode`、`login`、`refresh`、`logout`、`config`、`tasks`（默认 `list` 只读；仅 `add`/`done`/`reopen`/`update`/`delete` 为写操作）、`raw`（写请求）。
- 其余 22 个查询、轨迹、每日笔记、账户信息、转录、导出、报告、缓存与帮助命令均为 `read_only: true`。

---

## 配置文件结构

默认位于 `~/.config/rayneo-cli/config.json`（文件权限强制 `0600`）：

```json
{
  "base_url": "https://xr-api-p.rayneo.cn",
  "access_token": "<redacted-jwt>",
  "no_refresh": true,
  "token_source": "broker:loon:refresh",
  "token_captured_at": "2026-11-20T11:30:00Z",
  "token_expires_at": "2026-11-20T13:30:00Z",
  "broker_url": "https://your-broker.workers.dev",
  "broker_secret": "<redacted-broker-secret>",
  "broker_etag": "\"rev-1-0123456789ab\"",
  "account": "13800000000",
  "account_id": 100000000000001,
  "region": "cn",
  "device_id": "00000000-0000-4000-8000-000000000000",
  "client_headers": {
    "platform": "ios",
    "phone-model": "iPhone16,2"
  }
}
```

多账号隔离可通过指定 `XDG_CONFIG_HOME` 实现：

```bash
XDG_CONFIG_HOME="$HOME/.config/rayneo-work" rayneo sync -url https://work-broker.workers.dev -secret <secret> -verify
XDG_CONFIG_HOME="$HOME/.config/rayneo-work" rayneo whoami
```

---

## 仓库目录结构

```text
rayneo-cli/
├── cmd/rayneo/                # CLI 入口包（go install github.com/VanceHud/rayneo-cli/cmd/rayneo@latest）
│   ├── main.go                # 命令路由、Broker/HAR 免刷新守卫、自动同步与 401 重试、查询与导出
│   ├── main_test.go           # sync 生命周期、Broker 401 自动恢复与 HAR 免刷新回归测试
│   ├── full.go                # /events/{id}/full 对话转录解析与多格式渲染
│   ├── transcript.go          # aiTask 录音转录解析与多格式渲染
│   └── help.go                # help --json 规格、CSV / Markdown / 汇总报告生成
├── internal/
│   ├── api/client.go          # 签名 HTTP 客户端、设备头镜像、缓存读写与自动解压
│   ├── broker/                # 方案 A Token Broker HTTP 客户端、ETag 处理、Header 白名单与 Config 合并
│   ├── cache/cache.go         # 本地磁盘缓存与分级 TTL 策略
│   ├── config/config.go       # 0600 配置文件持久化、Broker 环境变量解析与 XDG 路径
│   ├── har/                   # 从 .har 抓包中筛选最新有效 Token 与安全客户端头
│   └── sign/                  # TCL1-HMAC-SHA256 签名实现与环境变量覆盖支持
├── broker/                    # 方案 A Token Broker 服务端（Cloudflare Workers + DO 与自托管 Go 服务）
├── loon/                      # 方案 A iOS Loon 被动同步插件与测试套件
├── skills/rayneo-cli/         # 可移植 AI Agent Skill（SKILL.md + HAR 抓包参考指南）
├── scripts/install-skill.sh   # 本地 Skill 安装脚本（支持 copy / symlink）
├── docs/CLI.md                # 本参考手册
├── Makefile                   # 构建、测试、静态检查目标
└── .goreleaser.yaml           # 跨平台发行构建配置
```

---

## 故障排查

| 现象 / 报错 | 原因分析与处理办法 |
|---|---|
| `{"code":401,"msg":"Missing token"}` | 本地尚未配置凭证 → **优先**运行 `rayneo sync -url <url> -secret <secret> -verify` 或 `rayneo har ./capture.har -verify` |
| `{"code":401,"msg":"Authentication failed"}` | `access_token` 已满约 2 小时过期，或手机 App 自身刷新导致旧 Token 轮换 → Broker 模式下 CLI 会自动拉取并重试；若 Broker 仍为旧 Token，请打开/唤醒手机端雷鸟 App（保持 Loon 运行）后重试；HAR 模式下请重新抓包执行 `rayneo har ./capture.har -verify` |
| `broker has no token synced yet`（Broker 返回 `404`） | Broker 刚部署完成或已被清空，尚未收到手机端 Loon 推送 → 保持 Loon 插件开启并在手机上打开雷鸟 App 刷新一下页面，再执行 `rayneo sync -verify` |
| `broker authentication failed (401 Unauthorized)` | `broker_secret` 不匹配 → 检查 `rayneo sync -url ... -secret ...`、环境变量 `RAYNEO_BROKER_SECRET` 与 Broker 服务端 `BROKER_SECRET` 是否一致 |
| `token refresh is disabled (no-refresh / broker / HAR mode)` | 免刷新保护机制生效，阻止调用 `/authapi/token/refresh` 踢掉手机端登录；请使用 `rayneo sync` 或重新导入 `.har` |
| Kong 返回 HTML `401 Authorization Required` | 请求签名未通过网关校验（常见原因是手动传入 `-H` 意外覆盖了 `xtcl-nonce` 或 `xtcl-timestamp`，或客户端签名密钥变更） |
| 短信登录模式下 `rayneo refresh` 也报 `401` | 手机端重新登录使旧 `refreshToken` 失效 → 建议改用 `rayneo har ./capture.har -verify` |
| `{"code":404,"msg":"No static resource …"}` | 使用 `raw -p` 时误传了客户端页面路由而非后端 REST 路径 |
| 眼镜刚同步的数据查不到 | 命中了当天数据的 5 分钟本地磁盘缓存 → 在命令后追加 `-refresh` 强制走网络拉取 |
| Excel 打开 CSV 中文乱码 | 导出的 CSV 默认已写入 UTF-8 BOM；若旧版表格软件仍乱码，可用数据导入向导显式选择 `UTF-8` |

排查签名细节时可运行：

```bash
rayneo raw -X GET -p /lifelog/api/v1/count -debug
```

---

## 已知限制

- **写操作范围说明**：所有全天智记事件、每日故事、转录与账户信息接口均为严格只读；仅待办管理子命令 `rayneo tasks add|done|reopen|update|delete` 支持写操作。
- **`journal` 与 `jd` 的区别**：`GET /lifelog/api/v1/journal/latest`（`rayneo journal`）服务端忽略日期参数，永远只返回最新一篇；按日期拉取每日故事必须使用 `rayneo jd -date YYYY-MM-DD`。
- **实时字幕（`cc`）无逐句切片**：`aiTask` 服务仅对 `recording` 类型存储逐句 `clips`，`cc` 类型仅返回任务元数据。
- **GPS 轨迹体积**：单日户外活动较多时 `events` 返回的 `trailPoints` 经纬度数组体积较大，处理 JSON 时建议按需用 `jq` 过滤字段。
- **请求频率**：批量拉取多天数据时请使用内置 `400ms` 限速的 `rayneo export`，避免高频并发调用。
