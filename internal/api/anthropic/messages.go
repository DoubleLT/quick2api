// Package anthropic adapts Anthropic /v1/messages onto one Quick turn.
//
// Why it exists: Claude Code (and the wider Anthropic-SDK ecosystem)
// speaks /v1/messages + SSE, not /v1/chat/completions. Serving it here
// lets a `claude` CLI point ANTHROPIC_BASE_URL at quick2api and run on
// the Quick subscription — no second proxy hop.
//
// Scope: the text surface only. Client-declared tools are flattened
// into a leading instruction block (the upstream agent sees their
// schemas as text); tool_use content blocks in the request are
// rendered as "ToolUse <name>(<json>)" so history stays coherent.
// Stop sequences and max_tokens are passed positionally but the
// upstream turn doesn't honor them server-side — they're metadata.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kilbertert/quick2api/internal/api/common"
	"github.com/kilbertert/quick2api/internal/bridge"
	"github.com/kilbertert/quick2api/internal/chat"
	"github.com/kilbertert/quick2api/internal/config"
	"github.com/kilbertert/quick2api/internal/debuglog"
	"github.com/kilbertert/quick2api/internal/util"
)

// Handler serves Anthropic-compatible endpoints.
type Handler struct {
	Bridge *bridge.Bridge
	Cfg    *config.Config
	Debug  *debuglog.Logger
	Sem    chan struct{}
}

// ---------- request/response types (subset of the Messages schema) ----------

type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type MessagesRequest struct {
	Model         string          `json:"model"`
	Messages      []Message       `json:"messages"`
	System        json.RawMessage `json:"system,omitempty"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	Stream        bool            `json:"stream"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Tools         json.RawMessage `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	Thinking      json.RawMessage `json:"thinking,omitempty"`
}

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
}

type anthropicMessage struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []ContentBlock `json:"content"`
	StopReason   string         `json:"stop_reason"`
	StopSequence string         `json:"stop_sequence"`
	Usage        anthropicUsage `json:"usage"`
}

// ---------- endpoint ----------

// Messages handles POST /v1/messages.
func (h *Handler) Messages(w http.ResponseWriter, r *http.Request) {
	reqID := common.ReqID(r)
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "use POST")
		return
	}
	var req MessagesRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "bad JSON: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "messages is empty")
		return
	}

	text, err := renderPrompt(&req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	release, ok := common.Acquire(r.Context(), h.Sem)
	if !ok {
		if r.Context().Err() == nil {
			writeErr(w, http.StatusTooManyRequests, "rate_limit_error", "local concurrency limit reached")
		}
		return
	}
	defer release()

	model := req.Model
	if model == "" {
		model = common.FirstModel(h.Cfg)
	}
	agentMode := h.Cfg.AgentModeFor(model)
	thinking := h.Cfg.Quick.ThinkingEffort
	if len(req.Thinking) > 0 && string(req.Thinking) != "null" {
		var t struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(req.Thinking, &t) == nil && t.Type == "enabled" {
			thinking = "high"
		}
	}

	dlogDir := h.Debug.Dir(reqID)
	h.Debug.Write(dlogDir, "00-request", map[string]any{
		"id": reqID, "model": model, "agent_mode": agentMode,
		"stream": req.Stream, "messages": len(req.Messages),
	})

	ctx, cancel := context.WithTimeout(r.Context(), h.Cfg.TurnTimeout())
	defer cancel()

	if req.Stream {
		h.stream(w, ctx, reqID, model, agentMode, thinking, text, dlogDir)
		return
	}
	h.complete(w, ctx, reqID, model, agentMode, thinking, text, dlogDir)
}

// complete handles the non-streaming path.
func (h *Handler) complete(w http.ResponseWriter, ctx context.Context,
	reqID, model, agentMode, thinking, text, dlogDir string) {

	frames := common.NewFrames(h.Debug.LogFrames)
	res, err := h.Bridge.RunTurn(ctx, chat.TurnOpts{
		Text:           text,
		AgentMode:      agentMode,
		ThinkingEffort: thinking,
		UserTimezone:   h.Cfg.Quick.UserTimezone,
		UserLocale:     h.Cfg.Quick.UserLocale,
		Title:          "quick2api " + reqID,
		OnEvent:        frames.OnEvent,
	})
	// Always write frames before returning — the error path is exactly
	// where they are needed to see the upstream's real error payload.
	frames.Write(h.Debug, dlogDir)
	if err != nil {
		h.finishDebug(dlogDir, nil, err)
		writeUpstream(w, err)
		return
	}

	content := parseToolCalls(res.Text)
	out := anthropicMessage{
		ID:      "msg_" + reqID,
		Type:    "message",
		Role:    "assistant",
		Model:   util.Or(res.ResolvedModel, model),
		Content: content,
		Usage:   mapUsage(res.Usage),
	}
	if len(content) == 1 && content[0].Type == "text" {
		out.StopReason = "end_turn"
	} else {
		out.StopReason = "tool_use"
	}
	h.finishDebug(dlogDir, &out, nil)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// stream emits Anthropic SSE: message_start → content_block_start →
// content_block_delta(text) ×N → content_block_stop → message_delta →
// message_stop. Tool-use blocks can't stream (the input arrives whole
// inside the text); when the finished text parses as a tool call we
// still emit it as a single content_block — legal per the spec.
func (h *Handler) stream(w http.ResponseWriter, ctx context.Context,
	reqID, model, agentMode, thinking, text, dlogDir string) {

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "api_error", "streaming unsupported")
		return
	}
	w.WriteHeader(http.StatusOK)

	emit := func(event string, v any) bool {
		data, err := json.Marshal(v)
		if err != nil {
			return false
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		fl.Flush()
		return true
	}

	msgID := "msg_" + reqID
	started := false
	start := func() {
		if started {
			return
		}
		started = true
		emit("message_start", map[string]any{
			"message": map[string]any{
				"id": msgID, "type": "message", "role": "assistant",
				"model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})
		emit("content_block_start", map[string]any{
			"index":         0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	}
	start()

	frames := common.NewFrames(h.Debug.LogFrames)
	res, err := h.Bridge.RunTurn(ctx, chat.TurnOpts{
		Text:           text,
		AgentMode:      agentMode,
		ThinkingEffort: thinking,
		UserTimezone:   h.Cfg.Quick.UserTimezone,
		UserLocale:     h.Cfg.Quick.UserLocale,
		Title:          "quick2api " + reqID,
		OnEvent: func(ev chat.TurnEvent) {
			frames.OnEvent(ev)
			if ev.Kind == "delta" && ev.Text != "" {
				emit("content_block_delta", map[string]any{
					"index": 0,
					"delta": map[string]any{"type": "text_delta", "text": ev.Text},
				})
			}
		},
	})
	frames.Write(h.Debug, dlogDir)

	// close the text block even on error — clients expect a terminated stream
	emit("content_block_stop", map[string]any{"index": 0})

	if err != nil {
		h.finishDebug(dlogDir, nil, err)
		emit("message_delta", map[string]any{
			"delta": map[string]any{"stop_reason": "error"},
			"usage": map[string]any{"output_tokens": 0},
		})
		emit("error", map[string]any{"type": "error", "error": map[string]any{
			"type": "api_error", "message": err.Error(),
		}})
		emit("message_stop", map[string]any{})
		return
	}

	// If the whole answer is a tool call, translate it into a tool_use block
	// (still a single block; legal to emit after the text block closes).
	content := parseToolCalls(res.Text)
	stopReason := "end_turn"
	if len(content) == 1 && content[0].Type == "tool_use" {
		stopReason = "tool_use"
		emit("content_block_start", map[string]any{
			"index":         1,
			"content_block": content[0],
		})
		emit("content_block_delta", map[string]any{
			"index": 1,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": string(content[0].Input),
			},
		})
		emit("content_block_stop", map[string]any{"index": 1})
	}

	u := mapUsage(res.Usage)
	emit("message_delta", map[string]any{
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"input_tokens":                 u.InputTokens,
			"output_tokens":                u.OutputTokens,
			"cache_read_input_tokens":      u.CacheReadInputTokens,
			"cache_creation_input_tokens":  u.CacheCreationInputTokens,
		},
	})
	emit("message_stop", map[string]any{})
	h.finishDebug(dlogDir, &anthropicMessage{
		ID: msgID, Type: "message", Role: "assistant",
		Model: util.Or(res.ResolvedModel, model), Content: content,
		StopReason: stopReason, Usage: u,
	}, nil)
}

func (h *Handler) finishDebug(dir string, out any, runErr error) {
	rec := map[string]any{"ts": time.Now().UTC()}
	if runErr != nil {
		rec["error"] = runErr.Error()
	}
	if out != nil {
		rec["response"] = out
	}
	h.Debug.Write(dir, "02-response", rec)
}

// ---------- helpers ----------

// renderPrompt flattens the Anthropic request into the single text the
// upstream user_message carries. System becomes a leading "System:"
// block; tool declarations become an instruction block so the agent can
// see its own tools' schemas.
func renderPrompt(req *MessagesRequest) (string, error) {
	var b strings.Builder

	// system: string | [ {type:"text",text:…} ]
	if len(req.System) > 0 && string(req.System) != "null" {
		var sys string
		if err := json.Unmarshal(req.System, &sys); err == nil {
			if sys != "" {
				fmt.Fprintf(&b, "System: %s\n\n", sys)
			}
		} else {
			var parts []ContentBlock
			if err := json.Unmarshal(req.System, &parts); err == nil {
				for _, p := range parts {
					if p.Type == "text" && p.Text != "" {
						fmt.Fprintf(&b, "System: %s\n\n", p.Text)
					}
				}
			}
		}
	}

	// tools → instruction block so the agent can see its own schemas.
	if len(req.Tools) > 0 && string(req.Tools) != "null" && string(req.Tools) != "[]" {
		b.WriteString("The user has declared the following tools available for use. " +
			"When you want to use one, respond with ONLY the JSON object " +
			"{\"type\":\"tool_use\",\"id\":\"<id>\",\"name\":\"<tool name>\"," +
			"\"input\":{...}}. Do not wrap it in markdown.\nTools:\n")
		b.WriteString(string(req.Tools))
		b.WriteString("\n\n")
	}

	for _, m := range req.Messages {
		// content: string | [ blocks ]
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			if s != "" {
				label := "User"
				if m.Role == "assistant" {
					label = "Assistant"
				}
				fmt.Fprintf(&b, "%s: %s\n\n", label, s)
			}
			continue
		}
		var blocks []ContentBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return "", fmt.Errorf("%s message: content must be a string or blocks", m.Role)
		}
		for _, bl := range blocks {
			switch bl.Type {
			case "text":
				label := "User"
				if m.Role == "assistant" {
					label = "Assistant"
				}
				fmt.Fprintf(&b, "%s: %s\n\n", label, bl.Text)
			case "tool_use":
				fmt.Fprintf(&b, "ToolUse %s(%s)\n\n", bl.Name, string(bl.Input))
			case "tool_result":
				inner := ""
				if len(bl.Content) > 0 {
					var s string
					if json.Unmarshal(bl.Content, &s) == nil {
						inner = s
					} else {
						var parts []ContentBlock
						if json.Unmarshal(bl.Content, &parts) == nil {
							for _, p := range parts {
								if p.Type == "text" {
									inner += p.Text
								}
							}
						}
					}
				}
				label := "ToolResult"
				if bl.IsError {
					label += " (error)"
				}
				fmt.Fprintf(&b, "%s: %s\n\n", label, inner)
			default:
				// images/redacted_thinking/etc — drop silently, upstream is text-only.
			}
		}
	}

	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", errors.New("messages rendered to empty text")
	}
	return out, nil
}

// parseToolCalls detects a single tool_use JSON the model emitted as its
// whole answer. Multiple calls degrade to plain text (acceptable — the
// upstream agent is conversational, not a strict tool runtime).
func parseToolCalls(text string) []ContentBlock {
	trimmed := strings.TrimSpace(text)
	// strip a stray ```json fence if the model wrapped it
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)

	var blk ContentBlock
	if json.Unmarshal([]byte(trimmed), &blk) == nil &&
		blk.Type == "tool_use" && blk.Name != "" && len(blk.Input) > 0 {
		if blk.ID == "" {
			blk.ID = common.NewID("toolu")
		}
		return []ContentBlock{blk}
	}
	return []ContentBlock{{Type: "text", Text: text}}
}

func mapUsage(u *chat.TurnUsage) anthropicUsage {
	if u == nil {
		return anthropicUsage{}
	}
	return anthropicUsage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
		CacheCreationInputTokens: u.CacheWriteTokens,
	}
}

func writeUpstream(w http.ResponseWriter, err error) {
	status, kind, msg := common.UpstreamError(err)
	typ := "api_error"
	switch kind {
	case common.KindUnauthenticated:
		typ = "authentication_error"
	case common.KindRateLimited:
		typ = "rate_limit_error"
	case common.KindTimeout:
		typ = "timeout"
	}
	writeErr(w, status, typ, msg)
}

func writeErr(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}


