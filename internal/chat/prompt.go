// prompt.go decides how the prompt text reaches the upstream: a single
// user_message when it fits, an out-of-band attachment reference when it
// doesn't, or sequential primed chunks as the last-resort fallback.
package chat

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/kilbertert/quick2api/internal/session"
)

// maxSingleText is the largest user_message text the AIDP upstream accepts
// before it closes the turn with an empty {"type":"error"} frame. Verified
// 2026-11-22: 64_000 bytes OK, 80_000 rejected. Keep margin for framing text.
const maxSingleText = 55_000

// sendPrompt sends the rendered prompt. For oversized text it first
// tries the out-of-band attachment path: upload the prompt as a file and
// reference it from a short user_message — one model turn, no per-chunk
// re-inference. If the attachment path fails it falls back to sequential
// user_message turns with context-priming prefixes.
func (c *Client) sendPrompt(ctx context.Context, t *turn, conversationID string, opts TurnOpts, sess *session.Session) (*TurnResult, error) {

	if len(opts.Text) <= maxSingleText {
		if err := sendUserMessage(t, conversationID, opts.Text, opts, nil); err != nil {
			return nil, err
		}
		return t.collect(ctx, &TurnResult{FinishReason: "stop"})
	}

	// Out-of-band attachment: upload the prompt as a text file via the
	// tenant's /integration/quick-work/* attachment APIs, then send a
	// short user_message that references the returned server path. The
	// model reads the file server-side; the text stays under the frame
	// limit. File-loaded sessions have no tenant binding → attachments
	// unavailable → straight to chunk fallback.
	if sess != nil && sess.TenantURL != "" && sess.IDToken != "" {
		path, attID, err := uploadAttachment(ctx, sess, conversationID, "prompt.txt", []byte(opts.Text))
		slog.Debug("sendPrompt upload attempt", "path", path, "attID", attID, "err", err)
		if err == nil && path != "" {
			attachments := []map[string]any{{
				"type":         "file",
				"name":         "prompt.txt",
				"size":         len(opts.Text),
				"mimeType":     "text/plain",
				"path":         path,
				"attachmentId": attID,
			}}
			short := "The full prompt is in the attached file prompt.txt. Read it and respond to it."
			if err := sendUserMessage(t, conversationID, short, opts, attachments); err != nil {
				return nil, err
			}
			res, err := t.collect(ctx, &TurnResult{FinishReason: "stop"})
			if err == nil && res != nil && res.Text != "" {
				slog.Debug("attachment path succeeded", "path", path)
				return res, nil
			}
			slog.Warn("attachment path returned empty/failed; falling back to chunks", "err", err)
		} else if err != nil {
			slog.Warn("attachment upload failed; falling back to chunks", "error", err)
		}
	}

	// Fallback: sequential user_message turns with context-priming prefixes.
	chunks := splitText(opts.Text, maxSingleText)
	var last *TurnResult
	for i, ch := range chunks {
		text := ch
		if i < len(chunks)-1 {
			text = fmt.Sprintf("[CONTEXT PART %d/%d — do not answer yet, just store this context]\n%s", i+1, len(chunks), ch)
		} else {
			text = fmt.Sprintf("[CONTEXT PART %d/%d — final part. Now answer based on ALL parts above.]\n%s", i+1, len(chunks), ch)
		}
		if err := sendUserMessage(t, conversationID, text, opts, nil); err != nil {
			return nil, err
		}
		got, err := t.collect(ctx, &TurnResult{FinishReason: "stop"})
		if err != nil {
			return nil, err
		}
		if i == len(chunks)-1 {
			last = got
		}
	}
	return last, nil
}

// sendUserMessage emits one user_message frame; attachments, when given,
// go on payload.attachments as [{type,name,size,mimeType,path,attachmentId}].
func sendUserMessage(t *turn, conversationID, text string, opts TurnOpts, attachments []map[string]any) error {
	payload := map[string]any{"thread_id": conversationID, "text": text, "message_id": newUUID()}
	fillMsgOpts(payload, opts)
	if len(attachments) > 0 {
		payload["attachments"] = attachments
	}
	if err := t.send(map[string]any{"type": "user_message", "payload": payload}); err != nil {
		return fmt.Errorf("send user_message: %w", err)
	}
	return nil
}

// fillMsgOpts copies the optional per-message knobs onto a payload map.
func fillMsgOpts(p map[string]any, opts TurnOpts) {
	if opts.AgentMode != "" {
		p["agent_mode"] = opts.AgentMode
	}
	if opts.ThinkingEffort != "" {
		p["thinking_effort"] = opts.ThinkingEffort
	}
	if opts.UserTimezone != "" {
		p["user_timezone"] = opts.UserTimezone
	}
	if opts.UserLocale != "" {
		p["user_locale"] = opts.UserLocale
	}
}
