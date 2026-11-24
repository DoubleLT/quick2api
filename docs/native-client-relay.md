# Experimental native client-tool relay

Status on 2026-11-24: opt-in/default-off; tested through an isolated candidate
service. The original service on port 18377 remains unchanged.
User-approved MCP registration is visible on a proxy-owned outpost when it uses
the selected Quick profile. A real Quick → isolated API client → Quick test has
performed the approved fixture Read, and exposed duplicate delivery over direct
MCP and the WebSocket courier. A subsequent live test verified that invocation
receipts join both deliveries: two callbacks, one actual Read, one upstream job,
and the correct final first line. A real installed Claude Code client also
completed one Read, returned its actual tool result, and received the expected
final line with result subtype `success` and process exit 0. A fresh Claude
Desktop session through CC Switch also passed: its transcript contains exactly
one Read of the approved fixture, one matching successful tool result, and the
expected final line. Affected old-session acceptance remains pending. This is
not a claim that historical refusals are fixed.

## Why a separate transport

The existing compatibility path describes client tools in a user prompt and
recognizes strict tool JSON in the assistant's reply. Quick still decides whether
to accept that text contract. Correct schemas, full history delivery and correct
SSE framing cannot force it to do so.

The new path uses one formally registered MCP tool, `quick2api_client_call`,
whose inputs are `route_id`, the external tool `name`, and its `arguments`.
Tool descriptions and schemas remain catalog data in the conversation context.
The relay itself is a native registered callable; final assistant prose is never
parsed into an executable tool in this mode.

The invocation `route_id` is `<turn-token>:<action-id>`. The turn token is random
and bound to one upstream job; the model chooses a new action suffix for each
intended execution. Duplicate direct-MCP/courier delivery with the same suffix,
tool name and canonical input waits for or returns the same real result. A
changed tool/input with that suffix is rejected. Each job retains at most 128
invocation receipts within a 4 MiB input/result budget until it ends. This
preserves the original three-field MCP schema, including when Quick still has
that schema cached.

1. A fresh Quick job owns one upstream socket and account lane.
2. Quick's actual native `tool_call` is delivered to that job's proxy-owned
   outpost, and only to the exact configured relay tool name.
3. The stdio MCP process makes an authenticated loopback callback to the proxy.
   The proxy returns an Anthropic `tool_use` to the external client.
4. Desktop executes under its own permissions. Its next Messages request carries
   the real `tool_result`; the proxy validates the original session, system,
   model, tool catalog, call arguments and append-only history before consuming it.
5. For ordinary tools, the callback returns that result, the same native tool call completes, and
   the same Quick job continues. No new upstream prompt or inference is started
   for this continuation.

`AskUserQuestion` is an explicit human boundary. After successfully delivering
the question, the proxy stops the upstream job/socket and retains the exact
question and transcript contract for 30 minutes from delivery. It does not
fabricate a tool answer. The real client answer must pass the same session,
catalog and append-only history validation; it consumes the pending handle
atomically and starts one new inference with the complete actual transcript.
Earlier tools remain completed history and are not automatically reexecuted.
This exception avoids spending the model execution deadline while a human is
answering. It does not change ordinary Read/Bash tool continuations.

The HTTP concurrency slot is released while Desktop works. There are at most
32 active jobs and parked questions combined, and 32 pending calls per job. Calls are delivered one at a
time to Anthropic clients. The overall job deadline defaults to 300 seconds and
cannot be configured above the courier's 300-second call budget. This is an
overall execution deadline, not an idle timeout refreshed after each tool
result. A parked human question has its own 30-minute answer deadline.

## Build and prepare

```powershell
go build -o quick2api.exe ./cmd/quick2api
go build -o quick2api-mcp.exe ./cmd/quick2api-mcp
```

The MCP process accepts `-config <absolute-path>`. It reads the existing API key
from that file; do not put keys in MCP JSON, command-line arguments, or logs.
It requires `server.listen: 127.0.0.1:<port>` (or `[::1]:<port>`) and a nonempty
`api.api_key`. Use an isolated candidate configuration and port first.

The standard MCP registration JSON is:

```json
{
  "mcpServers": {
    "quick2api-client-relay": {
      "command": "D:/UserProgram/quick2api/candidates/native-relay-20261124/quick2api-mcp.exe",
      "args": ["-config", "D:/UserProgram/quick2api/candidates/native-relay-20261124/config.yaml"]
    }
  }
}
```

This is a registration template, not evidence that registration succeeded.
Quick's shipped UI uses an OS confirmation dialog and confirmation nonce;
cloud registration additionally uses an attestation tied to an outpost device.
Use that supported confirmation flow. Do not create confirmation files, extract
signing keys, fabricate signatures, or attach to Desktop's port 8780.

After registration, set `quick.profile_root` to the existing Quick account
profile directory containing that approved MCP registration. Spawned outposts
receive it as `QUICKWORK_ROOT`; the proxy does not copy or create approvals and
does not choose an account automatically. When omitted the process environment
is preserved. Attached outposts keep their own startup profile.

Verify the proxy-owned `GET /local/mcp/list`: its server must be enabled,
connected, and expose the exact relay name with the routing schema. The built-in
`/local/list_tools` catalog does not include MCP tools. Live observation on this
installation returned `quick2api_client_relay__quick2api_client_call`; do not
derive a name by guessing an `mcp__` prefix. Native MCP calls carry a `params`
envelope inside their courier arguments; forward it unchanged. Do not unwrap
the client's own `arguments` field.

Only after that check succeeds, configure:

```yaml
quick:
  profile_root: "<existing approved account profile directory>"
tools:
  mode: passthrough
  client_transport: mcp
  native_relay_name: "<exact relay name from the proxy-owned tool catalog>"
  bridge_timeout_seconds: 300
```

The transport is selected for Anthropic Messages. OpenAI Chat Completions keeps
its existing compatibility behavior. The broker endpoint is registered only in
native mode, requires the API key and a loopback peer, and executes no local tool
itself. The MCP stdio implementation uses protocol version `2024-11-05`.

## Failure semantics and current limits

- Messages, CountTokens and Chat Completions accept JSON request bodies up to
  32 MiB by default. Configure `server.max_request_body_bytes` to change the
  limit (0 uses the default; the maximum is 128 MiB). This replaces the previous
  hardcoded 4 MiB ceiling, which rejected a restored Desktop conversation before
  it reached Quick. Oversized bodies return HTTP 413 `request_too_large`;
  malformed JSON returns HTTP 400. The entire body is checked, including
  unknown-length input and trailing whitespace. History is not trimmed to fit.
  This HTTP input limit does not raise the separate per-tool/result budgets.
- No replay, alternate-account retry, prompt fallback, or chunk fallback in
  native jobs. Only an explicit, validated human answer starts a new inference
  at the question boundary described above. Policy denial and content filtering
  remain failures/refusals.
- New submissions for an active named conversation are rejected. An identical
  initial request is also rejected for one job lifetime, including after
  completion; a bounded 1,024-entry receipt cache fails closed when full.
  This is bounded duplicate protection, not a distributed exactly-once promise.
  An initial request that parks a question retains its receipt throughout the
  answer window and one execution lifetime afterward, including anonymous
  conversations.
- Forked, compacted or changed pending transcripts are rejected. There is no
  pending-job recovery across proxy restart. The user must not rerun potentially
  completed external actions blindly after an interrupted result delivery.
  Equivalent text string/block representations and moved content-block
  `cache_control` hints are normalized during continuation matching; actual
  content and tool inputs remain exact.
  Continuation validation failures return HTTP 400 and `x-should-retry: false`,
  so intermediate proxies discarding that header cannot create an SDK 409
  retry loop. Expired handles cannot be restored by resending a result alone.
  Desktop can merge an ended question's result with newly submitted text in
  one user message. That explicit new submission is accepted when it contains
  one result followed only by nonempty text and the preceding assistant message
  is its matching, schema-valid AskUserQuestion. The complete real transcript
  is retained; a fresh inference is instructed to answer the new text and leave
  older actions as history. Initial-request receipts also protect this shape
  from duplicate inference. This is not automatic recovery of the expired job.
  Extra user content on an ordinary live tool result remains rejected rather
  than silently discarded. Reconcile potentially completed external work before
  repeating it after interruption.
- Text results only, at most 32 KiB after JSON encoding. Images, documents and
  oversized results fail explicitly; content is not silently discarded or
  truncated. The courier separately rejects responses above 48,000 bytes.
  Multimodal *input* still uses the existing attachment path.
- Downstream write failure, MCP cancellation, expiry and service shutdown end
  the bridge job and close its socket. An already delivered Desktop tool may
  continue executing; cancellation cannot undo its effects.
- Only the exact configured relay can be couriered. Other device tools return
  an explicit error and are not executed by this bridge.
- Native SSE forwards actual upstream text deltas as they arrive. Narration
  preceding a client call is delivered as a text block before its tool_use;
  continuation matching validates both blocks. Text is never interpreted as
  executable markup. The unread text queue is bounded at 1 MiB or 4096 text
  events; overflow or a
  failed downstream write cancels the job instead of silently dropping output.
  A partial stream followed by an error remains an error, not a completed turn.
- A native registry entry does not guarantee Quick will invoke it in every
  context. Model-level refusal and upstream policy still apply.
- The receiving client's presentation contract stays inline with the native
  routing instructions, including when history/catalog data is attached. Normal
  answers use Markdown; Quick-specific `<decision>` / `<option>` markup does
  not create client UI. When the catalog declares `AskUserQuestion`, its exact
  description/schema (up to 8 KiB) is repeated inline so a genuine user decision
  can use the real client tool and wait for its answer. Tool choice and schema
  validation still apply. This is model guidance, not guaranteed compliance;
  literal markup examples remain text and are never parsed into tool calls.

## Verification and acceptance

### Completed-turn performance checkpoints

After successful delivery of a normally completed native turn, the adapter may
retain one checkpoint for its named client conversation, bounded to 32 entries
and 10 minutes. It stores normalized transcript fingerprints, the system/model/
tool contract and a private socket-pool key, not another copy of raw history.
Only an exact prefix match including the delivered assistant response, followed
by a new user message and optional environment messages, permits a delta turn.
The bridge pins that still-live socket to its original account/device/tenant;
the new request sends only the new message and newly referenced attachments.
Each job still uses a fresh invocation route and retains its own receipts.

Missing/expired sockets, changed history or contract, forks, compaction and
anonymous requests seed a fresh upstream UUID with the full real transcript.
An already started native Send is never replayed or sent to another account.
Active native sockets are protected from pool sweeping; shutdown still closes
them. Failed writes, refusals, partial closes and parked human questions do not
create checkpoints. AskUserQuestion retains its explicit stopped-job/real-answer
boundary and thus still needs full initialization after the human answers.

Metadata-only diagnostics report connection reuse, full/sent byte counts,
preparation stages, time to first text/thinking and response delivery. These are
wall-clock stages, not estimates of model prefill, hidden thinking or queue time.
Streaming improves visibility and checkpoint reuse avoids redundant delivery;
neither guarantees a particular cloud response time or old-session recovery.

An isolated live Quick comparison on 2026-11-24 used 199,319 bytes of initial
synthetic request JSON and checked recall of an inert lookup value from the
original context. The previous transport took 11.918 s for its cold turn and
10.149 s for the next full-history turn. The checkpoint candidate took 11.698 s
cold and 3.211 s for the continuation (first text at 2.951 s). Diagnostics
confirmed reuse of the same live socket and a reduction of rendered context
from 196,931 to 217 bytes, before per-turn relay guidance. Both versions passed
the reference recall check. This is one synthetic comparison, not a benchmark
of the user's large Desktop conversation or a guaranteed speedup.

Local tests exercise actual loopback HTTP and WebSocket IO, plus an injected
runner for state transitions. They cover repeated calls with one job, pending
result delivery, transcript/catalog/session changes, precise numeric identity,
duplicates, expiry, cancellation, route authorization, codec budgets, single
argument delivery, real application errors, courier allowlists and no retries.
Human-question tests cover answers after the old execution deadline, stopping
the old exchange, exact-contract rejection without consuming the answer,
single-use continuation, expiry/shutdown cleanup and the timer/admission race.
They also cover Desktop's merged new-text/ended-question representation after
restart, rejecting mismatched calls, ordinary-tool recovery, empty text,
multimodal extras, multiple results and duplicate new submissions.
These tests establish local adapter/state behavior, not cloud acceptance.

Request-body tests cover UTF-8 byte boundaries, declared and unknown lengths,
trailing padding, malformed or multiple JSON values, all three API routes and
config validation. A 5 MiB native Messages request reaches one injected runner
with its complete history intact. The deployed candidate also accepted a
5,242,939-byte CountTokens request locally, rejected a declared 33,554,433-byte
body with HTTP 413, and retained HTTP 400 for malformed JSON. These decoding
checks invoke no Quick inference; the restored conversation still requires an
actual user-driven retry.

Live acceptance on 2026-11-24 also used the installed Claude Code client with
only Read enabled, the candidate API endpoint, a temporary child-process
environment and no session persistence. It completed the approved fixture read
below exactly once and returned the expected first line. That test exposed
string-to-text-block conversion and content-block cache hints on continuation;
both representations are now normalized without changing actual tool input or
history content. It establishes the tested Claude Code path.

A subsequent fresh Claude Desktop session used CC Switch's Desktop provider
with both its endpoint and credential matched to the isolated candidate. The
Desktop UI showed the Read and final line; its own transcript confirmed exactly
one Read, one correlated tool result, no tool error and one completed turn.
The test changed no permission settings. Selecting a provider called Quick2API
alone does not select this transport: an older provider entry can still point
to the compatibility service with a different credential. Verify the actual
endpoint and credential together before acceptance. Historical-session refusal
recovery still requires separate, user-driven acceptance.

A restored Desktop conversation subsequently passed the former 4 MiB request
rejection but returned a choice as literal `<decision>` / `<option>` text, with
no client tool call despite declaring `AskUserQuestion`. A separate real Quick
test with the deployed presentation guidance returned an actual native
`AskUserQuestion` call and continued after a test-client answer. This verifies
the native question round-trip only. A subsequent Desktop question did render
and receive the user's choice, but its continuation failed after the entire
300-second job deadline elapsed while waiting for the user. The human-question
boundary above addresses that lifetime mismatch; live acceptance of that
revised continuation is recorded separately from the earlier same-job test.

Reproducing acceptance requires a formally registered relay on the proxy-owned outpost,
then one fresh harmless request asking Desktop to Read only:

`D:/UserProgram/quick2api/captures/desktop-tool-check.txt`

Verify an actual upstream native call, actual Desktop file read, correlated
client result, same upstream job/socket and final answer. Do not replay or
rewrite previously filtered requests as part of this test. Basic `-probe` only
tests connectivity and does not establish native relay operation.
