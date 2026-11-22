// Package config loads quick2api's YAML configuration and resolves the
// config/state paths exactly like devin-2api does: -config flag →
// $QUICK2API_CONFIG → ./config.yaml → platform user-config dir.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root of config.yaml.
type Config struct {
	Server ServerConfig `yaml:"server"`
	Debug  DebugConfig  `yaml:"debug"`
	Quick  QuickConfig  `yaml:"quick"`
	API    APIConfig    `yaml:"api"`
	Tools  ToolsConfig  `yaml:"tools"`
}

// ToolsConfig describes the server-side tool executor. When the upstream
// reply contains a tool_use block whose name is enabled here, the proxy
// executes the tool and feeds the result back into the next turn so the
// client sees real effects (the Assistants-API pattern).
type ToolsConfig struct {
	// Bash controls shell-command tools ("Bash", "sh", "shell").
	Bash struct {
		Enabled  bool     `yaml:"enabled"`
		Allow    []string `yaml:"allow"` // glob/prefix patterns; empty = allow all
		Cwd      string   `yaml:"cwd"`   // working dir; empty = files.root
		TimeoutS int      `yaml:"timeout_seconds"`
	} `yaml:"bash"`
	// Files controls Read/Write/Edit/Glob/Grep/LS/TodoWrite. Root confines
	// every path the model can touch.
	Files struct {
		Enabled bool   `yaml:"enabled"`
		Root    string `yaml:"root"`
	} `yaml:"files"`
	// MaxToolRounds caps the tool_call→tool_result loop per request.
	MaxToolRounds int `yaml:"max_tool_rounds"`
}

// ServerConfig controls the HTTP listener and request ceiling.
type ServerConfig struct {
	// Listen is the bind address, e.g. "127.0.0.1:8377".
	Listen string `yaml:"listen"`
	// MaxConcurrency bounds simultaneously-running /v1 completions. Excess
	// requests fast-fail 429 instead of queueing on a shared upstream quota.
	MaxConcurrency int `yaml:"max_concurrency"`
	// TurnTimeoutSeconds caps one upstream turn (ws open → message_complete).
	TurnTimeoutSeconds int `yaml:"turn_timeout_seconds"`
}

// DebugConfig controls request-level debug logging under the state dir.
type DebugConfig struct {
	// Enabled writes one directory per request under <state>/debug/.
	Enabled bool `yaml:"enabled"`
	// RetentionDays prunes debug dirs older than this at startup. <=0 keeps all.
	RetentionDays int `yaml:"retention_days"`
	// LogFrames also records every inbound upstream frame (frames-<id>.jsonl).
	LogFrames bool `yaml:"log_frames"`
}

// QuickConfig describes how to reach the upstream Quick session.
type QuickConfig struct {
	// AgentPath points at quickwork-agent.exe (or the agent binary). Empty
	// means auto-detect the standard per-user install.
	AgentPath string `yaml:"agent_path"`
	// Manifest is the -manifest argument the agent expects ("desktop").
	Manifest string `yaml:"manifest"`
	// BuildStage maps to QUICKWORK_BUILD_STAGE; the agent exits non-zero
	// without it ("prod" for release installs).
	BuildStage string `yaml:"build_stage"`
	// OutpostPort is the loopback port for the spawned outpost instance.
	// We never reuse 8780: that instance belongs to the running Quick app and
	// its bearer token lives only in the app's main process.
	OutpostPort int `yaml:"outpost_port"`
	// OutpostURL + OutpostToken bypass spawning entirely: point at an already
	// running outpost whose /auth/sncs-session bearer you know. Both or neither.
	OutpostURL   string `yaml:"outpost_url"`
	OutpostToken string `yaml:"outpost_token"`
	// SessionFile loads a previously captured sncs-session JSON instead of
	// starting an outpost (CI/testing escape hatch).
	SessionFile string `yaml:"session_file"`
	// WSURL params for the AIDP bidirectional stream.
	SurfaceType string `yaml:"surface_type"` // e.g. QUICK_WORK_DESKTOP_WINDOWS
	Channel     string `yaml:"channel"`      // "prod"
	Origin      string `yaml:"origin"`       // e.g. https://quick.aws.com
	// Turn shaping.
	UserTimezone      string `yaml:"user_timezone"`
	UserLocale        string `yaml:"user_locale"`
	DefaultAgentMode  string `yaml:"default_agent_mode"`
	ThinkingEffort    string `yaml:"thinking_effort"`
	StrictHandshake   bool   `yaml:"strict_handshake"`
	HandshakeTimeoutS int    `yaml:"handshake_timeout_seconds"`
	// AgentModes maps client-facing model names to upstream agent_mode values.
	// Entries also become the advertised /v1/models list.
	AgentModes map[string]string `yaml:"agent_modes"`
	// Models names the extra IDs to advertise in /v1/models on top of
	// agent_modes keys (purely cosmetic aliases).
	Models []string `yaml:"models"`
	// SessionTTLSeconds caps how long a fetched sncs-session is reused before
	// a fresh one is pulled from the outpost.
	SessionTTLSeconds int `yaml:"session_ttl_seconds"`
	// Proxy optionally overrides the HTTP(S) proxy used for the outbound WSS
	// dial (falls back to HTTPS_PROXY/HTTP_PROXY env). "direct" disables proxy.
	Proxy string `yaml:"proxy"`
	// Accounts, when non-empty, turns the bridge into a lane pool: each
	// entry is an independent session source (outpost_url+token, or
	// session_file; spawn falls back to the top-level fields). Lanes
	// rotate round-robin; a lane that answers 401/403/5xx cools down for
	// cooldown_seconds before serving again. One account = the current
	// single-lane behavior.
	Accounts           []AccountConfig `yaml:"accounts"`
	CooldownSecs       int             `yaml:"cooldown_seconds"`
	SessionAffinityTTL int             `yaml:"session_affinity_ttl_seconds"`
}

// AccountConfig is one lane of the upstream pool. Exactly one of the
// three sources must be set:
//   - outpost_url + outpost_token   → attach to a running outpost
//   - session_file                  → load a captured sncs-session JSON
//   - (empty fields)                → spawn quickwork-agent locally with
//                                    the top-level AgentPath/Manifest/etc.
type AccountConfig struct {
	Name         string `yaml:"name"` // shown in /healthz lanes[] and logs
	OutpostURL   string `yaml:"outpost_url"`
	OutpostToken string `yaml:"outpost_token"`
	SessionFile  string `yaml:"session_file"`
	// Cooldown overrides the pool-wide default for this lane.
	CooldownS int `yaml:"cooldown_seconds"`
}

// APIConfig controls the downstream-facing surface.
type APIConfig struct {
	// APIKey is the required bearer for /v1/* endpoints. Empty = unauthenticated
	// (only sensible when Listen is loopback).
	APIKey string `yaml:"api_key"`
}

// Defaults returns a Config with the same values config.example.yaml ships.
func Defaults() *Config {
	return &Config{
		Server: ServerConfig{
			Listen:             "127.0.0.1:8377",
			MaxConcurrency:     16,
			TurnTimeoutSeconds: 600,
		},
		Debug: DebugConfig{
			Enabled:       false,
			RetentionDays: 14,
			LogFrames:     false,
		},
		Quick: QuickConfig{
			AgentPath:         "",
			Manifest:          "desktop",
			BuildStage:        "prod",
			// 0 = auto-pick a free loopback port. Hard-coding one would collide
			// with whatever an earlier crashed run left listening.
			OutpostPort:       0,
			SurfaceType:       "QUICK_WORK_DESKTOP_WINDOWS",
			Channel:           "prod",
			Origin:            "https://quick.aws.com",
			UserTimezone:      "Asia/Shanghai",
			UserLocale:        "zh-CN",
			DefaultAgentMode:  "smart",
			StrictHandshake:   false,
			HandshakeTimeoutS: 10,
			AgentModes: map[string]string{
				"quick": "smart",
			},
			Models:            nil,
			SessionTTLSeconds: 600,
		},
	}
}

// Load reads path over Defaults(). Unknown keys are rejected so typos fail
// loudly instead of silently keeping defaults.
func Load(path string) (*Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate sanity-checks the merged config.
func (c *Config) Validate() error {
	if c.Server.Listen == "" {
		return errors.New("server.listen is empty")
	}
	if c.Server.MaxConcurrency < 0 {
		return errors.New("server.max_concurrency must be >= 0")
	}
	hasURL := c.Quick.OutpostURL != ""
	hasTok := c.Quick.OutpostToken != ""
	if hasURL != hasTok {
		return errors.New("quick.outpost_url and quick.outpost_token must be set together or both empty")
	}
	if hasURL && c.Quick.SessionFile != "" {
		return errors.New("set only one of quick.outpost_url / quick.session_file")
	}
	for k, v := range c.Quick.AgentModes {
		if k == "" {
			return errors.New("quick.agent_modes has an empty key")
		}
		if v == "" {
			return fmt.Errorf("quick.agent_modes[%q] is empty", k)
		}
	}
	return nil
}

// AgentModeFor maps a client model name to an upstream agent_mode.
func (c *Config) AgentModeFor(model string) string {
	if v, ok := c.Quick.AgentModes[model]; ok {
		return v
	}
	return c.Quick.DefaultAgentMode
}

// TurnTimeout returns the configured ceiling as a Duration.
func (c *Config) TurnTimeout() time.Duration {
	if c.Server.TurnTimeoutSeconds <= 0 {
		return 10 * time.Minute
	}
	return time.Duration(c.Server.TurnTimeoutSeconds) * time.Second
}

// HandshakeTimeout bounds the session_connect/session_create ack wait.
func (c *Config) HandshakeTimeout() time.Duration {
	if c.Quick.HandshakeTimeoutS <= 0 {
		return 10 * time.Second
	}
	return time.Duration(c.Quick.HandshakeTimeoutS) * time.Second
}

// SessionTTL bounds reuse of a fetched sncs-session.
func (c *Config) SessionTTL() time.Duration {
	if c.Quick.SessionTTLSeconds <= 0 {
		return 10 * time.Minute
	}
	return time.Duration(c.Quick.SessionTTLSeconds) * time.Second
}

// ResolveConfigPath implements the documented search order.
func ResolveConfigPath(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if env := os.Getenv("QUICK2API_CONFIG"); env != "" {
		return env, nil
	}
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml", nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", errors.New("no config found: pass -config or set QUICK2API_CONFIG")
	}
	return filepath.Join(dir, "quick2api", "config.yaml"), nil
}

// ResolveStateDir returns the root for debug logs / runtime state:
// flag → $QUICK2API_STATE_DIR → platform default.
func ResolveStateDir(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if env := os.Getenv("QUICK2API_STATE_DIR"); env != "" {
		return env, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(base, "quick2api"), nil
	}
	// XDG_STATE_HOME-equivalent for logs on unix.
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "quick2api"), nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "quick2api"), nil
	}
	return filepath.Join(base, "quick2api"), nil
}
