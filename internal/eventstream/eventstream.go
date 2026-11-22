// Package eventstream decodes AWS event-stream (vnd.amazon.eventstream)
// frames — the binary envelope the AIDP chat-bidirectional-stream WebSocket
// uses for inbound messages. Outbound frames on that socket are plain UTF-8
// JSON, so only a decoder lives here.
//
// Wire layout per message:
//
//	[0:4]   uint32 BE  total_len (includes these 16 prelude bytes + trailing CRC)
//	[4:8]   uint32 BE  headers_len
//	[8:12]  uint32 BE  prelude CRC (CRC32-IEEE of bytes 0..8)
//	[12:..] headers    (name_len u8, name bytes, value_type u8, value)
//	[..]    payload    (total_len - 16 - headers_len - 4 bytes)
//	[tail]  uint32 BE  message CRC (CRC32-IEEE of everything before it)
package eventstream

import (
	"encoding/binary"
	"fmt"
)

// MaxMessageLen bounds a single event-stream message. 16 MiB matches the
// reference implementation's sanity cap.
const MaxMessageLen = 16 << 20

// Header carries one decoded event-stream header value.
type Header struct {
	Type  byte
	Bool  bool
	Int8  int8
	Int16 int16
	Int32 int32
	Int64 int64
	Bytes []byte
	Str   string
	UUID  [16]byte
	Time  int64 // unix millis (type 8)
}

// Message is one decoded event-stream message.
type Message struct {
	Headers map[string]Header
	Payload []byte
}

// MessageType is the conventional :message-type header string ("", if absent).
func (m *Message) MessageType() string {
	if h, ok := m.Headers[":message-type"]; ok && h.Type == 7 {
		return h.Str
	}
	return ""
}

// Drain consumes buf and returns every complete message plus the number of
// bytes consumed. An incomplete trailing message is left for the caller to
// keep buffered. Corrupt leading bytes are skipped one at a time (the
// reference JS implementation does the same resync).
func Drain(buf []byte) ([]Message, int, error) {
	var msgs []Message
	pos, n := 0, len(buf)
	for pos+16 <= n {
		total := binary.BigEndian.Uint32(buf[pos:])
		if total < 16 || total > MaxMessageLen {
			pos++
			continue
		}
		if pos+int(total) > n {
			break // wait for more bytes
		}
		msg, err := decode(buf[pos : pos+int(total)])
		if err != nil {
			pos++
			continue
		}
		msgs = append(msgs, msg)
		pos += int(total)
	}
	return msgs, pos, nil
}

func decode(buf []byte) (Message, error) {
	var m Message
	hlen := binary.BigEndian.Uint32(buf[4:])
	// CRCs are advisory: the reference JS decoder ignores them entirely and
	// some endpoints compute them differently — don't drop good messages.
	if int(hlen) > len(buf)-16 {
		return m, fmt.Errorf("eventstream: header len %d overruns message", hlen)
	}
	hstart, hend := 12, 12+int(hlen)
	headers := make(map[string]Header)
	j := hstart
	for j < hend {
		nl := int(buf[j])
		j++
		if j+nl > hend {
			return m, fmt.Errorf("eventstream: header name overruns")
		}
		name := string(buf[j : j+nl])
		j += nl
		if j >= hend {
			return m, fmt.Errorf("eventstream: header %q missing type", name)
		}
		vt := buf[j]
		j++
		var h Header
		h.Type = vt
		switch vt {
		case 0: // bool true
			h.Bool = true
		case 1: // bool false
			h.Bool = false
		case 2: // int8
			h.Int8 = int8(buf[j])
			j++
		case 3: // int16
			h.Int16 = int16(binary.BigEndian.Uint16(buf[j:]))
			j += 2
		case 4: // int32
			h.Int32 = int32(binary.BigEndian.Uint32(buf[j:]))
			j += 4
		case 5: // int64
			h.Int64 = int64(binary.BigEndian.Uint64(buf[j:]))
			j += 8
		case 6: // bytes
			bl := int(binary.BigEndian.Uint16(buf[j:]))
			j += 2
			if j+bl > hend {
				return m, fmt.Errorf("eventstream: header %q bytes overrun", name)
			}
			h.Bytes = buf[j : j+bl]
			j += bl
		case 7: // string
			vl := int(binary.BigEndian.Uint16(buf[j:]))
			j += 2
			if j+vl > hend {
				return m, fmt.Errorf("eventstream: header %q string overrun", name)
			}
			h.Str = string(buf[j : j+vl])
			j += vl
		case 8: // timestamp millis
			h.Time = int64(binary.BigEndian.Uint64(buf[j:]))
			j += 8
		case 9: // uuid
			copy(h.UUID[:], buf[j:j+16])
			j += 16
		default:
			return m, fmt.Errorf("eventstream: unknown header type %d for %q", vt, name)
		}
		headers[name] = h
	}
	m.Headers = headers
	payloadEnd := len(buf) - 4
	if hend > payloadEnd {
		return m, fmt.Errorf("eventstream: payload bounds")
	}
	m.Payload = buf[hend:payloadEnd]
	return m, nil
}
