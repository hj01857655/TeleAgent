# Changelog

本项目的所有重要变更都记录在此文件。
格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

每次发版前，先在本文件顶部新增一个 `## <tag>` 段落，
内容会自动作为 GitHub Release 的发布说明（见 .github/workflows/release.yml）。

## v1.1.0 — 2026-10-21

### 新增

- **清洗时报告被丢字段**：`SanitizeRequestReport` 返回被移除的字段名（已排序），`debug`
  级别打出 `request sanitized ... dropped="[metadata top_k thinking]"`。上游的错误只说
  「参数有误」不说是哪个，没有这份名单就没法判断该不该往 `TELEAGENT_EXTRA_FIELDS` 里加。
- **请求白名单清洗**（`internal/adapter`）：转发前把请求体裁剪到上游认识的字段
  （`model` `messages` `stream` `temperature` `top_p` `max_tokens` `tools` `tool_choice`），
  顺序在鉴权与选账号之前。Claude Code 这类客户端携带的 `metadata` / `top_k` / `thinking` /
  `system` 会让上游整包拒绝并回「API 调用参数有误，请检查文档」，此项从源头消掉该报错。
  白名单而非黑名单：未知字段是开放集合，黑名单只能枚举已经踩过的。
- **`max_tokens` 按模型封顶**：`chat-nano` 16384 / `chat-lite` 32768 / `chat-pro` 与
  `chat-flash` 65536，超出截断；未列出的模型不设上限。可用 `TELEAGENT_MODEL_LIMITS` 覆盖。
- **`<think>` 推理块拆分**：`TELEAGENT_REASONING_MODE` 四模式——`content`（默认，原样透传）/
  `reasoning_content`（拆到 `delta.reasoning_content`）/ `visible`（去标签留文字）/ `strip`。
  流式路径按 chunk 处理，标签被切在两个 chunk 之间时先缓冲可疑尾巴，避免把 `<thi` 当正文发出；
  流结束时未闭合的半截标签按正文释放，不静默丢弃。
- **空响应换账号重试**：非流式 200 但无 `choices`、或 message 里既无 `content`/`reasoning_content`
  也无 `tool_calls` 时，视为该次尝试失败并切换账号，而不是把空消息交给客户端。
  带 `tool_calls` 的回复不算空——那是所有函数调用响应的形状。

### 修复

- 非流式分支此前在本项目里不存在缓冲，`resp` 释放后无法再取响应头；现将上游响应头
  与状态码在读取 body 前保存，改写 body 后仍按原状态码回写。
- 流式改写后每条 data 行自带终止空行，原流的空行必须一并吞掉，否则客户端会读到
  多出来的空事件边界。
- `reasoning_content` 的 chunk 不再附带空的 `content` 字段（客户端会渲染成多余空 token）。

### 配置

- 新增 `TELEAGENT_REASONING_MODE`、`TELEAGENT_EXTRA_FIELDS`、`TELEAGENT_MODEL_LIMITS`，
  `config.json` 对应键为 `reasoningMode`、`extraFields`、`modelLimits`。

## v1.0.2 — 2026-10-18

### 修复

- **网页登录续期后账号立即可用**：`/auth/callback` 此前只更新内存凭证与状态文件，
  不写账号池，而账号池仅在进程启动时播种一次。后果是登录成功后控制台仍显示
  「无可用账号」、转发请求返回 503，必须重启容器才恢复。现在登录成功会立即
  写入或更新账号池中的对应账号。
- 账号匹配顺序：先按 `installId`，再按 `deviceId`，否则更新池中第一个账号；
  池为空时新建名为「网页登录」的账号。

### 文档

- README 修正入口位置：网页登录续期在「系统」页（此前误写为「系统」或「账号」页）。

## v1.0.1 — 2026-10-16

### 修复

- **账号池为空时降级启动**：不再在 HTTP 服务监听之前 `os.Exit(1)`。
  此前该检查会使进程在控制台可达之前就退出，配合 `restart: unless-stopped`
  形成无限重启循环，且提示中「在面板添加账号」的引导无法抵达。
  现在控制台保持可用，数据面 `/v1/*` 在无可用账号时返回 503 并说明原因。
- **控制台首次登录**：`ADMIN_PASSWORD` 与 `API_KEY` 均未设置时，
  启动会生成一次性控制台密码并打印到日志（`consolePassword=...`，仅本次运行有效），
  不再出现「面板能打开但无法登录」的死局。
- **账号池持久化**：`docker-compose.yml` 补上 `TELEAGENT_ACCOUNTS_FILE` 默认值，
  使账号池写入数据卷而非容器工作目录；`Dockerfile` 为 `/app/data` 授予
  `appuser` 写权限，避免非 root 启动时状态无法保存、重建容器即丢失。

### 文档

- README 补充降级启动、一次性控制台密码行为，以及 Linux 宿主需
  `chown 10001` 才能让绑定目录可写。

## v1.0.0 — 2026-10-15

### 新增

- 多账号池：轮询调度、优先级、失败冷却、认证失效自动切换。
- 控制台登录密码与客户端 API 密钥分离，支持多密钥、模型白名单、独立限流、用量统计。
- 自动签到（TeleAgent 积分福利社），按账号独立执行、幂等。
- 出口代理：全局或账号级 Resin 代理池（http / socks5，粘性会话）。
- 网页登录续期：浏览器登录一次，token 自动兑换保存。
- 流式 (SSE) 与非流式响应，兼容任何 OpenAI API 客户端。
- 内嵌控制台：概览 / 账号 / 模型 / 密钥 / 签到 / 代理 / 日志 / 系统。
