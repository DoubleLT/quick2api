# quick2api

> [English](README.md) | **中文**

quick2api 是一个非官方协议适配器：把你 **Amazon Quick** 桌面端账号背后
实际驱动的模型，以 OpenAI 兼容 `/v1` API 的形式暴露出来——任何标准客户
端（Codex、OpenAI SDK、各种聊天客户端）都能像调 OpenAI 一样调它。

> **免责声明**：本项目与 Amazon 无任何关联、未获其背书。它使用你自己
> Quick 账号的本地登录凭据（桌面应用已经存好的那一份）访问其内部
> WebSocket 接口。仅供个人账号自用；请自行确认符合 Amazon Quick 的
> 服务条款。

## 做什么

```
你的客户端 ──OpenAI /v1/chat/completions──▶ quick2api ──AIDP WSS──▶ Amazon Quick 云端 agent
        （流式或非流式）                          ▲
                                  本地自起的 quickwork-agent outpost
                                  → /auth/sncs-session → id_token
```

- `POST /v1/chat/completions` —— SSE 流式 + 非流式。
- `GET /v1/models` —— 对外的 model 名 → 上游 `agent_mode` 映射。
- `GET /healthz` —— 会话源状态（无密钥）。
- 用量透传：`turn_usage`（token、cache_read/write、`cost_usd`、
  `resolved_model_id`）落在 OpenAI `usage` 字段上。
- **跑在你 Quick 登录的那台机器上**——目前是 Windows（outpost 要读桌面
  应用的凭据库）；HTTP API 监听地址随你绑。
- `-probe` 一次性探针：起服务前先验证整条链路。
- 单个静态 Go 二进制；依赖仅 `gorilla/websocket` 与 `gopkg.in/yaml.v3`。

## 上游链路（为什么离不开桌面端）

1. `quick2api` 自己 spawn
   `quickwork-agent --outpost-only --manifest desktop --port <p>`，stdin
   第一行写自己生成的 bearer——与桌面应用完全同一条路径，但用独立
   端口，绝不碰应用自己的 8780 实例。
2. `GET /auth/sncs-session` 返回 `id_token`、`ws_host`、`provider`
   （`social`）、`actor_arn`、`aws_account_id`。
3. 聊天走
   `wss://<ws_host>/streaming/quickwork-chat-bidirectional-stream`：
   上行 `rawInputEvent` JSON、下行 AWS event-stream；完整协议细节见
   [docs/protocol.md](docs/protocol.md)（逆向实录，值得读）。

`agent_mode:"smart"` 目前在上游解析为 `us.anthropic.claude-opus-5-5`
（据 `turn_usage`），走的是你 Quick 订阅额度。

## 快速开始

前置条件：本机装有 **并登录过** Amazon Quick 桌面版（要反代的就是这个
账号），Go 1.22+（或直接用预编译二进制）。

```bash
go build -o quick2api ./cmd/quick2api

# 一键链路验证——起 outpost、取 session、发探针消息、打印回复与用量：
./quick2api -probe

# 起服务：
cp config.example.yaml config.yaml    # 可选——本机默认就能跑
./quick2api
```

然后把任何 OpenAI 客户端指到 `http://127.0.0.1:8377/v1`：

```bash
curl http://127.0.0.1:8377/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"quick","messages":[{"role":"user","content":"Reply with exactly: PONG"}],"stream":true}'
```

## 配置

见 [config.example.yaml](config.example.yaml)，每个键都有注释。配置查找
顺序：`-config` → `$QUICK2API_CONFIG` → `./config.yaml` → 平台用户配置目
录。状态/调试日志在 `$QUICK2API_STATE_DIR` 或平台 state 目录下。

最常用的几个键：

- `api.api_key` —— 下游 Bearer 校验（空=不校验，仅回环监听时可接受）。
- `quick.agent_modes` —— 客户端 model 名 → 上游 `agent_mode`。
- `quick.outpost_url`/`outpost_token` —— 挂到你已经跑着的 outpost 上。
- `quick.proxy` —— 上行 WSS 代理（默认走系统 `HTTPS_PROXY`）。

## v1 限制

- 仅文本——图片/文件 part 直接 400。
- 客户端 tools/function calling 不做映射；Quick agent 的工具在上游跑，
  我们只透传结果。
- 每个请求都是新的上游会话（新 WebSocket、新 thread）：多轮对话靠把
  历史拼进 prompt；还没有服务端 thread 复用。
- 一个上游账号 = 一个 outpost = 一条 lane；`max_concurrency` 只限本地
  并发，上游配额与桌面应用共享。

## 目录

```
cmd/quick2api/      入口（-config/-state-dir/-version/-probe）
internal/app/       HTTP 路由 + 鉴权
internal/api/openai OpenAI 适配层（/v1/chat/completions、/v1/models）
internal/bridge/    会话源 → 聊天客户端
internal/chat/      AIDP 双向流客户端（核心）
internal/eventstream AWS event-stream 解码器
internal/session/   session 模型 + outpost 生命周期
internal/config/    YAML 配置 + 路径解析
internal/debuglog/  按请求一个目录的调试记录
docs/protocol.md    线上验证过的协议实录（值得一读）
```

## 许可证

MIT —— 见 [LICENSE](LICENSE)。
