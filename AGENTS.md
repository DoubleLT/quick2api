# AGENTS.md — quick2api 项目准则

## 项目本质

quick2api 把 Amazon Quick 桌面端账号背后的模型通过本地 outpost +
AIDP 双向流 WebSocket 反代成 OpenAI `/v1`。协议事实全部记录在
`docs/protocol.md`——改任何上游交互代码前先读它，里面的每一条都是
线上验证过的，不是猜测。

## 硬规则

- **绝不把凭据写进日志/提交**：`id_token`、outpost token、`sess.json`、
  Windows Credential Manager 条目。日志里 token 一律
  `MaskedIDToken()`（前6…后4）。`.gitignore` 已挡住 `sess.json` /
  `config.yaml` / `debug/`，别放开。
- **不碰桌面应用的 outpost**：只连我们自己 spawn 的实例（`outpost_port`
  默认 8781）或用户显式配置的 `outpost_url`。应用的 8780 实例的 token
  拿不到，也不去想办法拿。
- **不改上游协议字段名**：`rawInputEvent` 信封、`{type,payload}` 内层
  帧、`session_connect/create/user_message` 字段名都是服务端契约；
  上游变了就改 `docs/protocol.md` 和 `internal/chat` 一起改。
- **OpenAI 适配层只做映射不做重试**：上游失败→下游同义错误码；
  本地不吞错、不假装成功。

## 结构

- `internal/chat/chat.go` — 协议客户端，唯一碰线上帧的地方。
- `internal/eventstream/` — AWS event-stream 解码，字节级，别"顺手重构"。
- `internal/session/` — outpost 生命周期 + sncs-session 获取。
- `internal/api/openai/` — OpenAI ↔ Quick 映射（SSE/usage/错误契约）。
- `internal/api/anthropic/` — Anthropic `/v1/messages` ↔ 同一条 Quick turn
  （Claude Code 走这里；tools 只做协议面展平/解析，没有运行时）。
- `internal/bridge/` — 会话源策略与刷新。

## 验证

- `go build ./...` 必须干净通过（本仓库在 dev-us 用 go1.23 构建，
  目标 GOOS=windows）。
- 端到端只有一条可靠路径：装有并登录 Amazon Quick 的 Windows 机器上
  `quick2api -probe`。单元测试不要 mock 出"看起来对"的假上游。
