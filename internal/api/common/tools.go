package common

import (
	"encoding/json"
	"strings"
)

// ToolsInstruction writes the prompt-side contract that tells the model
// how to request a tool call. Both adapters emit this same text so the
// upstream model sees one protocol regardless of which API surface the
// client speaks.
//
// The contract is deliberately tool-agnostic: the proxy parses the
// reply for {type:"tool_use",id,name,input} — the same shape Anthropic
// clients already use — so the executor code is shared.
func ToolsInstruction(toolsRaw []byte) string {
	if len(toolsRaw) == 0 || string(toolsRaw) == "null" || string(toolsRaw) == "[]" {
		return ""
	}
	var b strings.Builder
	b.WriteString("The user has declared the following tools available for use. " +
		"When you want to use one, respond with ONLY the JSON object " +
		"{\"type\":\"tool_use\",\"id\":\"<id>\",\"name\":\"<tool name>\"," +
		"\"input\":{...}}. Do not wrap it in markdown.\nTools:\n")
	b.Write(toolsRaw)
	b.WriteString("\n\n")
	return b.String()
}

// ToolCallJSON is the single tool-use shape the prompt contract asks
// for: {"type":"tool_use","id":"…","name":"…","input":{…}}.
type ToolCallJSON struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ParseToolCallJSON detects a whole-response tool_use call. Accepts the
// bare JSON object, a one-element array containing it, or a markdown
// fenced block containing it. Returns ok=false when the text is a
// normal reply (the caller then returns it verbatim).
func ParseToolCallJSON(text string) (ToolCallJSON, bool) {
	t := strings.TrimSpace(text)
	// Unwrap a single markdown fence (```json … ``` or ``` … ```).
	if strings.HasPrefix(t, "```") {
		t = strings.TrimPrefix(t, "```")
		if i := strings.IndexByte(t, '\n'); i >= 0 {
			t = t[i+1:]
		}
		t = strings.TrimSuffix(strings.TrimSpace(t), "```")
		t = strings.TrimSpace(t)
	}
	var one ToolCallJSON
	if err := json.Unmarshal([]byte(t), &one); err == nil && one.Type == "tool_use" && one.Name != "" {
		return one, true
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(t), &arr); err == nil && len(arr) == 1 {
		if err := json.Unmarshal(arr[0], &one); err == nil && one.Type == "tool_use" && one.Name != "" {
			return one, true
		}
	}
	return ToolCallJSON{}, false
}

// ParseToolCallsJSON detects zero-or-more tool_use calls packed in one
// reply: either a single object or an array of objects. Used by the
// multi-call variant of the tool loop.
func ParseToolCallsJSON(text string) ([]ToolCallJSON, bool) {
	if one, ok := ParseToolCallJSON(text); ok {
		return []ToolCallJSON{one}, true
	}
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "```") {
		t = strings.TrimPrefix(t, "```")
		if i := strings.IndexByte(t, '\n'); i >= 0 {
			t = t[i+1:]
		}
		t = strings.TrimSuffix(strings.TrimSpace(t), "```")
		t = strings.TrimSpace(t)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(t), &arr); err != nil || len(arr) == 0 {
		return nil, false
	}
	var out []ToolCallJSON
	for _, raw := range arr {
		var one ToolCallJSON
		if err := json.Unmarshal(raw, &one); err != nil || one.Type != "tool_use" || one.Name == "" {
			return nil, false
		}
		out = append(out, one)
	}
	return out, true
}
