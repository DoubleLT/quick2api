// Package session models the SNCS session document that a Quick outpost
// hands out at GET /auth/sncs-session. The JSON shape is mirrored from the
// outpost response (verified against quickwork-agent 1.2.x); unknown keys
// are ignored so new fields never break decoding.
package session

// Session is the decoded /auth/sncs-session response.
//
// Two field groups: the json-tagged block mirrors the wire contract (never
// rename tags — they are the API surface); the json:"-" block is runtime
// state filled locally by FetchSession and absent from file-loaded sessions.
type Session struct {
	// --- wire fields (the outpost's JSON contract) ---
	IDToken           string  `json:"id_token"`
	ActorARN          string  `json:"actor_arn"`
	WSHost            string  `json:"ws_host"`
	WSPath            string  `json:"ws_path"`
	TenantURL         string  `json:"tenant_url"`
	Provider          string  `json:"provider"`
	AWSAccountID      string  `json:"aws_account_id"`
	Region            string  `json:"region"`
	EmbedHost         string  `json:"embed_host"`
	ConsoleAccountURL string  `json:"console_account_url"`
	WSReplayEnabled   bool    `json:"ws_replay_enabled"`
	WSReplayResumeMS  int64   `json:"ws_replay_resume_stale_ms"`
	UsableUntil       float64 `json:"usable_until"`
	AccountName       string  `json:"account_name"`

	// --- runtime fields (filled by FetchSession, not on the wire) ---

	// OutpostID is the registered outpost UUID (/local/list_tools), NOT in
	// the sncs-session JSON. The chat WSS rejects arbitrary outpostIds —
	// it must be this server's own device id. Empty for file-loaded sessions.
	OutpostID string `json:"-"`

	// OutpostURL + OutpostToken identify the spawned/attached outpost that
	// produced this session. Empty for file-loaded sessions. The cloud
	// attachment path uses TenantURL, not this loopback.
	OutpostURL   string `json:"-"`
	OutpostToken string `json:"-"`
}

// MaskedIDToken returns the token abbreviated for logs — enough to tell two
// sessions apart, never enough to authenticate.
func (s *Session) MaskedIDToken() string {
	if len(s.IDToken) <= 12 {
		return "***"
	}
	return s.IDToken[:6] + "..." + s.IDToken[len(s.IDToken)-4:]
}
