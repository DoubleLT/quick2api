# quick2api

> **English** | [中文](README.zh-CN.md)

quick2api is an unofficial protocol adapter that exposes the model your
**Amazon Quick** desktop account talks to behind familiar `/v1` APIs —
OpenAI `chat/completions` and Anthropic `messages` — so standard clients
(OpenAI SDKs, Claude Code, any chat client) can call it like a normal
endpoint.

> **Disclaimer**: this project is not affiliated with or endorsed by Amazon.
> It authenticates with your own Quick account credentials (the ones the
> desktop app already stores locally) against an internal WebSocket
> surface. Intended for personal use with your own account; you are
> responsible for complying with Amazon Quick's terms of service.

## What it does

```
your client ── /v1/chat/completions ──▶ quick2api ──AIDP WSS──▶ Amazon Quick cloud agent
            ── /v1/messages ─────────┘        ▲
                                  locally-spawned quickwork-agent outpost
                                  → /auth/sncs-session → id_token
```

- `POST /v1/chat/completions` — OpenAI, streaming (SSE) and non-streaming.
- `POST /v1/messages` — Anthropic Messages API (the surface Claude Code
  and Anthropic SDKs speak); `/v1/messages/count_tokens` is accepted as
  an alias.
- `GET /v1/models` — advertised model names mapped to upstream `agent_mode` values.
- `GET /healthz` — session source status, no secrets.
- **Large prompts go out-of-band**: a `user_message` over ~55 KB is
  uploaded to Quick's attachment service and referenced from a short
  message — one model turn, not N chunk turns. Falls back to chunked
  turns when attachments are unavailable.
- Usage pass-through: `turn_usage` (tokens, cache read/write, `cost_usd`,
  `resolved_model_id`) lands on the OpenAI `usage` object.
- **Runs where your Quick login runs** — Windows today (the outpost reads
  the desktop app's credential store). The HTTP API can be reached from
  anywhere you bind it.
- `-probe` one-shot flag verifies the whole chain before you serve.
- Single static Go binary; only deps are `gorilla/websocket` and
  `gopkg.in/yaml.v3`.

## The upstream chain (why it needs the desktop install)

1. `quick2api` spawns `quickwork-agent --outpost-only --manifest desktop
   --port <p>` with a self-chosen bearer on stdin — the same way the app
   does, on a private port so it never collides with the app's own
   instance.
2. `GET /auth/sncs-session` returns `id_token`, `ws_host`, `provider`
   (`social`), `actor_arn`, `aws_account_id`.
3. Chat runs over
   `wss://<ws_host>/streaming/quickwork-chat-bidirectional-stream` — AWS
   event-stream inbound, `rawInputEvent` JSON outbound; see
   [docs/protocol.md](docs/protocol.md) for the full wire spec we reverse
   engineered.

`agent_mode:"smart"` currently resolves upstream to
`us.anthropic.claude-opus-5-5` (per `turn_usage`), billed against your
Quick subscription quota.

## Quick start

Prerequisites: Amazon Quick desktop installed **and logged in** on this
machine (the account whose model you want to expose), Go 1.22+ (or a
prebuilt binary).

```bash
go build -o quick2api ./cmd/quick2api

# one-shot chain check — spawns the outpost, fetches a session, sends the
# probe text, prints the reply + usage:
./quick2api -probe

# serve:
cp config.example.yaml config.yaml    # optional — defaults are fine locally
./quick2api
```

Then point any OpenAI client at `http://127.0.0.1:8377/v1`:

```bash
curl http://127.0.0.1:8377/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"quick","messages":[{"role":"user","content":"Reply with exactly: PONG"}],"stream":true}'
```

Or point **Claude Code** at it (the Anthropic surface):

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8377
export ANTHROPIC_AUTH_TOKEN=<api.api_key from config.yaml>
claude -p "say PONG"
```

To use it from another machine (e.g. a dev server), keep `server.listen`
on loopback and tunnel:

```bash
ssh -N -R 8377:127.0.0.1:8377 user@dev-host
```

## Configuration

See [config.example.yaml](config.example.yaml) — every key is annotated.
Config search order: `-config` flag → `$QUICK2API_CONFIG` → `./config.yaml`
→ platform user-config dir. State/debug logs live under
`$QUICK2API_STATE_DIR` or the platform state dir.

The knobs you're most likely to touch:

- `api.api_key` — downstream bearer (empty = no auth; fine on loopback only).
- `quick.agent_modes` — client model names → upstream `agent_mode`.
- `quick.outpost_url`/`outpost_token` — attach to an outpost you already run.
- `quick.proxy` — outbound WSS proxy (defaults to system `HTTPS_PROXY`).

## Limitations (v1)

- ~~Text only — image/file parts are rejected with a 400.~~ Fixed in v1.1:
  `image`/`document` blocks ride the upstream attachment channel
  (upload-attachment → dlp-status → deliver → short `user_message` with
  `attachments=[{path,attachmentId}]`), same path the app uses for pasted
  files. `/v1/chat/completions` still rejects client-declared tools.
- ~~Tools are advisory~~ Partial fix in v1.1: enable `tools:` in config
  (`tools.bash.enabled` / `tools.files.enabled`) and `/v1/messages` runs a
  real tool loop — the proxy executes `Bash`/`Read`/`Write`/`Edit`/
  `Glob`/`Grep`/`LS`/`TodoWrite` locally and feeds `tool_result` back
  until the model stops calling tools (`tools.max_tool_rounds` caps it).
  `/v1/chat/completions` still rejects client-declared tools.
- ~~Each request is a fresh upstream session~~ Fixed in v1.1: set
  `X-Conversation-Id` / `X-Session-Id` on the request and the bridge
  pools the upstream conversation by that id — multi-turn becomes a real
  thread instead of a re-flattened prompt.
- One upstream account = one outpost = one lane; `max_concurrency` bounds
  pressure but upstream quota is shared with your app usage. A
  `quick.accounts` pool is planned (devin2api's account-pool pattern).
- If the Quick desktop session itself dies (logged out / token revoked),
  the proxy has no fallback — the upstream answers 401 and turns fail
  until you sign back in to the app. `/healthz` now reports `degraded`
  so an operator can see it.

## Project layout

```
cmd/quick2api/      entrypoint (-config/-state-dir/-version/-probe)
cmd/wsdial/         WSS handshake status probe (diagnostics)
internal/app/       HTTP wiring + auth
internal/api/openai OpenAI adapter (/v1/chat/completions, /v1/models)
internal/api/anthropic Anthropic adapter (/v1/messages)
internal/bridge/    session source → chat client
internal/chat/      AIDP bidirectional-stream client (the core)
internal/eventstream AWS event-stream decoder
internal/session/   session model + outpost lifecycle
internal/config/    YAML config + path resolution
internal/debuglog/  per-request debug directories
docs/protocol.md    the wire facts (worth reading)
```

## License

MIT — see [LICENSE](LICENSE).
