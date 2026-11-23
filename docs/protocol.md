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
    &outpostId=<registered outpost device id>   # NOT arbitrary — see below
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
`tool_result` / `tool_error` (the agent runs its own tools upstream; use their
boundaries to separate text spans, not as client tool calls), `error` `{code,message}`,
`guardrail_blocked`, `session_end`, `session_title_updated`, various
`*_sync` housekeeping frames.

### Conversation reuse and content-filter errors (2026-11-23)

The upstream thread is stateful. A downstream conversation id is a local
transcript key, not permission to append concurrent prompts to one upstream
thread. Live regression: two overlapping requests with the same client id
asked for an ALPHA sequence and BETA_COMPLETE respectively; both returned the
ALPHA sequence before serialization was added. Serialize lane selection, first
dial and Send per client conversation. Reuse a connection only for an
append-only transcript with an unchanged preamble. Seed divergent histories
and reopened connections into fresh upstream UUIDs; replaying a full transcript
against its old server-side UUID duplicates context.

Content filtering can also arrive as an `error` frame with this observed
payload (a real Desktop fork), rather than `guardrail_blocked`:

```json
{"error":"The model's content filter stopped this reply. Rephrase, or start a new chat.","error_code":"content_filtered","initiator":"user"}
```

Map this to `content_filter` (Anthropic `stop_reason: refusal`), not a generic
unavailable-model error. Do not retry it through the chunk fallback. After an
error, cancellation, partial close, or guardrail termination, close the socket
and discard its pending frames rather than pooling an ambiguous turn boundary.
This preserves upstream checks; it does not remove Quick's agent constraints.

### Native-tool narration before client tool JSON (2026-11-23)

Observed in two actual Desktop requests: three `text_delta` events announce
an attachment read, followed by two native `tool_start` / `tool_result` pairs,
then the final text span contains a complete client `tool_use` JSON object.
Concatenating all text makes the answer look like
`I'll read the attached context first.{"type":"tool_use",...}` and defeats
whole-response JSON parsing. Track the byte offset of the latest native tool
boundary. Only that final text span is eligible for client-tool JSON parsing;
ordinary text responses retain all text. Do not extract arbitrary embedded JSON
from prose, and never convert blocked or incomplete output into a tool call.

### Per-message text limit (~64–80 KB)

A single `user_message.payload.text` over roughly **64–80 KB** makes the
upstream close the turn with an empty `{"type":"error","payload":null}`
frame — no code, no message. Verified 2026-11-22: 64 000 bytes goes
through, 80 000 does not; the exact cutoff sits somewhere in between and
probably depends on framing overhead.

quick2api has two workarounds, tried in this order:

**Primary — out-of-band attachment.** The renderer uploads files through
the tenant's attachment service and references them from a short
`user_message`; the model reads the file server-side in **one** turn.
quick2api mirrors that path:

1. `POST {tenant}/integration/quick-work/upload-attachment?name=…&contentType=text/plain&contentLength=…`
   with the prompt as the octet-stream body → `{attachmentId}`.
2. `GET {tenant}/integration/quick-work/attachment-dlp-status?attachmentId=…`
   polled until `policyAction=ALLOW` (or `dlpJobStatus` is
   SKIPPED/NOT_APPLICABLE/DISABLED).
3. Deliver: `POST {tenant}/integration/quick-work/qbs-event` with
   `invokeAgent{path:"files/upload/<conversationId>",_method:POST,body:{attachmentId}}`
   → `{path}` (nested inside `eventResponse.invokeAgentResponse` →
   `api_response.body`).
4. `user_message` is sent with
   `payload.attachments=[{type:"file",name,size,mimeType,path,attachmentId}]`
   and a short text telling the model to read the file.

Note the classic MTU pattern: the ~64–80 KB limit lives in the frame
codec, not the model — attachments bypass the frame entirely.

**Fallback — sequential chunks.** If the upload or deliver step fails
(e.g. attachments unavailable on a provider/edition, or `path` comes back
empty), the prompt is split into ≤ 55 000-byte rune-boundary chunks sent
as sequential `user_message` turns in the same session. Non-final chunks
are prefixed `[CONTEXT PART i/N — do not answer yet, just store this
context]`; the last gets `[CONTEXT PART N/N — final part. Now answer
based on ALL parts above.]` and its `text_delta` / `turn_usage` /
`message_complete` frames are what the caller sees.

Side effects of the chunk fallback:

- **One API call = N upstream turns.** A 200 KB prompt becomes ~4 model
  turns, so upstream token cost and latency scale with chunk count —
  this is exactly why the attachment path exists.
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
