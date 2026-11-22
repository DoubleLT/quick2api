// Package common holds the pieces both API adapters (openai + anthropic)
// share: request-id generation, the model fallback, the concurrency-slot
// acquire, the upstream-error mapping, and the 01-frames debug collector.
// Schema and wire details stay in the adapters themselves.
package common

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/kilbertert/quick2api/internal/chat"
	"github.com/kilbertert/quick2api/internal/config"
	"github.com/kilbertert/quick2api/internal/debuglog"
)

var idCounter uint64

// NewID mints a unique id with a prefix (tool_use ids, request fallbacks).
func NewID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), atomic.AddUint64(&idCounter, 1))
}

// ReqID is the X-Request-Id when present, else a fresh unique id.
func ReqID(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" {
		return v
	}
	return NewID("q2a")
}

// FirstModel returns a deterministic default model name: the alphabetically
// first configured agent_modes key, else "quick". (The previous
// pick-first-map-iteration was nondeterministic across restarts.)
func FirstModel(c *config.Config) string {
	best := ""
	for k := range c.Quick.AgentModes {
		if best == "" || k < best {
			best = k
		}
	}
	if best == "" {
		return "quick"
	}
	return best
}

// Acquire takes one slot out of the semaphore (non-blocking). ok=false
// means either the request was cancelled while waiting (ctx.Err()!=nil)
// or the pool is full — callers map the second case to a local 429.
func Acquire(ctx context.Context, sem chan struct{}) (release func(), ok bool) {
	if sem == nil {
		return func() {}, true
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	case <-ctx.Done():
		return func() {}, false
	default:
		return func() {}, false
	}
}

// UpstreamKind classifies a run failure for HTTP mapping. The adapters
// translate the kind into their own error-type labels.
type UpstreamKind int

const (
	KindUpstream        UpstreamKind = iota // generic upstream failure → 502
	KindUnauthenticated                     // upstream says creds bad/expired → 401
	KindRateLimited                         // upstream throttled → 429
	KindTimeout                             // ctx deadline → 504
)

// UpstreamError maps err to (httpStatus, kind, message). Message is the
// user-facing text: UpstreamError.Message for typed frames, err.Error()
// otherwise (it carries the "upstream: " prefix).
func UpstreamError(err error) (status int, kind UpstreamKind, message string) {
	var ue *chat.UpstreamError
	if errors.As(err, &ue) {
		switch ue.Code {
		case "unauthenticated", "token_expired":
			return http.StatusUnauthorized, KindUnauthenticated, ue.Message
		case "rate_limited", "throttled":
			return http.StatusTooManyRequests, KindRateLimited, ue.Message
		}
		return http.StatusBadGateway, KindUpstream, ue.Message
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, KindTimeout, err.Error()
	}
	return http.StatusBadGateway, KindUpstream, err.Error()
}

// Frames collects upstream frames for the 01-frames debug file when
// debug.log_frames is on. Adapters call OnEvent() inside their own
// OnEvent chain, then Write() on every exit path.
type Frames struct {
	enabled bool
	raw     []json.RawMessage
}

// NewFrames returns a collector; disabled collectors stay nil-cost.
func NewFrames(enabled bool) *Frames {
	if !enabled {
		return &Frames{}
	}
	return &Frames{enabled: true, raw: make([]json.RawMessage, 0, 64)}
}

// OnEvent records ev when it is an upstream frame.
func (f *Frames) OnEvent(ev chat.TurnEvent) {
	if !f.enabled || ev.Kind != "frame" || ev.Frame == nil {
		return
	}
	if raw, err := json.Marshal(ev.Frame); err == nil {
		f.raw = append(f.raw, raw)
	}
}

// Write appends the collected frames to 01-frames (one JSONL line holding
// the array). Safe to call on every path — disabled/empty writes nothing.
func (f *Frames) Write(log *debuglog.Logger, dir string) {
	if !f.enabled || log == nil || len(f.raw) == 0 {
		return
	}
	log.Write(dir, "01-frames", f.raw)
}
