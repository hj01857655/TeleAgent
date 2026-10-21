<h1 align="center">🚀 TeleAgent2API</h1>
<h3 align="center">OpenAI-Compatible API Gateway for TeleAgent</h3>

---

## 📖 简介

TeleAgent2API 是一个轻量级的 API 网关，将 TeleAgent 服务转换为标准的 OpenAI Chat Completions 接口。

- ⚡ 纯 Go 实现，Docker 镜像极小，前端内嵌单文件（无构建依赖）
- 🔐 **控制台密码与客户端密钥分离**，多密钥管理（模型白名单 / 独立限流 / 用量统计）
- 👥 **多账号池**：轮询调度、优先级、失败冷却、认证失效自动切换
- 📅 **自动签到**：积分福利社每日签到，按账号独立执行
- 🌐 **出口代理**：全局或账号级 Resin 代理池（http / socks5，粘性会话）
- 🔄 **网页登录续期**：浏览器登录一次，token 自动兑换保存，无需手动提取
- 🌊 支持流式 (SSE) 和非流式响应，兼容任何 OpenAI API 客户端
- 📊 内置控制台：概览 / 账号 / 模型 / 密钥 / 签到 / 代理 / 日志 / 系统

---

## 📌 本仓库说明

本项目 fork 自 [gotsomethings/teleagent2api](https://github.com/gotsomethings/teleagent2api)，
在协议兼容层做了扩展（见下方「协议兼容层」）。上游仓库保留为 `upstream` remote：

```bash
git fetch upstream && git merge upstream/main   # 拉取原作者更新
```

### 相对上游的改动

| 改动 | 解决什么 |
|---|---|
| 请求白名单清洗 | 「API 调用参数有误，请检查文档」——上游拒收 `metadata` / `top_k` / `thinking` / `system` 等字段 |
| `max_tokens` 按模型封顶 | 客户端请求超出模型输出上限时上游直接报错 |
| `<think>` 推理块拆分（四模式） | 思维链以 `<think>` 标签混在正文里，客户端当普通文本渲染 |
| 空响应换账号重试 | 上游偶发 200 但内容为空，客户端拿到空消息无从诊断 |

---

## 🚀 快速开始

### Docker（推荐）

1. 克隆代码或下载：
```bash
git clone https://github.com/hj01857655/TeleAgent.git
cd TeleAgent
```

2. 复制环境变量模板并填入参数：
```bash
cp .env.example .env
# 至少填写 TELEAGENT_TOKEN / TELEAGENT_INSTALL_ID / TELEAGENT_DEVICE_ID
# 并设置独立的 ADMIN_PASSWORD（控制台登录密码）
```

> **无需预先准备 token**：账号池为空时网关会以**降级模式**启动——控制台 `/ui` 照常可用，
> 可在其中添加账号（或走「网页登录续期」）；在出现可用账号之前，数据面 `/v1/*` 返回 503。
> 这样首次部署不会因为缺少凭证而崩溃重启。

3. 启动：
```bash
docker compose up -d --build
```

### 源码编译

```bash
go mod tidy
go build -o teleagent2api .
```

直接运行前，准备 `config.json` 或设置环境变量：
```bash
cp config.example.json config.json
./teleagent2api
```

### 验证

```bash
# 健康检查
curl http://localhost:10000/health

# 模型列表
curl http://localhost:10000/v1/models \
  -H "Authorization: Bearer <你的客户端密钥>"

# 聊天请求
curl -N http://localhost:10000/v1/chat/completions \
  -H "Authorization: Bearer <你的客户端密钥>" \
  -H "Content-Type: application/json" \
  -d '{"model":"chat-flash","messages":[{"role":"user","content":"Hello"}],"stream":true}'
```

---

## 🖥️ 控制台

浏览器打开 `http://<服务器>:10000/ui`（根路径 `/` 自动跳转）。

### 登录凭证与客户端密钥是**分开的两套**

| 平面 | 凭证 | 保护范围 |
|---|---|---|
| 管控面 | `ADMIN_PASSWORD` | 控制台 `/ui` 与全部 `/v1/admin/*` 接口 |
| 数据面 | 客户端 API 密钥（可多个） | `/v1/chat/completions`、`/v1/models` |

> 未设置 `ADMIN_PASSWORD` 时，控制台暂时沿用 `API_KEY` 登录（启动日志会告警），便于平滑升级。
> 若 `ADMIN_PASSWORD` 与 `API_KEY` **都未设置**，启动会生成一次性控制台密码并打印在日志中
> （`consolePassword=...`，仅本次运行有效），不会把管理接口对网络敞开。
> 这样即使某个客户端密钥泄漏，也无法进入后台管理账号、密钥和签到。

### 页面

| 页面 | 内容 |
|---|---|
| **概览** | 账号/健康数/请求数/错误率统计卡、账号状态、最近请求、请求活动图 |
| **账号** | 多账号卡片：状态徽标、有效期进度条、请求/错误计数、签到开关、独立代理；支持增删改、启停、测出口、单独签到 |
| **模型** | 可用模型列表（调用次数、可用密钥数、一键复制模型名）、curl 接入示例 |
| **密钥** | 客户端密钥管理：新建（可限定模型范围与独立限流）、一次性完整展示、启停、删除、用量统计 |
| **签到** | 全局签到设置 + 按账号的签到结果表 + 一键全部签到 |
| **代理** | 全局 Resin 代理配置、出口 IP 测试、各账号出口一览 |
| **日志** | 最近 200 条请求（状态筛选、耗时、来源、模型、账号、密钥） |
| **系统** | 运行信息、账号池/密钥/凭证文件路径、网页登录续期入口 |

> 控制台页面本身不含任何凭证，所有数据经 `/v1/admin/*` 获取，敏感值（token、密钥、代理密码）一律掩码返回。

---

## ⚙️ 配置

支持环境变量和 `config.json` 两种方式。环境变量优先级高于配置文件（12-factor 合规）。默认读取 `config.json`，可通过 `TELEAGENT_CONFIG` 变量指定路径。

### 凭证与鉴权

| 环境变量 / JSON 键 | 默认值 | 说明 |
|---------|--------|------|
| `TELEAGENT_TOKEN` / `token` | 空 | (必填) TeleAgent JWT Token（多账号时作为初始账号播种） |
| `TELEAGENT_INSTALL_ID` / `installId` | 空 | (必填) 安装 ID |
| `TELEAGENT_DEVICE_ID` / `deviceId` | 空 | (必填) 设备 ID |
| `ADMIN_PASSWORD` | 空 | **控制台登录密码**，与客户端密钥分离；未设置时回退 `API_KEY` |
| `API_KEY` / `apiKey` | 空 | 兼容用的单客户端密钥，启动时自动导入密钥库；为空则不鉴权 |
| `RATE_LIMIT_RPM` | `60` | 每密钥每分钟请求数上限（0 = 关闭），保护账号额度；`/v1/admin/*` 不受限 |
| `TELEAGENT_KEYS_FILE` | `teleagent-keys.json` | 客户端密钥库持久化路径（docker 中为 `/app/data/teleagent-keys.json`） |
| `TELEAGENT_ACCOUNTS_FILE` | `teleagent-accounts.json` | 多账号池持久化路径 |
| `TELEAGENT_STATE_FILE` | `teleagent-token-state.json` | 网页登录续期后的凭证保存位置 |
| `TELEAGENT_PUBLIC_URL` | 空 | 网关对外地址（反向代理后填写，用于拼接登录回调 URL） |

### 上游与模型

| 环境变量 / JSON 键 | 默认值 | 说明 |
|---------|--------|------|
| `TELEAGENT2API_LISTEN` / `listen` | `:10000` | 监听地址 |
| `TELEAGENT_BASE_URL` / `baseURL` | `https://agent.teleai.com.cn` | 上游接口地址 |
| `TELEAGENT_UPSTREAM_KEY` / `upstreamApiKey` | (内置) | 上游 API Key，一般无需修改 |
| `TELEAGENT_ROUTE_TARGET` | `ops-gateway` | 上游路由头 `X-Route-Target`（新版必需） |
| `TELEAGENT_APP_VERSION` | `2.7.0` | 客户端版本号 |
| `TELEAGENT_USER_AGENT` / `userAgent` | (内置) | User-Agent |
| `TELEAGENT_MODELS` | `chat-nano,chat-lite,chat-pro,chat-flash` | 模型列表 |
| `TELEAGENT_TIMEOUT` / `timeout` | `120s` | 请求超时 |
| `TELEAGENT_RETRY_COUNT` / `retryCount` | `0` | 上游 5xx 重试次数（多账号时同时是故障切换次数上限） |
| `TELEAGENT_SIGN` | `0` | 旧版 HMAC 签名开关（仅旧服务端需要） |
| `TELEAGENT_LOG_LEVEL` / `logLevel` | `info` | 日志级别 (debug/info/warn/error) |
| `TELEAGENT_LOG_FORMAT` / `logFormat` | `text` | 日志格式 (text/json) |

### 协议兼容层

上游是 TeleAgent 桌面端的自有后端，只认一小撮固定字段。Claude Code 一类客户端会带上
`metadata`、`top_k`、`thinking`、`system` 等字段，上游碰到未知字段就整包拒绝并返回
「API 调用参数有误，请检查文档」。网关默认按**白名单**清洗请求，把这类报错从源头消掉。

| 环境变量 / JSON 键 | 默认值 | 说明 |
|---------|--------|------|
| `TELEAGENT_REASONING_MODE` / `reasoningMode` | `content` | `<think>` 推理块去向，见下表 |
| `TELEAGENT_EXTRA_FIELDS` / `extraFields` | 空 | 白名单外额外放行的字段（逗号分隔 / JSON 数组） |
| `TELEAGENT_MODEL_LIMITS` / `modelLimits` | 见下 | 各模型 `max_tokens` 上限（JSON 对象） |

放行的字段：`model` `messages` `stream` `temperature` `top_p` `max_tokens` `tools` `tool_choice`。

`<think>` 推理块的四种去向（GLM 系上游把思维链放在 `content` 开头）：

| 值 | 行为 |
|---|---|
| `content` | 原样透传，保留标签（默认，唯一不会让回复变差的模式） |
| `reasoning_content` | 拆到 `delta.reasoning_content`，`content` 保持干净（支持推理通道的客户端推荐） |
| `visible` | 去掉标签、保留文字，留在 `content` 里 |
| `strip` | 整个丢弃推理块 |

流式路径会处理标签被切到两个 chunk 里的情况：可能是标签开头的尾巴先缓冲，确认不是标签
再放行，避免把 `<thi` 当正文发出去；流结束时仍未闭合的半截标签按正文放出，不静默丢弃。

`max_tokens` 默认上限：`chat-nano` 16384、`chat-lite` 32768、`chat-pro` 65536、`chat-flash` 65536。
超出会被截到上限，未列出的模型不设上限。覆盖示例：
`TELEAGENT_MODEL_LIMITS='{"chat-pro":32768}'`。

非流式响应若为空（无 `choices`，或 message 里既无 `content`/`reasoning_content` 也无
`tool_calls`），网关会换一个账号重试，而不是把空消息交给客户端。

### 自动签到

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `CHECKIN_ENABLED` | `0` | 自动签到开关（积分福利社每日签到） |
| `CHECKIN_TIME` | `08:00` | 每日签到时间（Asia/Shanghai）；账号可单独设置覆盖 |
| `CHECKIN_TYPE` | `daily_login` | 签到任务类型（daily_login / first_login / monthly_basic） |
| `TELEAGENT_USER_ID` | 空 | 可选，福利社门户的 userId 请求头 |

### 出口代理（Resin 形式）

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `TELEAGENT_PROXY` | 空 | 上游出口代理，支持 http/https/socks5；可写 `http://Default.user_tom:token@127.0.0.1:2260` |
| `TELEAGENT_PROXY_PLATFORM` | 空 | Resin 平台（节点池） |
| `TELEAGENT_PROXY_ACCOUNT` | 空 | 业务账号（同账号粘性同一出口 IP） |
| `TELEAGENT_PROXY_TOKEN` | 空 | `RESIN_PROXY_TOKEN` |

> 账号级代理优先于全局：在控制台「账号」页为单个账号填写代理，即可让不同账号走不同出口。

---

## 👥 多账号

在控制台「账号」页添加，或让网关首次启动时从环境变量播种「默认账号」。

> 首次部署可以先不填 `TELEAGENT_TOKEN`：网关在账号池为空时以降级模式启动，
> 控制台保持可用，添加账号后数据面自动恢复（无需重启）。

**调度规则**：

1. 只选**启用**且未处于冷却/失效状态的账号；
2. 按 `优先级`（大者优先）排序后**轮询**分发；
3. 上游返回认证失败（401 / 登录页）→ 该账号标记**凭证失效并退休**，请求**自动切换到下一个账号**重试；
4. 其他错误（5xx / 网络）→ 该账号进入 **60 秒冷却**，同样触发切换；
5. 全部账号不可用时返回 `503 no available account`。

**每账号可独立配置**：名称、Token、设备信息、启用开关、优先级、独立出口代理、自动签到开关与时间。账号卡片会显示有效期进度条（≤7 天转黄、过期转红）。

---

## 🔑 客户端密钥

在控制台「密钥」页管理：

- 为每个客户端（Claude Code、Cherry Studio 等）创建**独立密钥**，吊销单个不影响其他客户端；
- 可限定**可用模型**（留空 = 全部）——越权调用返回 403 并说明原因；
- 可设置**独立限流**（次/分），未设置则用全局 `RATE_LIMIT_RPM`；
- 完整密钥**仅在创建时显示一次**，之后只保留掩码；
- 记录每把密钥的调用次数、失败次数、最近使用时间。

> 环境变量 `API_KEY` 会自动导入为「默认密钥（来自 .env）」，便于从单密钥模式平滑迁移。

---

## 📅 自动签到

签到接口来自 TeleAgent 官方「积分福利社」（官网宣传：首登赠 3000 · 每月登录再赠 3000 · 每日签到领 100）。

- 定时任务只对开启「自动签到」的账号执行；控制台手动签到会覆盖全部启用账号；
- **幂等**：先查询当日任务状态，已领取则跳过，不会重复领取；
- 每个账号的签到结果（成功/失败/已签到）会记录在账号卡片上。

> 注意：签到接口在 token 失效时返回 **200 + HTML 登录页**而非 401。网关已做 JSON 校验，遇到该情况会明确报错「token 无效或已过期」，不会静默假成功。

---

## ❓ 如何获取令牌信息

有三种方式，**推荐第一种**：

### ① 网页登录（推荐，可自动续期）

启动服务后打开 `http://<服务器>:10000/ui` → 「系统」页 → 「凭证与存储」点击 **🌐 网页登录续期** → 用手机号/微信登录 TeleAgent 账号。
登录成功后新 token 会**立即写入账号池**（无需重启），并持久化到状态文件（`./data/token-state.json`），重启容器依然有效。
此后每月 token 到期时，重复一次网页登录即可，**无需再碰 .env**。

> 网关在反向代理后时，请设置 `TELEAGENT_PUBLIC_URL=https://你的域名`，以便正确拼接登录回调地址。
> 若上游后续收紧回调地址白名单导致网页登录不可用，请改用方式②。

### ② 一键提取脚本（从桌面客户端）

在已登录 TeleAgent 桌面客户端的电脑上运行：

```powershell
powershell -ExecutionPolicy Bypass -File tools/get-teleagent-token.ps1
```

脚本会自动定位凭证目录、解密 token（新版用 Windows DPAPI 加密存储）、解码到期时间，
并直接打印可粘贴到 `.env` 的三行配置。

### ③ 手动查找凭证文件

新版客户端（Windows）数据目录：`C:\Users\<你的用户名>\.local\share\TeleAgent\`

```
.local\share\TeleAgent\
├─ app-auth\
│   └─ device-meta.json              ← deviceId / installId（明文，直接可读）
└─ users\<用户ID>\app-auth\
    └─ token.json                    ← token（DPAPI 加密，需用脚本②解密）
```

> 目录在**首次登录客户端后**才会生成；旧版客户端路径 `%APPDATA%\TeleAgent\app-auth\state.json`（明文）仍兼容读取。
> 注意 `~/.config/TeleAgent/` 是客户端内核数据目录（skills/tools），**不是**凭证目录。
> token 有效期约一个月。

---

## 🐳 部署建议

```bash
# 目录结构
TeleAgent/
├─ .env                 # 凭证与配置（chmod 600）
└─ data/                # 持久化：账号池、密钥库、网页登录状态
```

- **必须设置** `ADMIN_PASSWORD` 与强随机客户端密钥，不要使用默认值；
- `data/` 需对容器内非特权用户（uid 10001）可写：Linux 首次部署执行
  `mkdir -p data && sudo chown 10001:10001 data`（Docker Desktop / Windows 通常无需处理）；
- 不要将 10000 端口直接暴露公网，建议前置 Nginx/Caddy 做 HTTPS 反代；
- `.env`、`data/` 已在 `.gitignore` 中，切勿提交到仓库；
- 单副本部署时限流为进程内存计数；多副本部署时每副本独立计数。

---

## ⚠️ 免责声明

- 本项目仅供学习和研究使用，不得用于任何商业用途或牟利。
- 本项目不提供任何模型服务，仅作为接口转换网关使用。
