# Client tool compatibility and conversation isolation

Validated on Windows with a signed-in Amazon Quick desktop installation on
2026-11-23. This describes a compatibility layer over Quick's agent, not a
native Anthropic model API.

## Symptoms and fixes

### A tool call appears as ordinary text

Anthropic streaming clients dispatch on the JSON `type`, not just the SSE
`event:` header. Each event now includes a matching top-level `type`.
Tool blocks start with `input: {}` and deliver arguments once through
`input_json_delta`, so clients do not merge duplicate arguments.

Quick can also narrate a native attachment read before producing the final
client tool request:

```text
text_delta: I'll read the attached context first.
tool_start / tool_result: Quick reads the attached conversation context
text_delta: {"type":"tool_use","id":"example","name":"Read","input":{...}}
message_complete
```

Previously all text was concatenated and strict JSON parsing failed. The
collector now records a byte offset at each native tool boundary. Only the
final text span is eligible for client-tool JSON parsing in either API adapter.
Ordinary replies retain all text. Arbitrary JSON embedded in prose is not
extracted as an executable command.

### A short action sentence and tool JSON share the final text span

A separate Desktop trace on 2026-11-24 contained both of these in the final
span, after the last native tool boundary:

```text
Need to find the shell session's local id.

{"type":"tool_use","id":"example","name":"list_sessions","input":{"limit":10}}
```

The Anthropic client-passthrough adapter now accepts one short action paragraph,
a blank line, and a complete terminal tool object or array. It preserves the
paragraph as text and emits separate `tool_use` blocks with `stop_reason:
tool_use`. The tool name must match the current request's catalog and its input
must validate against that tool's JSON Schema. Tool choice restrictions apply;
external schema references cannot load URLs or files. Duplicate JSON keys,
duplicate call IDs, extra wrapper fields, incomplete replies, and filtered
replies cannot be promoted.

This is a narrow formatting heuristic, not a reliable semantic intent classifier.
Recognized action prefixes are limited; examples, negation, mixed fenced JSON,
additional paragraphs, and trailing prose remain text. Unrecognized wording may
still fail to produce a tool call. Client permissions remain the execution gate.
This extension applies only to Anthropic client passthrough; OpenAI parsing and
server tool execution retain their existing behavior. No prompt rewriting,
content-filter retry, or native device bridge is introduced.

### Large histories lose the operative request or tool schema

Oversized prompts use the existing attachment path. Trailing environment
messages must not displace the latest user request or tool result from the
inline text. System messages keep their role, and a compact schema catalog
plus a client-execution reminder stays inline with the operative message.
Descriptions remain in the original tool context; compaction preserves schema
constraints and literal data. The reminder is added after delta selection and
does not enter the transcript watermark.

### Replies cross between overlapping requests, forks, or probes

The upstream conversation is stateful. The proxy now serializes requests for
the same normalized client conversation before lane selection and dialing.
Independent conversations remain concurrent; waiting requests can cancel.

Only an append-only transcript with the same preamble reuses a connection.
Reconnects, forks, compaction, and auxiliary probes seed new upstream UUIDs
instead of appending a complete history to an old upstream thread. Cancellation,
errors, filtering, and incomplete replies close the connection and release a
read pump waiting on a full inbound queue.

### Filtering looks like a broken model, or partial JSON becomes a tool call

Quick has returned both `guardrail_blocked` and an `error` payload with
`error_code: content_filtered` / `error: ...`. Both map to `content_filter`;
Anthropic clients receive `stop_reason: refusal`. A blocked reply is not
retried through the chunk fallback. Incomplete Anthropic replies return an
error, and blocked or incomplete output is never executed as a tool request.

Debug logging also records bounded event-name sequences and counts in
`01-events.jsonl`; raw frame bodies still require `debug.log_frames`.

## Validation

Automated checks: `go test ./...`, `go vet ./...`, and `go build ./...` on
Windows/amd64 with Go 1.27.4. Regression cases cover stream event types,
operative-message placement, schema constraints, cancellable conversation
locks, transcript divergence, abandoned queues, filtering, and final-span
tool parsing. Additional regressions cover the 2026-11-24 mixed final span,
schema validation, tool choice, multiple tool blocks, and single-delivery SSE
arguments. These are local parser/state tests, not simulated upstream
proof.

Live checks against a signed-in Quick account established:

- Before serialization, two overlapping requests for one conversation asking
  for ALPHA lines and BETA_COMPLETE both received the ALPHA reply. After the
  fix they returned their respective replies in two runs.
- A main conversation retained its marker across an independent probe using
  the same client id. Cancelling a long reply did not contaminate the next
  reply. A Read request, actual local file read, and subsequent tool_result
  completed successfully.
- After deployment, a real Claude Desktop conversation resumed Bash, Grep,
  and Read calls with corresponding client tool results. One command returned
  an execution error; subsequent tool calls continued normally.
- Two earlier real traces establish the narration/native-tool/final-JSON
  failure shape. Its parser regression passes. The successful Desktop retry
  did not repeat the exact initial-narration sequence, so it is not an exact
  before/after replay of that failure.
- On 2026-11-24, a separate candidate server completed both non-streaming and
  streaming fixture Read/result/final-answer loops against real Quick. The SSE
  check verified event types, block closure, empty starting inputs, and one
  argument delta per call. The exact mixed final-span reply is covered by a
  local regression; those live loops did not establish a Desktop replay of
  that precise formatting or resolve refusals in large old histories.

For an installation check, build `./cmd/quick2api` and run
`quick2api -config <your-config> -probe`. Use a proxy-owned outpost and a
separate free port if another proxy is running. A successful probe verifies
basic upstream connectivity; it does not prove client tool execution.

## Remaining limits

Quick's native tools are distinct from client-provided tools. A syntactically
correct request can still receive a natural-language refusal to use the client
tool contract, or an upstream content-filter refusal. Both occurred in
independent live tests, including a minimal Read test. A successful Desktop
session does not establish universal compatibility.

This change preserves upstream content checks and does not promise to make
every existing conversation resumable. Reseeding a large or divergent history
can increase latency and upstream usage. Local credentials, conversation
captures, SSH commands, and deployment configuration are not included here.
