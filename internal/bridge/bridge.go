// Package bridge glues the three pieces of the upstream chain into one
// RunTurn call: outpost (spawn or attach) → sncs-session → chat WSS.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/kilbertert/quick2api/internal/chat"
	"github.com/kilbertert/quick2api/internal/config"
	"github.com/kilbertert/quick2api/internal/session"
)

// Bridge owns the upstream session source.
type Bridge struct {
	cfg *config.Config

	mu      sync.Mutex
	outpost *session.Outpost // spawned; nil when attached/file mode
	sess    *session.Session
	sessAt  time.Time
	sessErr error
}

// New prepares the session source. It eagerly verifies the source once so a
// broken install fails at startup, not on the first request.
func New(ctx context.Context, cfg *config.Config) (*Bridge, error) {
	b := &Bridge{cfg: cfg}
	done := ctx.Done()
	switch {
	case cfg.Quick.SessionFile != "":
		s, err := session.LoadFile(cfg.Quick.SessionFile)
		if err != nil {
			return nil, err
		}
		b.sess, b.sessAt = s, time.Now()
		slog.Info("session source: file", "ws_host", s.WSHost, "provider", s.Provider)
	case cfg.Quick.OutpostURL != "":
		o := session.Attach(cfg.Quick.OutpostURL, cfg.Quick.OutpostToken)
		s, err := o.FetchSession()
		if err != nil {
			return nil, fmt.Errorf("attached outpost: %w", err)
		}
		b.outpost, b.sess, b.sessAt = o, s, time.Now()
		slog.Info("session source: attached outpost", "url", o.URL, "ws_host", s.WSHost)
	default:
		o, err := session.Spawn(done, session.SpawnOpts{
			AgentPath:  cfg.Quick.AgentPath,
			Manifest:   cfg.Quick.Manifest,
			BuildStage: cfg.Quick.BuildStage,
			Port:       cfg.Quick.OutpostPort,
			WorkDir:    "",
		})
		if err != nil {
			return nil, err
		}
		s, err := o.FetchSession()
		if err != nil {
			o.Close()
			return nil, err
		}
		b.outpost, b.sess, b.sessAt = o, s, time.Now()
		slog.Info("session source: spawned outpost",
			"url", o.URL, "ws_host", s.WSHost, "provider", s.Provider,
			"account", s.AWSAccountID, "usable_until", time.Unix(int64(s.UsableUntil), 0))
	}
	return b, nil
}

// Close releases the spawned outpost (attached/file modes are no-ops).
func (b *Bridge) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.outpost != nil {
		b.outpost.Close()
		b.outpost = nil
	}
}

// Session returns a session fresh enough to use, refreshing when the cache
// is older than SessionTTL or when force is set (after an auth failure).
func (b *Bridge) Session(force bool) (*session.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sess == nil {
		return nil, errors.New("no session source")
	}
	if !force && time.Since(b.sessAt) < b.cfg.SessionTTL() {
		return b.sess, nil
	}
	// File source can't refresh — reload the file (it may have been rewritten).
	if b.cfg.Quick.SessionFile != "" {
		s, err := session.LoadFile(b.cfg.Quick.SessionFile)
		if err != nil {
			return nil, err
		}
		b.sess, b.sessAt = s, time.Now()
		return s, nil
	}
	if b.outpost == nil {
		return b.sess, nil // attached but no outpost handle — shouldn't happen
	}
	s, err := b.outpost.FetchSession()
	if err != nil {
		b.sessErr = err
		return nil, fmt.Errorf("refresh sncs-session: %w", err)
	}
	slog.Info("sncs-session refreshed", "token", s.MaskedIDToken(), "usable_until", time.Unix(int64(s.UsableUntil), 0))
	b.sess, b.sessAt = s, time.Now()
	return s, nil
}

// ChatClient builds the WebSocket client from config.
func (b *Bridge) ChatClient() *chat.Client {
	return &chat.Client{
		SurfaceType:      b.cfg.Quick.SurfaceType,
		Channel:          b.cfg.Quick.Channel,
		Origin:           b.cfg.Quick.Origin,
		HandshakeTimeout: b.cfg.HandshakeTimeout(),
		StrictHandshake:  b.cfg.Quick.StrictHandshake,
		Proxy:            b.cfg.Quick.Proxy,
	}
}

// RunTurn fetches a fresh-enough session then runs the turn. One retry on a
// mid-handshake auth failure refreshes the session once.
func (b *Bridge) RunTurn(ctx context.Context, opts chat.TurnOpts) (*chat.TurnResult, error) {
	sess, err := b.Session(false)
	if err != nil {
		return nil, err
	}
	res, err := b.ChatClient().RunTurn(ctx, sess, opts)
	if err != nil {
		var ue *chat.UpstreamError
		if errors.As(err, &ue) && (ue.Code == "unauthenticated" || ue.Code == "token_expired") {
			slog.Warn("auth rejected; refreshing session and retrying once")
			if sess2, ferr := b.Session(true); ferr == nil {
				return b.ChatClient().RunTurn(ctx, sess2, opts)
			}
		}
		return nil, err
	}
	return res, nil
}

// Info reports the current session source for /healthz (no secrets).
func (b *Bridge) Info() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]any{
		"session_source": "spawned",
	}
	if b.cfg.Quick.OutpostURL != "" {
		out["session_source"] = "attached"
	}
	if b.cfg.Quick.SessionFile != "" {
		out["session_source"] = "file"
	}
	if b.outpost != nil && b.outpost.URL != "" {
		out["outpost_url"] = b.outpost.URL
	}
	if b.sess != nil {
		out["ws_host"] = b.sess.WSHost
		out["provider"] = b.sess.Provider
		out["account"] = b.sess.AWSAccountID
		out["session_age_s"] = int(time.Since(b.sessAt).Seconds())
	}
	return out
}
