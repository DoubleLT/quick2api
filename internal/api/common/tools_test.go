package common

import (
	"encoding/json"
	"testing"
)

func TestParseToolCallJSON_Bare(t *testing.T) {
	got, ok := ParseToolCallJSON(`{"type":"tool_use","id":"x","name":"Bash","input":{"command":"ls"}}`)
	if !ok || got.Name != "Bash" || got.ID != "x" {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	var in map[string]any
	if err := json.Unmarshal(got.Input, &in); err != nil || in["command"] != "ls" {
		t.Fatalf("input %v", in)
	}
}

func TestParseToolCallJSON_MarkdownFence(t *testing.T) {
	got, ok := ParseToolCallJSON("```json\n{\"type\":\"tool_use\",\"id\":\"x\",\"name\":\"Bash\"}\n```")
	if !ok || got.Name != "Bash" {
		t.Fatalf("fence: %+v ok=%v", got, ok)
	}
}

func TestParseToolCallJSON_PlainText(t *testing.T) {
	_, ok := ParseToolCallJSON("hello world")
	if ok {
		t.Fatal("plain text parsed as tool call")
	}
}

func TestParseToolCallJSON_Array(t *testing.T) {
	got, ok := ParseToolCallJSON(`[{"type":"tool_use","id":"x","name":"Bash"}]`)
	if !ok || got.Name != "Bash" {
		t.Fatalf("array: %+v ok=%v", got, ok)
	}
}

func TestParseToolCallsJSON_Multi(t *testing.T) {
	got, ok := ParseToolCallsJSON(`[
		{"type":"tool_use","id":"a","name":"Bash"},
		{"type":"tool_use","id":"b","name":"Read"}
	]`)
	if !ok || len(got) != 2 || got[0].Name != "Bash" || got[1].Name != "Read" {
		t.Fatalf("multi: %+v ok=%v", got, ok)
	}
}

func TestParseToolCallsJSON_Mixed(t *testing.T) {
	_, ok := ParseToolCallsJSON(`[
		{"type":"tool_use","id":"a","name":"Bash"},
		{"type":"text","text":"oops"}
	]`)
	if ok {
		t.Fatal("mixed array should not parse")
	}
}

func TestToolsInstruction_Empty(t *testing.T) {
	if ToolsInstruction(nil) != "" || ToolsInstruction([]byte("[]")) != "" {
		t.Fatal("empty tools should produce no instruction")
	}
}
