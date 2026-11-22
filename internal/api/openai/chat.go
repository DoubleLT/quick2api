// Package openai adapts OpenAI /v1/chat/completions onto one Quick turn.
// v1 deliberately covers the text surface: system/developer messages render
// as a leading transcript block, multi-turn history flattens into a single
// user_message (Quick's agent keeps its own thread state).
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kilbertert/quick2api/internal/bridge"
	"github.com/kilbertert/quick2api/internal/chat"
	"github.com/kilbertert/quick2api/internal/config"
	"github.com/kilbertert/quick2api/internal/debuglog"
)

// Handler serves OpenAI-compatible endpoints.
type Handler struct {
	Bridge *bridge.Bridge
	Cfg    *config.Config
	Debug  *debuglog.Logger
	Sem    chan struct{} // concurrency ceiling
}

// ---------- request/response types (subset of the OpenAI schema) ----------

type ChatRequest struct {
	Model            string          `json:"model"`
	Messages         []Message       `json:"messages"`
	Stream           bool            `json:"stream"`
	StreamOptions    *StreamOptions  `json:"stream_options"`
	MaxTokens        int             `json:"max_tokens"`
	Temperature      *float64        `json:"temperature"`
	ReasoningEffort  string          `json:"reasoning_effort"`
	Metadata         json.RawMessage `json:"metadata"`
	ResponseFormat   json.RawMessage `json:"response_format"`
	Tools            json.RawMessage `json:"tools"`
	ToolChoice       json.RawMessage `json:"tool_choice"`
	Stop             json.RawMessage `json:"stop"`
	N                int             `json:"n"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Name    string          `json:"name"`
}

type choice struct {
	Index        int            `json:"index"`
	Message      *MessageOut    `json:"message,omitempty"`
	Delta        *MessageOut    `json:"delta,omitempty"`
	FinishReason string         `json:"finish_reason,omitempty"`
}

type MessageOut struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type completion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   *usage   `json:"usage,omitempty"`
}

type usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	// Quick-specific extensions (all optional).
	CacheReadTokens   int64   `json:"cache_read_input_tokens,omitempty"`
	CacheWriteTokens  int64   `json:"cache_write_input_tokens,omitempty"`
	ResolvedModelID   string  `json:"quick_resolved_model_id,omitempty"`
	CostUSD           float64 `json:"quick_cost_usd,omitempty"`
	ContextWindow     int64   `json:"quick_context_window,omitempty"`
}

// ---------- endpoint ----------

// ChatCompletions handles POST /v1/chat/completions.
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	reqID := reqID(r)
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", "")
		return
	}
	var req ChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "bad JSON: "+err.Error(), "")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages is empty", "")
		return
	}
	if req.N > 1 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "n>1 unsupported: Quick runs one turn per request", "")
		return
	}
	if len(req.Tools) > 0 && string(req.Tools) != "null" && string(req.Tools) != "[]" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "client-side tools unsupported in v1: Quick's agent runs its own tools upstream", "")
		return
	}
	text, err := renderTranscript(req.Messages)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}

	// concurrency ceiling
	if h.Sem != nil {
		select {
		case h.Sem <- struct{}{}:
			defer func() { <-h.Sem }()
		case <-r.Context().Done():
			return
		default:
			writeError(w, http.StatusTooManyRequests, "rate_limit_error",
				"local concurrency limit reached", "")
			return
		}
	}

	model := req.Model
	if model == "" {
		model = firstModel(h.Cfg)
	}
	agentMode := h.Cfg.AgentModeFor(model)
	thinking := h.Cfg.Quick.ThinkingEffort
	if req.ReasoningEffort != "" {
		thinking = req.ReasoningEffort
	}

	dlogDir := h.Debug.Dir(reqID)
	h.Debug.Write(dlogDir, "00-request", map[string]any{
		"id": reqID, "model": model, "agent_mode": agentMode,
		"stream": req.Stream, "messages": req.Messages,
	})

	ctx, cancel := context.WithTimeout(r.Context(), h.Cfg.TurnTimeout())
	defer cancel()

	if req.Stream {
		h.stream(w, r, ctx, reqID, model, agentMode, thinking, text, dlogDir)
		return
	}
	h.complete(w, r, ctx, reqID, model, agentMode, thinking, text, dlogDir)
}

// complete handles the non-streaming path.
func (h *Handler) complete(w http.ResponseWriter, r *http.Request, ctx context.Context,
	reqID, model, agentMode, thinking, text, dlogDir string) {

	var frames []json.RawMessage
	if h.Debug.LogFrames {
		frames = make([]json.RawMessage, 0, 64)
	}
	res, err := h.Bridge.RunTurn(ctx, chat.TurnOpts{
		Text:           text,
		AgentMode:      agentMode,
		ThinkingEffort: thinking,
		UserTimezone:   h.Cfg.Quick.UserTimezone,
		UserLocale:     h.Cfg.Quick.UserLocale,
		Title:          "quick2api " + reqID,
		OnEvent: func(ev chat.TurnEvent) {
			if h.Debug.LogFrames && ev.Kind == "frame" && ev.Frame != nil {
				if raw, err := json.Marshal(ev.Frame); err == nil {
					frames = append(frames, raw)
				}
			}
		},
	})
	if err != nil {
		h.finishDebug(dlogDir, nil, err)
		writeUpstreamError(w, err)
		return
	}
	h.Debug.Write(dlogDir, "01-frames", frames)
	out := completion{
		ID:      "chatcmpl-" + reqID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   orElse(res.ResolvedModel, model),
		Choices: []choice{{
			Index:        0,
			Message:      &MessageOut{Role: "assistant", Content: res.Text},
			FinishReason: res.FinishReason,
		}},
		Usage: mapUsage(res.Usage),
	}
	h.finishDebug(dlogDir, &out, nil)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// stream handles the SSE path.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request, ctx context.Context,
	reqID, model, agentMode, thinking, text, dlogDir string) {

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "streaming unsupported", "")
		return
	}
	w.WriteHeader(http.StatusOK)

	emit := func(v any) bool {
		data, err := json.Marshal(v)
		if err != nil {
			return false
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", data)
		fl.Flush()
		return err == nil
	}

	// role chunk first (OpenAI convention)
	emit(completion{
		ID: "chatcmpl-" + reqID, Object: "chat.completion.chunk",
		Created: time.Now().Unix(), Model: model,
		Choices: []choice{{Index: 0, Delta: &MessageOut{Role: "assistant"}}},
	})

	res, err := h.Bridge.RunTurn(ctx, chat.TurnOpts{
		Text:           text,
		AgentMode:      agentMode,
		ThinkingEffort: thinking,
		UserTimezone:   h.Cfg.Quick.UserTimezone,
		UserLocale:     h.Cfg.Quick.UserLocale,
		Title:          "quick2api " + reqID,
		OnEvent: func(ev chat.TurnEvent) {
			switch ev.Kind {
			case "delta":
				emit(completion{
					ID: "chatcmpl-" + reqID, Object: "chat.completion.chunk",
					Created: time.Now().Unix(), Model: model,
					Choices: []choice{{Index: 0, Delta: &MessageOut{Content: ev.Text}}},
				})
			case "frame":
				if h.Debug.LogFrames && ev.Frame != nil {
					h.Debug.Write(dlogDir, "01-frames", ev.Frame)
				}
			}
		},
	})
	if err != nil {
		h.finishDebug(dlogDir, nil, err)
		// OpenAI streams still terminate with [DONE]; send an error event
		// chunk so clients see a typed failure rather than a silent cut.
		emit(map[string]any{"error": map[string]any{
			"type": "upstream_error", "message": err.Error(),
		}})
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
		return
	}
	result := res

	// final chunk: finish_reason + usage (OpenAI convention puts usage on a
	// dedicated final chunk when include_usage is requested; we always send
	// it — the payload is small and clients tolerate it).
	fin := completion{
		ID: "chatcmpl-" + reqID, Object: "chat.completion.chunk",
		Created: time.Now().Unix(), Model: orElse(result.ResolvedModel, model),
		Choices: []choice{{Index: 0, Delta: &MessageOut{}, FinishReason: result.FinishReason}},
		Usage:   mapUsage(result.Usage),
	}
	emit(fin)
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
	h.finishDebug(dlogDir, &completion{
		ID: "chatcmpl-" + reqID, Object: "chat.completion",
		Created: time.Now().Unix(), Model: orElse(result.ResolvedModel, model),
		Choices: []choice{{Index: 0, Message: &MessageOut{Role: "assistant", Content: result.Text}, FinishReason: result.FinishReason}},
		Usage: mapUsage(result.Usage),
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

// Models handles GET /v1/models: every agent_modes key + extra `models` IDs.
func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	ids := map[string]struct{}{}
	for k := range h.Cfg.Quick.AgentModes {
		ids[k] = struct{}{}
	}
	for _, m := range h.Cfg.Quick.Models {
		ids[m] = struct{}{}
	}
	if len(ids) == 0 {
		ids[firstModel(h.Cfg)] = struct{}{}
	}
	list := make([]map[string]any, 0, len(ids))
	for id := range ids {
		entry := map[string]any{
			"id":       id,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": "quick2api",
		}
		if target := h.Cfg.AgentModeFor(id); target != id {
			entry["agent_mode"] = target
		}
		list = append(list, entry)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": list})
}

// ---------- helpers ----------

// renderTranscript flattens OpenAI messages into the single text the
// upstream user_message carries. Quick's agent tracks its own threads, so
// history must be explicit — roles render as plain labels.
func renderTranscript(msgs []Message) (string, error) {
	var b strings.Builder
	for _, m := range msgs {
		content, err := flattenContent(m.Content)
		if err != nil {
			return "", fmt.Errorf("%s message: %w", m.Role, err)
		}
		if content == "" {
			continue
		}
		label := ""
		switch m.Role {
		case "system", "developer":
			label = "System"
		case "user":
			label = "User"
		case "assistant":
			label = "Assistant"
		case "tool":
			label = "Tool"
		default:
			label = strings.Title(m.Role)
		}
		fmt.Fprintf(&b, "%s: %s\n\n", label, content)
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", errors.New("messages rendered to empty text")
	}
	return out, nil
}

// flattenContent accepts either a string or an array of content parts.
// Only text-bearing parts are supported in v1.
func flattenContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("content must be a string or an array of parts")
	}
	var b strings.Builder
	for _, p := range parts {
		t, _ := p["type"].(string)
		switch t {
		case "text":
			if v, ok := p["text"].(string); ok {
				b.WriteString(v)
			}
		case "refusal":
			if v, ok := p["refusal"].(string); ok {
				b.WriteString("[refusal] " + v)
			}
		default:
			return "", fmt.Errorf("unsupported content part %q: v1 is text-only", t)
		}
	}
	return b.String(), nil
}

func mapUsage(u *chat.TurnUsage) *usage {
	if u == nil {
		return nil
	}
	return &usage{
		PromptTokens:    u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:     u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
		ResolvedModelID:  u.ResolvedModelID,
		CostUSD:          u.CostUSD,
		ContextWindow:    u.ContextWindow,
	}
}

func writeUpstreamError(w http.ResponseWriter, err error) {
	var ue *chat.UpstreamError
	if errors.As(err, &ue) {
		code := "upstream_error"
		status := http.StatusBadGateway
		switch ue.Code {
		case "unauthenticated", "token_expired":
			status = http.StatusUnauthorized
			code = "authentication_error"
		case "rate_limited", "throttled":
			status = http.StatusTooManyRequests
			code = "rate_limit_error"
		}
		writeError(w, status, code, ue.Message, ue.Code)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, "timeout", err.Error(), "")
		return
	}
	writeError(w, http.StatusBadGateway, "upstream_error", err.Error(), "")
}

func writeError(w http.ResponseWriter, status int, typ, msg, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg, "type": typ, "code": code, "param": nil,
		},
	})
}

func reqID(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" {
		return v
	}
	return fmt.Sprintf("q2a-%d", time.Now().UnixNano())
}

func firstModel(c *config.Config) string {
	for k := range c.Quick.AgentModes {
		return k
	}
	return "quick"
}

func orElse(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
