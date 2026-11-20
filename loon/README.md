# RayNeo Token Sync — iOS Loon 插件（方案 A：被动只读同步）

> **English Summary:** Passive read-only iOS [Loon](https://nsloon.com/) plugin & JavaScriptCore script that mirrors RayNeo App's active `access_token` (and safe client headers) to your own Token Broker (`POST <broker_url>/v1/token`). It **never** reads, stores, logs, or transmits `refreshToken`, so the mobile App remains the sole authority for token refresh and is never kicked out.

---

## 工作原理与安全设计

- **只读镜像，绝不踢号**：
  - 仅在 `http-response` 监听 `/authapi/token/refresh` 与 `/identify/quickLogin`，只读取 `data.token` 与 `data.expiresIn`（以及脱敏账号标识），**严格忽略 `refreshToken`**。
  - 在 `http-request` 被动读取普通接口请求头中的 `xtcl-access-token`，并在 **Loon 正则层 + JS 代码层双重排除** `/authapi/token/refresh`、`/identify/quickLogin`、`/identify/common/sendVerifyCode`（因为 `/authapi/token/refresh` 的 `xtcl-access-token` 请求头里放的是 `refreshToken`）。
  - 请求头仅白名单放行 15 个设备/客户端基础头（与 `internal/har/har.go` 完全一致），绝不上传 `xtcl-*`、`Authorization`、`Cookie`、`Host`、`Accept-Encoding` 等敏感或签名头。
- **不阻塞、不篡改 App 流量**：
  - 所有分支均包裹在 `try/catch` 中并固定调用 `$done({})` 原样放行。
  - 正常 `http-request` 命中 `$persistentStore` 去重快路径时 **零网络请求（<1ms）**；仅在发现新 Token 或需要补全一次客户端头元数据时才向 Broker 发起短超时（4s）POST。
  - 若 Broker 暂时不可达，同一 Token 自动进入 15 秒退避冷却，绝不拖慢 RayNeo App。

---

## 文件清单

| 文件 | 说明 |
| --- | --- |
| `rayneo-token-sync.plugin` | iOS Loon 插件配置（含 `[Argument]`、`[Script]`、`[MITM]`） |
| `rayneo-token-sync.js` | Loon JavaScriptCore 同步脚本（兼容对象 / `k=v&k2=v2` / JSON `$argument` 与 `$persistentStore` 回退配置） |
| `rayneo-token-sync.test.mjs` | Node.js 单元测试套件（`node --test`） |

---

## iOS Loon 安装与配置步骤

### 1. 准备 Token Broker

先部署你的 Token Broker（Cloudflare Worker 或自建 Go Broker，参见仓库 `broker/` 目录），记录好：
- **`broker_url`**：例如 `https://rayneo-broker.example.workers.dev` 或 Tailscale 内网地址 `http://100.x.x.x:8787`（不要拼接 `/v1/token`，脚本会自动处理）
- **`broker_secret`**：与服务端 `BROKER_SECRET` 一致的高强度随机密钥

### 2. 开启 Loon MITM 并信任 CA 证书

1. 打开 iOS **Loon** -> **配置** -> **MITM**。
2. 开启 **启用 MITM**（`Enable MITM`）。
3. 若尚未生成/信任证书：
   - 点击 **证书管理** -> **生成新的 CA 证书** -> **安装证书**（按系统提示在「设置 -> 已下载描述文件」中安装）。
   - 前往 iOS **设置 -> 通用 -> 关于本机 -> 证书信任设置**，打开刚安装的 Loon CA 证书完全信任开关。
4. 确认以下域名位于 MITM 列表中（导入插件后会自动加入）：
   ```text
   xr-api-p.rayneo.cn, xr-api-*.rayneo.cn, xr-api-*.rayneo.com
   ```

### 3. 导入插件并填写参数

1. 在 Loon 中进入 **配置 -> 插件 -> 右上角 `+`**。
2. 粘贴插件 Raw URL（也可 Fork 后改为你自己的托管地址或本地导入）：
   ```text
   https://raw.githubusercontent.com/VanceHud/rayneo-cli/main/loon/rayneo-token-sync.plugin
   ```
   > **提示（自托管脚本）**：插件内 `script-path` 默认指向 `https://raw.githubusercontent.com/VanceHud/rayneo-cli/main/loon/rayneo-token-sync.js`。如果你希望完全脱离 GitHub 访问，可将 `.plugin` 与 `.js` 一并托管到自己的仓库/对象存储/内网静态服务，并把 `.plugin` 中两处 `script-path` 改为你的地址。
3. 在插件详情页的 **参数（Argument）** 中填写：

| 参数名 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `broker_url` | 输入框 | `""` | Broker 基础 URL（例如 `https://rayneo-broker.example.workers.dev`） |
| `broker_secret` | 输入框 | `""` | Broker Bearer 密钥（`Authorization: Bearer <secret>`） |
| `notify` | 开关 | `false` | 同步到新 Token 成功时发送 iOS 通知（仅显示脱敏 `fp` 和 TTL，不含完整 Token） |
| `debug` | 开关 | `false` | 在 Loon 脚本日志打印脱敏诊断日志（不含完整 Token 与 Secret） |

4. 确保插件总开关以及两条脚本均处于开启状态。

### 4. 触发首次同步并验证

1. 临时开启 `notify` 或 `debug`（验证完成后可关闭）。
2. 打开手机上的 **RayNeo App**，下拉刷新一次「全天智记」或首页数据。
3. 正常情况下几秒内即可收到 `RayNeo Token Sync` 通知（或在 Loon -> 工具 -> 脚本日志中看到 `synced loon:header` / `synced loon:refresh`）。
4. 在电脑端执行：
   ```bash
   rayneo sync
   ```
   即可验证 CLI 已拉取到最新 Access Token 且处于 `no_refresh: true` 只读模式。

---

## 高级用法：手动脚本传参 / `$persistentStore` 回退

如果不使用 `[Argument]`，也可以通过以下任一方式配置：

1. **Query String `$argument`**：
   ```text
   broker_url=https://rayneo-broker.example.workers.dev&broker_secret=YOUR_SECRET&notify=false&debug=false
   ```
   （脚本按首个 `=` 切分且不会将 `+` 转为空格，安全兼容 Base64 密钥。）
2. **`$persistentStore` 持久化键**（当 `$argument` 为空时自动读取）：
   - `rayneo_sync_broker_url`
   - `rayneo_sync_broker_secret`
   - `rayneo_sync_notify`
   - `rayneo_sync_debug`

---

## 常见问题排查（Troubleshooting）

| 现象 | 可能原因与排查方式 |
| --- | --- |
| 打开 RayNeo App 后没有任何同步日志 | 1. 检查 Loon **MITM 总开关** 是否开启，以及 iOS「证书信任设置」中是否已完全信任 Loon CA。<br>2. 检查 Loon 是否处于代理/分流运行状态，且 `xr-api-p.rayneo.cn` 未被「跳过代理」列表排除。<br>3. 开启插件的 `debug` 开关后重开 RayNeo App，在 Loon「脚本日志」查看 `[RayNeo Sync]` 输出。 |
| 日志提示 `broker_url or broker_secret not configured` | 插件参数未填写完整，或 URL 缺少 `http://` / `https://` 前缀。 |
| 日志提示 `sync failed (...): HTTP 401` | `broker_secret` 与服务端 `BROKER_SECRET` 不一致（注意前后多余空格）。 |
| 日志提示 `sync failed (...): The request timed out` | 手机当前网络无法连通 `broker_url`：若使用 Tailscale 内网 Broker，请确认 iOS 端 Tailscale 已连接且未被 Loon 代理规则劫持走外网代理（建议给 Broker 域名或 Tailscale 网段 `100.64.0.0/10` 配置 `DIRECT`）。 |
| 为什么明明刷新了界面，却没有再次请求 Broker？ | 这是正常的**去重快路径**：当前 Token 和客户端头均已同步过，脚本会跳过网络请求直接 `$done({})`。当 RayNeo App 自动刷新出新 Token（约每 2 小时）或你修改了 `broker_url` 时会自动再次推送。 |
| 为什么每次 Token 刷新会有两次上报？ | 第 1 次来自 `/authapi/token/refresh` 的 `http-response`（上报精确的 `expires_in: 7200`）；第 2 次来自随后首个业务接口的 `http-request`（补全 15 个客户端头与 `region`/`device_id`/`account_id`，不带 `expires_in` 且不重复弹通知）。之后所有请求均走 `<1ms` 快路径。 |

---

## 本地运行测试

需要 Node.js 18+（无需安装任何 npm 依赖）：

```bash
node --test loon/rayneo-token-sync.test.mjs
```
