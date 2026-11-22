package eventstream

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// buildMessage assembles a valid event-stream message. Wire layout:
//
//	[0:4]  total_len (whole message incl prelude+trailing CRC)
//	[4:8]  headers_len
//	[8:12] prelude CRC (CRC32-IEEE of bytes 0..8)
//	[12:..] headers, payload, [tail] msg CRC (of everything before it)
func buildMessage(t *testing.T, headers map[string]any, payload []byte) []byte {
	t.Helper()
	var hb bytes.Buffer
	for name, v := range headers {
		hb.WriteByte(byte(len(name)))
		hb.WriteString(name)
		switch val := v.(type) {
		case string:
			hb.WriteByte(7)
			var u16 [2]byte
			binary.BigEndian.PutUint16(u16[:], uint16(len(val)))
			hb.Write(u16[:])
			hb.WriteString(val)
		case int32:
			hb.WriteByte(4)
			var u32 [4]byte
			binary.BigEndian.PutUint32(u32[:], uint32(val))
			hb.Write(u32[:])
		case bool:
			if val {
				hb.WriteByte(0)
			} else {
				hb.WriteByte(1)
			}
		}
	}
	hlen := uint32(hb.Len())
	total := uint32(12 + hlen + uint32(len(payload)) + 4) // 12 prelude + headers + payload + 4 crc

	var msg bytes.Buffer
	var u32 [4]byte
	binary.BigEndian.PutUint32(u32[:], total)
	msg.Write(u32[:]) // bytes 0..4  total
	binary.BigEndian.PutUint32(u32[:], hlen)
	msg.Write(u32[:])      // bytes 4..8  hlen
	prelude := msg.Bytes() // 8 bytes so far
	binary.BigEndian.PutUint32(u32[:], crc32.ChecksumIEEE(prelude))
	msg.Write(u32[:])     // bytes 8..12 prelude CRC
	msg.Write(hb.Bytes()) // headers
	msg.Write(payload)    // payload
	binary.BigEndian.PutUint32(u32[:], crc32.ChecksumIEEE(msg.Bytes()))
	msg.Write(u32[:]) // trailing CRC
	return msg.Bytes()
}

func TestDrainSingleMessage(t *testing.T) {
	want := `{"type":"text_delta","payload":{"text":"hi"}}`
	buf := buildMessage(t, map[string]any{":message-type": "event"}, []byte(want))
	msgs, n, err := Drain(buf)
	if err != nil {
		t.Fatalf("Drain err: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("consumed %d want %d", n, len(buf))
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d msgs", len(msgs))
	}
	if string(msgs[0].Payload) != want {
		t.Fatalf("payload %q", msgs[0].Payload)
	}
	if msgs[0].MessageType() != "event" {
		t.Fatalf("message-type %q", msgs[0].MessageType())
	}
}

func TestDrainConcatenated(t *testing.T) {
	a := buildMessage(t, nil, []byte("one"))
	b := buildMessage(t, nil, []byte("two"))
	c := buildMessage(t, nil, []byte("three"))
	buf := append(append(a, b...), c...)
	msgs, n, err := Drain(buf)
	if err != nil || len(msgs) != 3 || n != len(buf) {
		t.Fatalf("msgs=%d n=%d err=%v", len(msgs), n, err)
	}
}

func TestDrainPartialTail(t *testing.T) {
	full := buildMessage(t, nil, []byte("x"))
	trunc := full[:len(full)-3]
	msgs, n, err := Drain(append(full, trunc...))
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if len(msgs) != 1 || n != len(full) {
		t.Fatalf("msgs=%d n=%d want 1,%d", len(msgs), n, len(full))
	}
}

func TestDrainBadLeadingBytes(t *testing.T) {
	good := buildMessage(t, nil, []byte("ok"))
	buf := append([]byte{0xde, 0xad, 0xbe}, good...)
	msgs, n, err := Drain(buf)
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if len(msgs) != 1 || n != len(buf) {
		t.Fatalf("msgs=%d n=%d", len(msgs), n)
	}
}
