package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kilbertert/quick2api/internal/chat"
)

func TestRefusalDoesNotBecomeSuccessfulToolCall(t *testing.T) {
	toolJSON := `{"type":"tool_use","name":"Bash","input":{"command":"echo test"}}`
	content, stop := responseContent(&chat.TurnResult{Text: toolJSON, FinishReason: "content_filter"})
	if stop != "refusal" || len(content) != 1 || content[0].Type != "text" || content[0].Text != toolJSON {
		t.Fatalf("blocked output became a tool invocation: %s %+v", stop, content)
	}
}

func TestClientToolReplyAfterNativeProgress(t *testing.T) {
	const progress = "先读取附件上下文。"
	const toolJSON = `{"type":"tool_use","id":"toolu_test","name":"Bash","input":{"command":"echo TOOL_BOUNDARY_OK"}}`
	res := &chat.TurnResult{Text: progress + toolJSON, ToolTextOffset: len(progress), FinishReason: "stop"}
	content, stop := responseContent(res)
	if stop != "tool_use" || len(content) != 1 || content[0].Name != "Bash" || string(content[0].Input) != `{"command":"echo TOOL_BOUNDARY_OK"}` {
		t.Fatalf("native progress hid client tool invocation: %s %+v", stop, content)
	}
	// Without a native tool boundary this is arbitrary prose containing
	// JSON, not an instruction to extract and execute the embedded object.
	res.ToolTextOffset = 0
	content, stop = responseContent(res)
	if stop != "end_turn" || content[0].Type != "text" || content[0].Text != res.Text {
		t.Fatalf("prose was promoted to a tool call: %s %+v", stop, content)
	}
	res.ToolTextOffset = len(progress)
	res.FinishReason = "content_filter"
	content, stop = responseContent(res)
	if stop != "refusal" || content[0].Type != "text" {
		t.Fatalf("blocked reply became a tool call: %s %+v", stop, content)
	}
}

func TestNormalReplyRetainsNativeProgress(t *testing.T) {
	res := &chat.TurnResult{Text: "Reading context.\nHere is the summary.", ToolTextOffset: len("Reading context.\n"), FinishReason: "stop"}
	content, stop := responseContent(res)
	if stop != "end_turn" || content[0].Text != res.Text {
		t.Fatalf("normal reply lost text: %s %+v", stop, content)
	}
}

func req(t *testing.T, body string) *MessagesRequest {
	t.Helper()
	var r MessagesRequest
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func TestRenderPrompt_Plain(t *testing.T) {
	r := req(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	text, _, _, atts, err := renderPrompt(r)
	if err != nil || text != "User: hi" || len(atts) != 0 {
		t.Fatalf("text=%q atts=%v err=%v", text, atts, err)
	}
}

func TestRenderPrompt_System(t *testing.T) {
	r := req(t, `{"system":"be terse","messages":[{"role":"user","content":"hi"}]}`)
	text, _, _, _, _ := renderPrompt(r)
	if text != "System: be terse\n\nUser: hi" {
		t.Fatalf("text=%q", text)
	}
}

func TestRenderPrompt_Assistant(t *testing.T) {
	r := req(t, `{"messages":[{"role":"assistant","content":"yes"}]}`)
	text, _, _, _, _ := renderPrompt(r)
	if text != "Assistant: yes" {
		t.Fatalf("text=%q", text)
	}
}

func TestRenderPrompt_TrailingEnvironment(t *testing.T) {
	for _, env := range []string{`"environment"`, `[{"type":"text","text":"environment"}]`} {
		r := req(t, `{"system":"preamble","messages":[{"role":"user","content":"old"},{"role":"assistant","content":"ok"},{"role":"user","content":[{"type":"text","text":"run ssh"}]},{"role":"system","content":`+env+`}]}`)
		text, tailAt, finalAt, _, err := renderPrompt(r)
		if err != nil {
			t.Fatal(err)
		}
		if got := text[finalAt:]; got != "User: run ssh\n\nSystem: environment" {
			t.Fatalf("operative request displaced: %q", got)
		}
		if !strings.HasPrefix(text[tailAt:], "User: old\n\nAssistant: ok\n\n") {
			t.Fatalf("history offset lost: %q", text[tailAt:])
		}
	}
}

func TestRenderPrompt_ToolResultWithTrailingEnvironment(t *testing.T) {
	r := req(t, `{"messages":[{"role":"user","content":"run ssh"},{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"Bash","input":{"command":"ssh host hostname"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"host"}]},{"role":"system","content":"environment"}]}`)
	text, _, finalAt, _, err := renderPrompt(r)
	if err != nil || text[finalAt:] != "ToolResult: host\n\nSystem: environment" {
		t.Fatalf("final=%q err=%v", text[finalAt:], err)
	}
}

func TestWriteSSE_EventType(t *testing.T) {
	for _, event := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", "error"} {
		var wire strings.Builder
		if err := writeSSE(&wire, event, map[string]any{}); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(wire.String(), "\n")
		var payload struct {
			Type string `json:"type"`
		}
		if lines[0] != "event: "+event || json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &payload) != nil || payload.Type != event {
			t.Fatalf("SDK cannot dispatch event: %q", wire.String())
		}
	}
}

func TestClientToolCount(t *testing.T) {
	if got := clientToolCount(json.RawMessage(`[{"name":"A"},{"name":"B"}]`)); got != 2 {
		t.Fatalf("count=%d", got)
	}
	if got := clientToolCount(json.RawMessage(`null`)); got != 0 {
		t.Fatalf("null count=%d", got)
	}
}

func TestRenderPrompt_ImageBase64(t *testing.T) {
	png := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8/5+hHgAHggJ/PchI7wAAAABJRU5ErkJggg=="
	body := `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png + `"}}]}]}`
	r := req(t, body)
	_, _, _, atts, err := renderPrompt(r)
	if err != nil || len(atts) != 1 {
		t.Fatalf("atts=%v err=%v", atts, err)
	}
	if atts[0].MimeType != "image/png" || len(atts[0].Data) == 0 {
		t.Fatalf("att %+v", atts[0])
	}
}

func TestRenderPrompt_ToolUse(t *testing.T) {
	r := req(t, `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"Bash","input":{"c":"ls"}}]}]}`)
	text, _, _, _, _ := renderPrompt(r)
	if text != `ToolUse Bash({"c":"ls"})` {
		t.Fatalf("text=%q", text)
	}
}

func TestConversationIDOf_Header(t *testing.T) {
	// header lookup is easiest via httptest — skip, exercised by e2e.
}
