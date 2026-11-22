// types.go collects the shared value types of the chat package: the wire
// frame, the turn result/event/option shapes, the upstream error, and a few
// small helpers (UUID, string truncation) used across conn/turn/prompt.
package chat

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Frame is one decoded inner protocol frame.
type Frame struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`

	// Envelope fields that appear on some inbound frames.
	MessageID string `json:"message_id,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
}

// TurnUsage mirrors the turn_usage frame payload (defensive: all optional).
type TurnUsage struct {
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_input_tokens"`
	CacheWriteTokens int64   `json:"cache_write_input_tokens"`
	Model            string  `json:"model"`
	ResolvedModelID  string  `json:"resolved_model_id"`
	CostUSD          float64 `json:"cost_usd"`
	ContextWindow    int64   `json:"context_window"`
	// Fallback spellings seen on some builds.
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// TurnResult is the completed turn.
type TurnResult struct {
	Text           string
	FinishReason   string // "stop" | "content_filter" | "error"
	Usage          *TurnUsage
	ResolvedModel  string
	PartialOnClose bool
}

// TurnEvent is a streaming callback event.
type TurnEvent struct {
	Kind  string // "delta" | "thinking" | "usage" | "frame" | "done" | "error"
	Text  string
	Usage *TurnUsage
	Frame *Frame // raw frame when Kind=="frame"
	Err   error
}

// TurnOpts controls one RunTurn call.
type TurnOpts struct {
	Text           string
	AgentMode      string
	ThinkingEffort string
	UserTimezone   string
	UserLocale     string
	Title          string
	// OnEvent is invoked synchronously from the read loop; implementations
	// should be fast or hand off to a channel.
	OnEvent func(TurnEvent)
}

// Client wires the WebSocket connection.
type Client struct {
	SurfaceType      string
	Channel          string
	Origin           string
	AppVersion       string // quickwork desktop version, sent in session_connect
	HandshakeTimeout time.Duration
	StrictHandshake  bool
	Proxy            string // "", or "direct", or a proxy URL
}

// UpstreamError is an explicit error frame from the upstream.
type UpstreamError struct {
	Code    string
	Message string
}

func (e *UpstreamError) Error() string {
	msg := e.Message
	if msg == "" {
		// The upstream occasionally sends {"type":"error","payload":null};
		// keep a marker so the caller doesn't log a bare "upstream: ".
		msg = "(empty upstream error frame)"
	}
	if e.Code != "" {
		return "upstream " + e.Code + ": " + msg
	}
	return "upstream: " + msg
}

// inbound is one read result off the websocket.
type inbound struct {
	f   *Frame
	err error
}

// splitText cuts s into pieces of at most n bytes on rune boundaries.
func splitText(s string, n int) []string {
	if len(s) <= n {
		return []string{s}
	}
	var out []string
	for len(s) > 0 {
		cut := n
		if cut > len(s) {
			cut = len(s)
		}
		for cut > 0 && !utf8.ValidString(s[:cut]) {
			cut--
		}
		if cut == 0 {
			cut = n
			if cut > len(s) {
				cut = len(s)
			}
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return out
}

func orElse(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var uuidCounter uint64

// newUUID returns a random RFC-4122 v4 UUID without pulling a dependency.
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		b[6] = (b[6] & 0x0f) | 0x40
		b[8] = (b[8] & 0x3f) | 0x80
		return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
			b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	// Extremely unlikely fallback: counter-based pseudo-uuid.
	n := atomic.AddUint64(&uuidCounter, 1)
	t := uint64(time.Now().UnixNano())
	return fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x",
		uint32(t>>32), uint16(t>>16), uint16(t)&0xfff, uint16(n)&0xfff, n&0xffffffffffff)
}
