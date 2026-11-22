// Package chat speaks the AIDP "quickwork-chat-bidirectional-stream"
// protocol: one WebSocket per session, AWS event-stream frames inbound,
// UTF-8 rawInputEvent JSON frames outbound.
//
// Verified wire facts (Amazon Quick 1.2.x renderer + live probe):
//
//	URL:  wss://<wsHost>/streaming/quickwork-chat-bidirectional-stream
//	      ?surfaceType=<QUICK_WORK_DESKTOP_WINDOWS>
//	      &token=<id_token>
//	      &outpostId=<any string>
//	      &workingSessionId=<uuid>
//	      &channel=prod
//	Header: Origin: https://quick.aws.com   (required — without it the
//	        handshake is refused before upgrade)
//
//	Outbound frame: UTF-8 JSON
//	  {"eventType":"rawInputEvent","payload":{"payload":"<b64(inner JSON)>"}}
//	  inner JSON = {"type":<frame_type>,"payload":{...}}
//
//	Inbound frame: binary; AWS event-stream message; payload is UTF-8 JSON
//	  {"eventType":<outer type>,"payload":{"payload":"<b64(inner JSON)>"}}
//	  and the inner JSON is {"type":<frame_type>,"payload":{...}}
package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/kilbertert/quick2api/internal/eventstream"
	"github.com/kilbertert/quick2api/internal/session"
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

// wsURL builds the dial URL. token is query-escaped (its `:` and `:`-joined
// segments must survive).
func (c *Client) wsURL(sess *session.Session, outpostID, workingSessionID string) string {
	q := url.Values{}
	q.Set("surfaceType", c.SurfaceType)
	q.Set("token", sess.IDToken)
	q.Set("outpostId", outpostID)
	q.Set("workingSessionId", workingSessionID)
	q.Set("channel", c.Channel)
	return "wss://" + sess.WSHost + "/streaming/quickwork-chat-bidirectional-stream?" + q.Encode()
}

// wrap encodes an inner frame into the rawInputEvent envelope the renderer
// sends.
func wrap(inner map[string]any) ([]byte, error) {
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		return nil, err
	}
	env := map[string]any{
		"eventType": "rawInputEvent",
		"payload":   map[string]string{"payload": base64.StdEncoding.EncodeToString(innerJSON)},
	}
	return json.Marshal(env)
}

// RunTurn performs one complete turn on a fresh WebSocket connection and
// fresh session: connect → session_connect → session_create → user_message
// → collect until message_complete.
//
// One WS per turn is deliberately simple (each upstream session maps to its
// own socket); session reuse across turns is future work.
func (c *Client) RunTurn(ctx context.Context, sess *session.Session, opts TurnOpts) (*TurnResult, error) {
	if opts.Text == "" {
		return nil, errors.New("empty prompt text")
	}
	outpostID := sess.OutpostID
	if outpostID == "" {
		outpostID = fmt.Sprintf("q2a-%d", time.Now().UnixNano()%0xffffff)
		slog.Warn("no registered outpost id; chat will likely close early", "outpostID", outpostID)
	}
	wsid := newUUID()
	sid := newUUID() // session/thread id

	dial := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		ReadBufferSize:   256 * 1024,
		WriteBufferSize:  64 * 1024,
	}
	if c.Proxy == "direct" {
		dial.Proxy = nil
	} else if c.Proxy != "" {
		pu, err := url.Parse(c.Proxy)
		if err != nil {
			return nil, fmt.Errorf("quick.proxy: %w", err)
		}
		dial.Proxy = http.ProxyURL(pu)
	} else {
		dial.Proxy = http.ProxyFromEnvironment
	}
	hdr := http.Header{}
	hdr.Set("Origin", c.Origin)
	ws, wresp, err := dial.DialContext(ctx, c.wsURL(sess, outpostID, wsid), hdr)
	if err != nil {
		// The dialer returns ErrBadHandshake with the HTTP response when the
		// server refuses the upgrade — surface the status so bridge can
		// refresh the session and retry (id_token is short-lived).
		if wresp != nil {
			body, _ := io.ReadAll(io.LimitReader(wresp.Body, 4096))
			wresp.Body.Close()
			slog.Warn("ws handshake refused", "status", wresp.StatusCode, "body", trunc(string(body), 300))
			code := "server"
			if wresp.StatusCode == 401 || wresp.StatusCode == 403 {
				code = "unauthenticated"
			} else if wresp.StatusCode == 429 {
				code = "rate_limited"
			}
			return nil, &UpstreamError{Code: code, Message: fmt.Sprintf("ws handshake %d: %s", wresp.StatusCode, trunc(string(body), 200))}
		}
		return nil, fmt.Errorf("upstream ws dial: %w", err)
	}
	defer ws.Close()

	send := func(inner map[string]any) error {
		b, err := wrap(inner)
		if err != nil {
			return err
		}
		slog.Debug("ws send", "type", inner["type"], "len", len(b))
		// CUe() in the renderer returns a Uint8Array — the server sends
		// "Unexpected WebSocket frame received" on TEXT frames.
		return ws.WriteMessage(websocket.BinaryMessage, b)
	}

	inCh := make(chan inbound, 64)
	readErr := make(chan error, 1)
	go func() {
		defer close(inCh)
		var rawbuf []byte
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			slog.Debug("ws inbound", "mt", mt, "len", len(data))
			if mt == websocket.TextMessage {
				slog.Debug("ws text payload", "data", trunc(string(data), 400))
				if fr := parseEnvelopePayload(data); fr != nil {
					inCh <- inbound{f: fr}
				}
				continue
			}
			rawbuf = append(rawbuf, data...)
			msgs, used, err := eventstream.Drain(rawbuf)
			if err != nil {
				slog.Warn("eventstream drain error", "error", err)
			}
			rawbuf = rawbuf[used:]
			for i := range msgs {
				slog.Debug("es msg", "headers", len(msgs[i].Headers), "payload", trunc(string(msgs[i].Payload), 400))
				fr := parseEnvelopePayload(msgs[i].Payload)
				if fr != nil {
					inCh <- inbound{f: fr}
				}
			}
		}
	}()

	emit := func(ev TurnEvent) {
		if opts.OnEvent != nil {
			opts.OnEvent(ev)
		}
	}

	// waitFor consumes frames until want() returns true or the timeout fires.
	waitFor := func(d time.Duration, want func(*Frame) bool) error {
		t := time.NewTimer(d)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return fmt.Errorf("handshake timeout after %s", d)
			case err := <-readErr:
				return fmt.Errorf("ws closed during handshake: %w", err)
			case in, ok := <-inCh:
				if !ok {
					return errors.New("ws closed during handshake")
				}
				if in.err != nil {
					return in.err
				}
				emit(TurnEvent{Kind: "frame", Frame: in.f})
				if in.f.Type == "error" {
					return fmt.Errorf("upstream error frame: %s", in.f.Payload)
				}
				if want(in.f) {
					return nil
				}
			}
		}
	}

	// 1. session_connect — renderer payload is
	// {actor_id, ...tf(), ...nf()}: tf() adds surfaceType+outpostId+
	// appVersion+clientInstanceId; nf() adds the capability flags the server
	// needs to consider the client functional. Missing flags made the server
	// close the stream right after session_established in testing.
	if err := send(map[string]any{
		"type": "session_connect",
		"payload": map[string]any{
			"actor_id":                    sess.ActorARN,
			"surfaceType":                 c.SurfaceType,
			"outpostId":                   outpostID,
			"appVersion":                  orElse(c.AppVersion, "1.2.4660"),
			"clientInstanceId":            newUUID(),
			"acp_first_text":              1,
			"user_notification":           1,
			"subtask_progress":            1,
			"external_agent_sign_in":      1,
			"recents_promotion":           1,
			"self_registering_connectors": 1,
		},
	}); err != nil {
		return nil, fmt.Errorf("send session_connect: %w", err)
	}
	// federate_status — renderer sends it right after session_connect.
	if err := send(map[string]any{
		"type":    "federate_status",
		"payload": map[string]any{},
	}); err != nil {
		return nil, fmt.Errorf("send federate_status: %w", err)
	}
	time.Sleep(400 * time.Millisecond)
	if err := waitFor(c.HandshakeTimeout, func(f *Frame) bool {
		return f.Type == "session_established" || f.Type == "session_connected"
	}); err != nil {
		if c.StrictHandshake {
			return nil, err
		}
		slog.Warn("session_connect ack not received; proceeding", "error", err)
	}

	// 2. session_create
	time.Sleep(400 * time.Millisecond)
	if err := send(map[string]any{
		"type": "session_create",
		"payload": map[string]any{
			"session_id": sid,
			"title":      orElse(opts.Title, "quick2api"),
		},
	}); err != nil {
		return nil, fmt.Errorf("send session_create: %w", err)
	}
	if err := waitFor(c.HandshakeTimeout, func(f *Frame) bool {
		return f.Type == "session_created"
	}); err != nil {
		if c.StrictHandshake {
			return nil, err
		}
		slog.Warn("session_create ack not received; proceeding", "error", err)
	}
	time.Sleep(400 * time.Millisecond)

	// 3+4. user_message: send the prompt. Texts over the upstream
	// per-message limit are uploaded out-of-band as an attachment and the
	// message references it — one model turn instead of N. Falls back to
	// sequential user_message turns if the upload path fails.
	res, err := c.sendUserPrompt(ctx, send, collectTurn, readErr, inCh, emit, sid, opts, sess)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// maxSingleText is the largest user_message text the AIDP upstream accepts
// before it closes the turn with an empty {"type":"error"} frame. Verified
// 2026-11-22: 64_000 bytes OK, 80_000 rejected. Keep margin for framing text.
const maxSingleText = 55_000

// sendUserPrompt sends the rendered prompt. For oversized text it first
// tries the out-of-band attachment path: upload the prompt as a file and
// reference it from a short user_message — one model turn, no per-chunk
// re-inference. If the attachment path fails it falls back to sequential
// user_message turns with context-priming prefixes.
func (c *Client) sendUserPrompt(ctx context.Context,
	send func(map[string]any) error,
	collect func(context.Context, <-chan error, <-chan inbound, func(TurnEvent), *TurnResult) (*TurnResult, error),
	readErr <-chan error, inCh <-chan inbound,
	emit func(TurnEvent), sid string, opts TurnOpts, sess *session.Session) (*TurnResult, error) {

	if len(opts.Text) <= maxSingleText {
		if err := sendUserMessage(send, sid, opts.Text, opts, nil); err != nil {
			return nil, err
		}
		return collect(ctx, readErr, inCh, emit, &TurnResult{FinishReason: "stop"})
	}

	// Out-of-band attachment: upload the prompt as a text file via the
	// tenant's /integration/quick-work/* attachment APIs, then send a
	// short user_message that references the returned server path. The
	// model reads the file server-side; the text stays under the frame
	// limit. File-loaded sessions have no tenant binding → attachments
	// unavailable → straight to chunk fallback.
	if sess != nil && sess.TenantURL != "" && sess.IDToken != "" {
		path, attID, err := c.uploadAttachment(ctx, sess, sid, "prompt.txt", []byte(opts.Text))
		slog.Debug("sendUserPrompt upload attempt", "path", path, "attID", attID, "err", err)
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
			if err := sendUserMessage(send, sid, short, opts, attachments); err != nil {
				return nil, err
			}
			res, err := collect(ctx, readErr, inCh, emit, &TurnResult{FinishReason: "stop"})
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
		if err := sendUserMessage(send, sid, text, opts, nil); err != nil {
			return nil, err
		}
		got, err := collect(ctx, readErr, inCh, emit, &TurnResult{FinishReason: "stop"})
		if err != nil {
			return nil, err
		}
		if i == len(chunks)-1 {
			last = got
		}
	}
	return last, nil
}

// uploadAttachment delivers data as a Quick cloud attachment and
// returns (serverPath, attachmentId). Three steps, all on the tenant_url the
// outpost's sncs-session hands out — same IPCs the renderer's nZ()
// uses (nook-upload-attachment / nook-describe-attachment-dlp / deliver via
// invokeAgent):
//
//	POST {tenant}/integration/quick-work/upload-attachment?name=...
//	      &contentType=...&contentLength=...  → {attachmentId}
//	GET  {tenant}/integration/quick-work/attachment-dlp-status
//	      ?attachmentId=...                     → {dlpJobStatus,policyAction}
//	POST {tenant}/integration/quick-work/qbs-event
//	      invokeAgent{path:"files/upload/<sid>",_method:POST,
//	                  body:{attachmentId}}     → {path|files[0].path}
//
// The returned serverPath goes onto user_message payload.attachments[].path;
// the model reads the file server-side in one turn.
func (c *Client) uploadAttachment(ctx context.Context, sess *session.Session, convID, name string, data []byte) (string, string, error) {
	if sess == nil || sess.IDToken == "" || sess.TenantURL == "" {
		return "", "", fmt.Errorf("upload: no session bound")
	}
	auth := "Bearer " + sess.IDToken
	httpc := &http.Client{Timeout: 30 * time.Second}

	// 1. upload-attachment: raw bytes, octet-stream, id_token bearer.
	up, err := http.NewRequestWithContext(ctx, "POST",
		sess.TenantURL+"/integration/quick-work/upload-attachment"+
			"?name="+url.QueryEscape(name)+
			"&contentType="+url.QueryEscape("text/plain")+
			"&contentLength="+fmt.Sprint(len(data)),
		bytes.NewReader(data))
	if err != nil {
		return "", "", err
	}
	up.Header.Set("Authorization", auth)
	up.Header.Set("Content-Type", "application/octet-stream")
	up.Header.Set("Origin", "https://quick.aws.com")
	upResp, err := httpc.Do(up)
	if err != nil {
		return "", "", fmt.Errorf("upload: %w", err)
	}
	upBody, _ := io.ReadAll(io.LimitReader(upResp.Body, 1<<20))
	upResp.Body.Close()
	slog.Debug("upload-attachment", "status", upResp.StatusCode, "body", trunc(string(upBody), 400))
	if upResp.StatusCode >= 400 {
		return "", "", fmt.Errorf("upload-attachment %d: %s", upResp.StatusCode, trunc(string(upBody), 200))
	}
	var upOut struct {
		AttachmentID string `json:"attachmentId"`
		Path         string `json:"path"`
	}
	if err := json.Unmarshal(upBody, &upOut); err != nil {
		return "", "", fmt.Errorf("upload-attachment decode: %w", err)
	}
	if upOut.AttachmentID == "" {
		if upOut.Path != "" {
			return upOut.Path, "", nil
		}
		return "", "", fmt.Errorf("upload-attachment returned no attachmentId: %s", trunc(string(upBody), 200))
	}
	attID := upOut.AttachmentID

	// 2. DLP poll — wait for policyAction=ALLOW (or a skip status).
	for i := 0; i < 20; i++ {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
		d, err := http.NewRequestWithContext(ctx, "GET",
			sess.TenantURL+"/integration/quick-work/attachment-dlp-status?attachmentId="+url.QueryEscape(attID), nil)
		if err != nil {
			return "", "", err
		}
		d.Header.Set("Authorization", auth)
		d.Header.Set("Origin", "https://quick.aws.com")
		dResp, err := httpc.Do(d)
		if err != nil {
			slog.Warn("dlp-status poll failed", "error", err)
			continue
		}
		dBody, _ := io.ReadAll(io.LimitReader(dResp.Body, 1<<20))
		dResp.Body.Close()
		slog.Debug("dlp-status", "attempt", i+1, "status", dResp.StatusCode, "body", trunc(string(dBody), 400))
		var dOut struct {
			DLPJobStatus string `json:"dlpJobStatus"`
			PolicyAction string `json:"policyAction"`
		}
		if err := json.Unmarshal(dBody, &dOut); err != nil {
			return "", "", fmt.Errorf("dlp-status decode: %w", err)
		}
		switch strings.ToUpper(dOut.PolicyAction) {
		case "ALLOW":
			goto deliver
		case "BLOCK", "DENY", "REJECT":
			return "", "", fmt.Errorf("dlp blocked: %s", trunc(string(dBody), 200))
		}
		switch strings.ToUpper(dOut.DLPJobStatus) {
		case "SKIPPED", "NOT_APPLICABLE", "DISABLED":
			goto deliver
		}
	}
	return "", "", fmt.Errorf("dlp-status did not settle in time")

deliver:
	// 3. Deliver: bind attachmentId to the conversation, returns serverPath.
	// Renderer lZn() → ll("/files/upload/<sid>", {attachmentId}) → flat
	// qbs-event invokeAgent.
	delBody := map[string]any{
		"eventType": "invokeAgent",
		"eventBody": map[string]any{"invokeAgent": map[string]any{
			"path":        "files/upload/" + convID,
			"queryParams": map[string]any{"_method": "POST"},
			"body":        `{"attachmentId":"` + attID + `"}`,
		}},
	}
	raw, _ := json.Marshal(delBody)
	delReq, err := http.NewRequestWithContext(ctx, "POST",
		sess.TenantURL+"/integration/quick-work/qbs-event", bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	delReq.Header.Set("Authorization", auth)
	delReq.Header.Set("Content-Type", "application/json")
	delReq.Header.Set("Origin", "https://quick.aws.com")
	delResp, err := httpc.Do(delReq)
	if err != nil {
		return "", "", fmt.Errorf("deliver: %w", err)
	}
	delBodyRaw, _ := io.ReadAll(io.LimitReader(delResp.Body, 2<<20))
	delResp.Body.Close()
	slog.Debug("deliver", "status", delResp.StatusCode, "body", trunc(string(delBodyRaw), 500))
	if delResp.StatusCode >= 400 {
		return "", "", fmt.Errorf("deliver %d: %s", delResp.StatusCode, trunc(string(delBodyRaw), 200))
	}
	// lZn response: sZn(o)=o.path||o.files[0].path, wrapped inside
	// eventResponse.invokeAgentResponse → {type:api_response, body:"<json>"}.
	var ev struct {
		EventResponse json.RawMessage `json:"eventResponse"`
	}
	if err := json.Unmarshal(delBodyRaw, &ev); err != nil || len(ev.EventResponse) == 0 {
		return "", "", fmt.Errorf("deliver decode: %w body=%s", err, trunc(string(delBodyRaw), 300))
	}
	var inner struct {
		InvokeAgentResponse string `json:"invokeAgentResponse"`
	}
	if err := json.Unmarshal(ev.EventResponse, &inner); err != nil || inner.InvokeAgentResponse == "" {
		var direct struct {
			Path  string `json:"path"`
			Files []struct {
				Path string `json:"path"`
			} `json:"files"`
		}
		if err2 := json.Unmarshal(ev.EventResponse, &direct); err2 == nil {
			if direct.Path != "" {
				return direct.Path, attID, nil
			}
			if len(direct.Files) > 0 && direct.Files[0].Path != "" {
				return direct.Files[0].Path, attID, nil
			}
		}
		return "", "", fmt.Errorf("deliver inner decode: %s", trunc(string(ev.EventResponse), 300))
	}
	var resp struct {
		Type string `json:"type"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(inner.InvokeAgentResponse), &resp); err != nil {
		return "", "", fmt.Errorf("invokeAgentResponse decode: %w", err)
	}
	var fin struct {
		Path  string `json:"path"`
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &fin); err != nil {
		return "", "", fmt.Errorf("deliver body decode: %w", err)
	}
	if fin.Path != "" {
		return fin.Path, attID, nil
	}
	if len(fin.Files) > 0 && fin.Files[0].Path != "" {
		return fin.Files[0].Path, attID, nil
	}
	return "", "", fmt.Errorf("deliver returned no path: %s", trunc(resp.Body, 300))
}

// sendUserMessage emits one user_message frame; attachments, when given,
// go on payload.attachments as [{type,name,size,mimeType,path,attachmentId}].
func sendUserMessage(send func(map[string]any) error, sid, text string, opts TurnOpts, attachments []map[string]any) error {
	payload := map[string]any{"thread_id": sid, "text": text, "message_id": newUUID()}
	fillMsgOpts(payload, opts)
	if len(attachments) > 0 {
		payload["attachments"] = attachments
	}
	if err := send(map[string]any{"type": "user_message", "payload": payload}); err != nil {
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

// collectTurn reads inbound frames until message_complete, session end, or
// close; extracted so multi-chunk turns can reuse it per sub-message.
func collectTurn(ctx context.Context, readErr <-chan error, inCh <-chan inbound, emit func(TurnEvent), res *TurnResult) (*TurnResult, error) {
	var textSeen atomic.Bool
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-readErr:
			if textSeen.Load() {
				res.PartialOnClose = true
				emit(TurnEvent{Kind: "done"})
				return res, nil
			}
			return nil, fmt.Errorf("upstream ws closed mid-turn: %w", err)
		case in, ok := <-inCh:
			if !ok {
				if textSeen.Load() {
					res.PartialOnClose = true
					emit(TurnEvent{Kind: "done"})
					return res, nil
				}
				return nil, errors.New("upstream ws closed before message_complete")
			}
			f := in.f
			emit(TurnEvent{Kind: "frame", Frame: f})
			switch f.Type {
			case "text_delta", "thinking_delta":
				var p struct {
					Delta string `json:"delta"`
				}
				_ = json.Unmarshal(f.Payload, &p)
				if p.Delta != "" {
					if f.Type == "thinking_delta" {
						emit(TurnEvent{Kind: "thinking", Text: p.Delta})
					} else {
						res.Text += p.Delta
						textSeen.Store(true)
						emit(TurnEvent{Kind: "delta", Text: p.Delta})
					}
				}
			case "turn_usage":
				var u TurnUsage
				if err := json.Unmarshal(f.Payload, &u); err == nil {
					if u.CacheReadTokens == 0 {
						u.CacheReadTokens = u.CacheRead
					}
					if u.CacheWriteTokens == 0 {
						u.CacheWriteTokens = u.CacheWrite
					}
					res.Usage = &u
					if u.ResolvedModelID != "" {
						res.ResolvedModel = u.ResolvedModelID
					}
					emit(TurnEvent{Kind: "usage", Usage: &u})
				}
			case "guardrail_blocked":
				res.FinishReason = "content_filter"
				emit(TurnEvent{Kind: "done"})
				return res, nil
			case "error":
				var p struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				}
				_ = json.Unmarshal(f.Payload, &p)
				msg := p.Message
				if msg == "" {
					msg = string(f.Payload)
				}
				return nil, &UpstreamError{Code: p.Code, Message: msg}
			case "message_complete":
				emit(TurnEvent{Kind: "done"})
				return res, nil
			case "session_end", "session_ended":
				if textSeen.Load() {
					res.PartialOnClose = true
					emit(TurnEvent{Kind: "done"})
					return res, nil
				}
				return nil, errors.New("upstream ended session before reply")
			}
		}
	}
}

// UpstreamError is an explicit error frame from the upstream.
type UpstreamError struct {
	Code    string
	Message string
}

func (e *UpstreamError) Error() string {
	if e.Code != "" {
		return "upstream " + e.Code + ": " + e.Message
	}
	return "upstream: " + e.Message
}

// parseEnvelopePayload turns one inbound event-stream payload (UTF-8 JSON)
// into the inner frame. Observed wire shapes:
//
//	{"payload":"<b64(inner)>"}                            (most frames)
//	{"eventType":..., "payload":{"payload":"<b64(inner)>"} (some builds)
//	{"type":<frame_type>, "payload":{...}}                (direct inner)
//
// Returns nil for unparseable payloads.
func parseEnvelopePayload(data []byte) *Frame {
	var outer struct {
		EventType string          `json:"eventType"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &outer); err != nil {
		return nil
	}
	// Direct inner-frame form: {"type": ..., "payload": {...}}
	if outer.Type != "" && outer.EventType == "" {
		var f Frame
		f.Type = outer.Type
		f.Payload = outer.Payload
		return &f
	}
	// Envelope form A: payload is the b64 string itself: {"payload":"<b64>"}
	var b64s string
	if err := json.Unmarshal(outer.Payload, &b64s); err == nil && b64s != "" {
		if f := decodeInner(b64s); f != nil {
			return f
		}
	}
	// Envelope form B: payload is an object holding "payload": "<b64>"
	var pl struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(outer.Payload, &pl); err == nil && pl.Payload != "" {
		return decodeInner(pl.Payload)
	}
	return nil
}

// decodeInner base64-decodes an inner frame JSON into a Frame.
func decodeInner(b64 string) *Frame {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil
	}
	var f Frame
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil
	}
	return &f
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
