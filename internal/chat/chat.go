// Package chat speaks the AIDP "quickwork-chat-bidirectional-stream"
// protocol: one WebSocket per session, AWS event-stream frames inbound,
// UTF-8 rawInputEvent JSON frames outbound.
//
// Verified wire facts (Amazon Quick 1.2.x renderer + live probe):
//
//	URL:  wss://<wsHost>/streaming/quickwork-chat-bidirectional-stream
//	      ?surfaceType=<QUICK_WORK_DESKTOP_WINDOWS>
//	      &token=<id_token>
//	      &outpostId=<registered outpost device id>  — must be the id the
//	      outpost registered itself under (GET /local/list_tools →
//	      outpost_id); an arbitrary value gets a close-1000 right after
//	      session_established
//	      &workingSessionId=<uuid>
//	      &channel=prod
//	Header: Origin: https://quick.aws.com   (required — without it the
//	        handshake is refused before upgrade)
//
//	Outbound frame: binary WS message carrying UTF-8 JSON
//	  {"eventType":"rawInputEvent","payload":{"payload":"<b64(inner JSON)>"}}
//	  inner JSON = {"type":<frame_type>,"payload":{...}}
//
//	Inbound frame: binary; AWS event-stream message; payload is UTF-8 JSON
//	  {"eventType":<outer type>,"payload":{"payload":"<b64(inner JSON)>"}}
//	  and the inner JSON is {"type":<frame_type>,"payload":{...}}
//
// Files:
//   - types.go   Frame/TurnUsage/TurnResult/TurnOpts/Client/UpstreamError
//   - conn.go    wsURL, envelope wrap/decode, the WS read pump
//   - turn.go    the turn struct, RunTurn handshake + collect loop
//   - prompt.go  prompt sizing: single message / attachment / chunk fallback
//   - upload.go  the three-step cloud attachment path
package chat
