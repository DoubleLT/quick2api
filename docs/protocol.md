# Quick upstream protocol — what quick2api actually speaks

This document is the load-bearing knowledge. Everything below was recovered
from the Amazon Quick desktop app (1.2.x) main/renderer bundles and verified
against a live account in November 2026.

## 1. Session acquisition (local, HTTP)

The Quick desktop app ships a Python "outpost" agent
(`quickwork-agent`, extracted from `resources/agent.zip` into
`%LOCALAPPDATA%\quickwork\agent\`). The desktop spawns it with:

```
quickwork-agent --outpost-only --manifest desktop --port 8780
```

and hands it a random 64-hex token on **stdin line 1**. The outpost exposes
loopback HTTP; the only endpoint we need:

```
GET  http://127.0.0.1:<port>/health                      → 200 {"status":"ok","service":"outpost"}
GET  http://127.0.0.1:<port>/auth/sncs-session           → 200 session JSON   (Bearer: the stdin token)
```

Environment: `QUICKWORK_BUILD_STAGE=prod` is mandatory — without it the
agent exits with RuntimeError.

Session JSON (key fields):

| field | meaning |
|---|---|
| `id_token` | ~230-char JWT-ish token; goes in the WSS `token=` query param |
| `ws_host` | `qbs-cell<uuid>.dp.appintegrations.<region>.<stage>.plato.ai.aws.dev` |
| `tenant_url` | `https://<same host>` — base for the REST/qbs-event surface |
| `provider` | `social` (consumer Quick) — selects the AIDP transport, NOT the gateway transport |
| `actor_arn` | user arn for `session_connect` |
| `aws_account_id` | the Quick account |
| `usable_until` | unix seconds; the underlying login is ~7 days, refreshed by the desktop app |

Credentials chain: the agent reads the desktop app's login from **Windows
Credential Manager** (`quickwork-social-social-*` entries). A second
outpost spawned by us reads the same credential store — that is why no
extra login is needed.

## 2. Two transports — use AIDP

| provider | transport | chat path |
|---|---|---|
| `gateway`, `social_gateway` | Gateway WSS | CBOR+event-stream, different URL |
| `enterprise`, `social` | AIDP WSS | `/streaming/quickwork-chat-bidirectional-stream`, JSON-in-b64 |

Consumer Quick (what we target) is `social` → AIDP.

There is also a REST surface
`POST {tenant_url}/integration/quick-work/qbs-event` for
`invokeAgent:{path,queryParams:{_method},body}` — it answers `api/*` RPCs
(e.g. `api/chat-agents`, `api/ws-rpc` session management) but **does not
run chat turns**: `user_message` replies `unknown_message_type`. Chat is
WS-only.

## 3. The chat WebSocket

```
wss://<ws_host>/streaming/quickwork-chat-bidirectional-stream
    ?surfaceType=QUICK_WORK_DESKTOP_WINDOWS
    &token=<id_token>            # percent-encoded
    &outpostId=<any string>
    &workingSessionId=<uuid>
    &channel=prod
Header: Origin: https://quick.aws.com     # required — no Origin, no upgrade
```

Outbound frames are the same JSON envelope, sent as a **binary** WS
frame whose payload is UTF-8 text (the renderer passes `Uint8Array` to
`WebSocket.send`). Text frames are rejected with
`{"Message":"Unexpected WebSocket frame received."}` and a close 1000.

```json
{"eventType":"rawInputEvent","payload":{"payload":"<base64(inner JSON)>"}}
```

`inner JSON = {"type": <frame_type>, "payload": {...}}`.

Inbound frames are **binary AWS event-stream** messages (prelude CRC +
headers + payload + trailing CRC — CRCs are advisory, several endpoints
differ). Each message payload is UTF-8 JSON in *either* shape:

```json
{"payload": "<base64(inner JSON)>"}                              // most frames
{"eventType": <outer>, "payload": {"payload": "<base64(inner JSON)>"}}
```

and the inner JSON is the same `{type, payload}` shape.

Two things that bite if skipped:

- `?outpostId=` must be the outpost's **registered** device id, learned
  from `GET /local/list_tools` (`outpost_id`). An arbitrary string gets
  a polite close-1000 right after `session_established`.
- `session_connect` needs the capability flags the renderer sends
  (`acp_first_text`, `user_notification`, `subtask_progress`,
  `external_agent_sign_in`, `recents_promotion`, `appVersion`,
  `clientInstanceId`) — a bare `{actor_id,surfaceType,outpostId}`
  connect gets `{"type":"status","payload":{"status":"unknown_
  message_type","received_type":"session_init"}}` and an early close.

## 4. Turn sequence

```
→ {"type":"session_connect","payload":{"actor_id":<arn>,
     "surfaceType":"QUICK_WORK_DESKTOP_WINDOWS","outpostId":<registered>,
     "appVersion":"1.2.4660","clientInstanceId":<uuid>,
     "acp_first_text":1,"user_notification":1,"subtask_progress":1,
     "external_agent_sign_in":1,"recents_promotion":1}}
→ {"type":"federate_status","payload":{}}          // optional, harmless
← session_established + connected (user info) + federate_auth_status

→ {"type":"session_create","payload":{"session_id":<uuid>,"title":<any>}}
← session_created

→ {"type":"user_message","payload":{
     "thread_id":<same uuid>, "text":<prompt>, "message_id":<uuid>,
     "agent_mode":"smart",                  # optional
     "thinking_effort":"high",              # optional
     "user_timezone":"Asia/Shanghai",
     "user_locale":"zh-CN"}}
← message_start
← text_delta        {"delta": "…"}         (N times — the answer body)
← thinking_delta    {"delta": "…"}         (optional — reasoning, not shown)
← turn_usage        {"input_tokens":…, "output_tokens":…,
                     "cache_read_input_tokens":…, "cache_write_input_tokens":…,
                     "model":"smart", "resolved_model_id":"us.anthropic.…",
                     "cost_usd":…, "context_window":…}
← message_complete
← keepalive                                 (idle heartbeats, ignore)
```

Also possible mid-turn: `tool_start` / `tool_arguments` / `tool_progress` /
`tool_result` / `tool_error` (the agent runs its own tools upstream — we
just pass through and ignore them), `error` `{code,message}`,
`guardrail_blocked`, `session_end`, `session_title_updated`, various
`*_sync` housekeeping frames.

### Per-message text limit (~64–80 KB)

A single `user_message.payload.text` over roughly **64–80 KB** makes the
upstream close the turn with an empty `{"type":"error","payload":null}`
frame — no code, no message. Verified 2026-11-22: 64 000 bytes goes
through, 80 000 does not; the exact cutoff sits somewhere in between and
probably depends on framing overhead.

quick2api works around this by splitting the rendered prompt into
≤ 55 000-byte rune-boundary chunks and sending them as **sequential
`user_message` turns inside the same session**. Non-final chunks are
prefixed with `[CONTEXT PART i/N — do not answer yet, just store this
context]` so the model stores them; the last chunk is prefixed
`[CONTEXT PART N/N — final part. Now answer based on ALL parts above.]`
and its `text_delta` / `turn_usage` / `message_complete` frames are what
the caller sees. Side effects to be aware of:

- **One API call = N upstream turns.** A 200 KB prompt becomes ~4 model
  turns, so upstream token cost and latency scale with chunk count.
- Interim chunks' replies are discarded; only the last turn's reply is
  surfaced.
- Empty `error` frames from the upstream lose their detail — see
  `debug.log_frames` if you need to watch the raw inbound stream.

## 5. What the "model" is

`agent_mode` selects a server-side preset. Observed Nov 2026 on a LITE
account: `agent_mode:"smart"` →
`turn_usage.resolved_model_id = "us.anthropic.claude-opus-5-5"`,
`context_window` 1,000,000. The account is billed `cost_usd` per turn —
**this proxy spends your Quick subscription quota**, not AWS credits.

## 6. Hard constraints

- **The outpost must run on the same Windows machine** as the logged-in
  Quick desktop install (credential store + agent binary live there).
  quick2api therefore runs on Windows; the API can be consumed from
  anywhere that can reach the listen address.
- `id_token` in the WSS query is unavoidable (server design); keep the URL
  out of logs — we never print it.
- No streaming cancellation beyond closing the socket (the protocol has
  `steering_cancelled` for the app's own UI, not wired here).
