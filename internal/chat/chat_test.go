package chat

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func innerB64(t *testing.T, typ, payload string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": typ, "payload": json.RawMessage(payload)})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestParseEnvelopePayload_FormA(t *testing.T) {
	env := `{"payload":"` + innerB64(t, "text_delta", `{"text":"hi"}`) + `"}`
	f := parseEnvelopePayload([]byte(env))
	if f == nil || f.Type != "text_delta" {
		t.Fatalf("form A: %+v", f)
	}
}

func TestParseEnvelopePayload_FormB(t *testing.T) {
	env := `{"eventType":"x","payload":{"payload":"` + innerB64(t, "message_complete", `{}`) + `"}}`
	f := parseEnvelopePayload([]byte(env))
	if f == nil || f.Type != "message_complete" {
		t.Fatalf("form B: %+v", f)
	}
}

func TestParseEnvelopePayload_DirectInner(t *testing.T) {
	env := `{"type":"session_established","payload":{"x":1}}`
	f := parseEnvelopePayload([]byte(env))
	if f == nil || f.Type != "session_established" {
		t.Fatalf("direct: %+v", f)
	}
}

func TestParseEnvelopePayload_Garbage(t *testing.T) {
	if parseEnvelopePayload([]byte("not json")) != nil {
		t.Fatal("garbage parsed")
	}
	if parseEnvelopePayload([]byte(`{"payload":"not b64"}`)) != nil {
		t.Fatal("bad b64 parsed")
	}
}

func TestSplitText_ASCII(t *testing.T) {
	got := splitText("abcdefghij", 3)
	if len(got) != 4 || strings.Join(got, "") != "abcdefghij" {
		t.Fatalf("got %v", got)
	}
}

func TestSplitText_Runes(t *testing.T) {
	// Each "😀" is 4 bytes. n=5 can only hold one rune per chunk, so the
	// four-rune input becomes four single-rune pieces.
	got := splitText("😀😀😀😀", 5)
	if len(got) != 4 {
		t.Fatalf("got %d chunks %v", len(got), got)
	}
	for _, p := range got {
		if p != "😀" {
			t.Fatalf("chunk %q", p)
		}
		if !utf8.ValidString(p) {
			t.Fatalf("invalid utf8 %q", p)
		}
	}
	if strings.Join(got, "") != "😀😀😀😀" {
		t.Fatalf("roundtrip lost data")
	}
}
